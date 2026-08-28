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

	// iterate the waitlist looking for waiters that are still live and have not
	// been skipped over yet. Waiters whose context has already expired are not
	// counted: opening a new connection on their behalf would be wasted work.
	for e := wl.list.Front(); e != nil; e = e.Next() {
		if e.Value.ctx != nil && e.Value.ctx.Err() != nil {
			continue
		}
		if e.Value.age == 0 {
			maybeStarving++
		}
	}

	return
}

// tryReturnConn tries handing over a connection to one of the waiters in the pool.
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

		// Never hand a connection over to a waiter whose context has already
		// expired: that waiter can no longer use it, and for the transaction
		// pool the subsequent failed Begin closes the connection and forces a
		// fresh dial. Evict the expired waiter instead so the connection stays
		// available for a waiter that can still use it. Expired waiters can sit
		// anywhere in the list, not just at the front, because deadlines are
		// heterogeneous, so the whole scan has to check.
		if e.Value.ctx != nil && e.Value.ctx.Err() != nil {
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

	// Wake the evicted waiters. Their conn stays nil, which waitForConn hands
	// back to the caller and which Get maps to a timeout. This is safe to do
	// after releasing the lock: an evicted waiter is blocked on its semaphore
	// and cannot return (nor recycle its list element) until we notify it, and
	// because we removed it from the list under the lock nobody else can.
	for _, e := range expired {
		e.Value.sema.notify(false)
	}

	// maybe there isn't anybody to hand over the connection to, because we've
	// raced with another client returning another connection
	if target == nil {
		return false
	}

	// if we have a target to return the connection to, simply write the connection
	// into the waiter and signal their semaphore. they'll wake up to pick up the
	// connection.
	target.Value.conn = conn
	target.Value.sema.notify(true)
	return true
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
