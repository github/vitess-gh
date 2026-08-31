/*
Copyright 2025 The Vitess Authors.

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
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// lateExpiryCtx is valid at the moment its waiter enqueues and expires while
// that waiter is parked on the waitlist. Its Done channel is never closed, so
// the waiter does not remove itself on expiry. This deterministically produces
// a waitlist containing already-expired waiters.
//
// Scope: this pins the precondition -- an expired waiter still linked when a
// returner examines it -- so that selection behaviour can be asserted without a
// race. It is not evidence about how often real workloads reach that state.
type lateExpiryCtx struct {
	context.Context
	deadline time.Time
	done     chan struct{}
}

func (c *lateExpiryCtx) Deadline() (time.Time, bool) { return c.deadline, true }
func (c *lateExpiryCtx) Done() <-chan struct{}       { return c.done }
func (c *lateExpiryCtx) Err() error {
	if time.Now().After(c.deadline) {
		return context.DeadlineExceeded
	}
	return nil
}

func newLateExpiryCtx(deadline time.Time) *lateExpiryCtx {
	return &lateExpiryCtx{
		Context:  context.Background(),
		deadline: deadline,
		done:     make(chan struct{}),
	}
}

// TestExpiredWaitersDoNotReceiveConnections asserts that a connection returned
// to the pool is never handed to a waiter whose context has already expired,
// including when expired waiters sit *behind* a live waiter in the list.
//
// Serving an expired waiter wastes the handoff: its acquisition has already
// expired, so it is no longer eligible for a successful acquisition, and the
// return makes no progress for the live waiters that are still waiting.
func TestExpiredWaitersDoNotReceiveConnections(t *testing.T) {
	var state TestState

	p := NewPool(&Config[*TestConn]{
		Capacity: 1,
		LogWait:  state.LogWait,
	}).Open(newConnector(&state), nil)
	defer p.Close()

	// Occupy the only slot so that every subsequent Get parks on the waitlist.
	held, err := p.Get(context.Background(), nil)
	require.NoError(t, err)

	// A single shared deadline keeps expiry deterministic across all waiters.
	deadline := time.Now().Add(2 * time.Second)

	// Expired (true) and live (false) waiters, interleaved so that expired
	// waiters sit both ahead of and behind live waiters.
	kinds := []bool{true, true, true, false, true, true, true, false}

	type outcome struct {
		expired bool
		gotConn bool
	}
	results := make([]outcome, len(kinds))

	var wg sync.WaitGroup
	for i, isExpired := range kinds {
		results[i].expired = isExpired

		ctx := context.Context(context.Background())
		if isExpired {
			ctx = newLateExpiryCtx(deadline)
		}

		before := p.wait.waiting()
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, err := p.Get(ctx, nil)
			if err == nil && conn != nil {
				results[i].gotConn = true
				p.put(conn)
			}
		}()

		// Wait until this waiter is on the list before starting the next one,
		// so that the resulting list order is exactly `kinds`.
		require.Eventuallyf(t, func() bool {
			return p.wait.waiting() == before+1
		}, 5*time.Second, time.Millisecond, "waiter %d never enqueued", i)
	}

	// Let the lateExpiryCtx waiters expire while they are parked.
	time.Sleep(time.Until(deadline) + 100*time.Millisecond)

	// Releasing the held connection starts the handoff chain.
	p.put(held)

	waited := make(chan struct{})
	go func() { wg.Wait(); close(waited) }()
	select {
	case <-waited:
	case <-time.After(30 * time.Second):
		t.Fatal("waiters did not finish; connection handoff stalled")
	}

	for i, r := range results {
		if r.expired {
			require.Falsef(t, r.gotConn,
				"expired waiter %d received a connection; its acquisition had already expired", i)
		} else {
			require.Truef(t, r.gotConn,
				"live waiter %d was starved of a connection", i)
		}
	}
}

// TestExpiredWaiterEvictionIsRaceSafe drives pool-side eviction of expired
// waiters concurrently with waiters that remove themselves on genuine context
// cancellation, to confirm that exactly one party owns each waitlist element.
//
// Two properties are needed for that race to happen on every run:
//
// The pool is held at capacity until every worker is parked. A pool with a free
// slot satisfies Get immediately, so a serialized or lightly loaded run would
// enqueue no waiters at all and the drain assertion would pass vacuously.
//
// The expiring group uses lateExpiryCtx rather than context.WithTimeout. A real
// context closes Done on expiry, which wakes its own waiter and makes it remove
// itself; the pool never evicts it. Only a context that becomes expired without
// firing Done leaves the element for the pool to evict, which is the path under
// test.
func TestExpiredWaiterEvictionIsRaceSafe(t *testing.T) {
	// Divisible by 3 so each group below has exactly workers/3 members.
	const workers = 201
	const capacity = 2

	var state TestState

	p := NewPool(&Config[*TestConn]{
		Capacity: capacity,
		LogWait:  state.LogWait,
	}).Open(newConnector(&state), nil)
	defer p.Close()

	// Occupy every slot, so each worker below is forced onto the waitlist.
	var held []*Pooled[*TestConn]
	for i := 0; i < capacity; i++ {
		conn, err := p.Get(context.Background(), nil)
		require.NoError(t, err)
		held = append(held, conn)
	}

	expiry := time.Now().Add(250 * time.Millisecond)
	cancelGate := make(chan struct{})

	var evictedServed, liveServed atomic.Int64

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()

			var (
				ctx    context.Context
				cancel context.CancelFunc
				served *atomic.Int64
			)
			switch i % 3 {
			case 0:
				// Expires while parked without firing Done, so the pool has to
				// evict it.
				ctx, cancel = newLateExpiryCtx(expiry), func() {}
				served = &evictedServed
			case 1:
				// Cancelled while parked, so it removes itself, racing the
				// eviction scan.
				ctx, cancel = context.WithCancel(context.Background())
				go func() { <-cancelGate; cancel() }()
				served = nil
			default:
				// Stays live throughout and must be served.
				ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
				served = &liveServed
			}
			defer cancel()

			conn, err := p.Get(ctx, nil)
			if err == nil && conn != nil {
				if served != nil {
					served.Add(1)
				}
				p.put(conn)
			}
		}(i)
	}

	// Nothing can leave the waitlist until cancelGate closes and the held
	// connections go back, so reaching the full count is a deterministic proof
	// that the race below is exercised rather than skipped.
	require.Eventually(t, func() bool {
		return p.wait.waiting() == workers
	}, 30*time.Second, time.Millisecond,
		"workers never all enqueued, so the eviction race was not exercised")

	// Let the lateExpiryCtx group expire while it is parked.
	time.Sleep(time.Until(expiry) + 50*time.Millisecond)

	// Overlap the two removal paths: closing the gate starts the self-removals,
	// returning the connections starts the scan that evicts expired waiters.
	close(cancelGate)
	for _, conn := range held {
		p.put(conn)
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatalf("workers did not finish; %d still parked", p.wait.waiting())
	}

	require.Zero(t, evictedServed.Load(),
		"expired waiter was handed a connection instead of being evicted")
	require.Equal(t, int64(workers/3), liveServed.Load(),
		"live waiters were starved by the eviction scan")
	require.Zero(t, p.wait.waiting(), "waitlist should be drained")
}
