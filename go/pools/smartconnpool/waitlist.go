/*
Copyright 2023 The Vitess Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package smartconnpool

import (
	"context"
	"sync"

	"vitess.io/vitess/go/list"
)

// waiter represents a client waiting for a connection in the waitlist
type waiter[C Connection] struct {
	// setting is the connection Setting that we'd like, or nil if we'd like a
	// a connection with no Setting applied
	setting *Setting
	// conn will be set by another client to hand over the connection to use
	conn *Pooled[C]
	// ctx is the context of the waiting client to check for expiration
	ctx context.Context
	// sema is a synchronization primitive that allows us to block until our request
	// has been fulfilled
	sema semaphore
	// age is the amount of cycles this client has been on the waitlist
	age uint32
}

type waitlist[C Connection] struct {
	nodes sync.Pool
	mu    sync.Mutex
	list  list.List[waiter[C]]
}

// waitForConn blocks until a connection with the given Setting is returned by another client,
// or until the given context expires.
// The returned connection may _not_ have the requested Setting. This function can
// also return a `nil` connection even if our context has expired, if the pool has
// forced an expiration of all waiters in the waitlist.
func (wl *waitlist[C]) waitForConn(ctx context.Context, setting *Setting, closeChan <-chan struct{}) (*Pooled[C], error) {
	elem := wl.nodes.Get().(*list.Element[waiter[C]])
	defer wl.nodes.Put(elem)
	elem.Value = waiter[C]{setting: setting, conn: nil, ctx: ctx}

	wl.mu.Lock()
	// add ourselves as a waiter at the end of the waitlist
	wl.list.PushBackValue(elem)
	wl.mu.Unlock()

	done := make(chan struct{})
	go func() {
		// Block on our waiter's semaphore until somebody can hand over a connection to us.
		elem.Value.sema.wait()
		close(done)
	}()

	select {
	case <-closeChan:
		// Pool was closed while we were waiting.
		// Try to remove ourselves from the list. If we lose the race against
		// tryReturnConnSlow, it owns the element and will notify our semaphore.
		wl.mu.Lock()
		removed := wl.list.RemoveIfPresent(elem)
		wl.mu.Unlock()

		// If we removed ourselves from the waitlist, we need to notify our semaphore
		if removed {
			elem.Value.sema.notify(false)
		}

		// Wait for the semaphore to have been notified, either by us or by someone else
		<-done

		if removed {
			return nil, ErrConnPoolClosed
		}

		return elem.Value.conn, nil

	case <-ctx.Done():
		// Context expired. We need to try to remove ourselves from the waitlist to
		// prevent another goroutine from trying to hand us a connection later on.
		//
		// This removal MUST stay O(1). The code this replaces scanned the list
		// to locate elem before removing it, which is O(n) while holding wl.mu,
		// the mutex that serializes every acquisition and return in the pool.
		// Every waiter that times out pays that cost, so a timeout storm at
		// depth n serializes O(n^2) work on the hot path. RemoveIfPresent is a
		// required part of this backport, not incidental cleanup: do not
		// reintroduce the scan.
		//
		// If we lose the race against tryReturnConnSlow, it owns the element and
		// will notify our semaphore.
		wl.mu.Lock()
		removed := wl.list.RemoveIfPresent(elem)
		wl.mu.Unlock()

		// If we removed ourselves from the waitlist, we need to notify our semaphore
		if removed {
			elem.Value.sema.notify(false)
		}

		// Wait for the semaphore to have been notified, either by us or by someone else
		<-done

		if removed {
			return nil, context.Cause(ctx)
		}

		return elem.Value.conn, nil

	case <-done:
		return elem.Value.conn, nil
	}
}

func (wl *waitlist[C]) maybeStarvingCount() (maybeStarving int) {
	if wl.list.Len() == 0 {
		return
	}

	wl.mu.Lock()
	defer wl.mu.Unlock()

	// count the waiters that no returner has aged yet; they may be starving.
	// Waiters whose context has already expired cannot use a connection and are
	// only listed until a returner evicts them, so they don't count.
	for e := wl.list.Front(); e != nil; e = e.Next() {
		if e.Value.ctx.Err() != nil {
			continue
		}
		if e.Value.age == 0 {
			maybeStarving++
		}
	}

	return
}

// tryReturnConn tries handing over a connection to one of the waiters in the pool.
//
// Waiters whose context has already expired when they are examined are evicted
// rather than selected. A waiter that is still live when examined but expires
// before it is notified will still receive the connection; that narrow race is
// unchanged by this fix and is not something the pool can close from here.
func (wl *waitlist[D]) tryReturnConn(conn *Pooled[D]) bool {
	// fast path: if there's nobody waiting there's nothing to do
	if wl.list.Len() == 0 {
		return false
	}
	// split the slow path into a separate function to enable inlining
	return wl.tryReturnConnSlow(conn)
}

func (wl *waitlist[D]) tryReturnConnSlow(conn *Pooled[D]) bool {
	const maxAge = 8
	var (
		target      *list.Element[waiter[D]]
		expired     []*list.Element[waiter[D]]
		connSetting = conn.Conn.Setting()
	)

	wl.mu.Lock()
	var next *list.Element[waiter[D]]
	for e := wl.list.Front(); e != nil; e = next {
		// capture the successor before a possible Remove unlinks e
		next = e.Next()

		// Never hand a connection to a waiter whose context has already
		// expired: that waiter cannot use it, so the handoff would consume a
		// return without making progress for anyone. Evicting it instead also
		// stops the list accumulating a dead prefix that every later return
		// has to walk. Deadlines are heterogeneous, so expired waiters sit
		// anywhere in the list, not only at the front; every waiter examined
		// before a target is selected is therefore checked for expiry.
		if e.Value.ctx.Err() != nil {
			wl.list.Remove(e)
			expired = append(expired, e)
			continue
		}

		if target == nil {
			// front-most live waiter, used unless a better match is found below
			target = e
		}
		if e.Value.age > maxAge || e.Value.setting == connSetting {
			target = e
			break
		}
		// this only ages the waiters that are being skipped over: we'll start
		// aging the waiters in the back once they get to the front of the pool.
		// the maxAge of 8 has been set empirically: smaller values cause clients
		// with a specific setting to slightly starve, and aging all the clients
		// in the list every time leads to unfairness when the system is at capacity
		e.Value.age++
	}
	if target != nil {
		wl.list.Remove(target)
	}
	wl.mu.Unlock()

	// Hand the connection over before waking the evicted waiters. The target is
	// the only party here with a deadline left to make; the evicted waiters are
	// all going to return a timeout, so their wakeups must not queue in front of
	// the handoff. Under mass expiry that ordering is the difference between the
	// target being notified immediately and being notified after N semaphore
	// releases, which would widen the window in which it expires post-selection.
	if target != nil {
		// write the connection into the waiter and signal their semaphore. they'll
		// wake up to pick up the connection.
		target.Value.conn = conn
		target.Value.sema.notify(true)
	}

	// Wake the evicted waiters. Their conn stays nil, which waitForConn hands
	// back to the caller and which Get maps to a timeout.
	//
	// This is deliberately outside the lock. It is safe because an evicted
	// waiter is blocked on its semaphore and cannot return (nor recycle its
	// list element) until we notify it, and because we removed it from the list
	// under the lock nobody else can. Keeping the wakeups out of the critical
	// section matters: under mass expiry this loop can be long, and wl.mu
	// serializes every acquisition and return in the pool. `expired` is nil,
	// and allocates nothing, whenever no waiter has expired.
	for _, e := range expired {
		e.Value.sema.notify(false)
	}

	// target is nil when there was nobody to hand the connection to, because
	// every waiter had expired or we raced with another returner.
	return target != nil
}

func (wl *waitlist[C]) init() {
	wl.nodes.New = func() any {
		return &list.Element[waiter[C]]{}
	}
	wl.list.Init()
}

func (wl *waitlist[C]) waiting() int {
	return wl.list.Len()
}
