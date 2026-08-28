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
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// lateExpiryCtx is valid at the moment its waiter enqueues and expires while
// that waiter is parked on the waitlist. Its Done channel is never closed, so
// the waiter does not remove itself on expiry. This deterministically produces
// a waitlist containing already-expired waiters, which is the shape that arises
// in production when clients time out faster than the pool hands out
// connections.
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
// Handing a connection to an expired waiter wastes it: the caller can no longer
// use it and, in the transaction pool, the failed Begin closes the connection
// and forces a fresh dial. Under load this starves the live waiters.
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
				"expired waiter %d received a connection; it can never use it", i)
		} else {
			require.Truef(t, r.gotConn,
				"live waiter %d was starved of a connection", i)
		}
	}
}

// TestExpiredWaiterEvictionIsRaceSafe exercises the eviction path concurrently
// with waiters removing themselves on genuine context cancellation, to confirm
// that exactly one party owns each waitlist element.
func TestExpiredWaiterEvictionIsRaceSafe(t *testing.T) {
	var state TestState

	p := NewPool(&Config[*TestConn]{
		Capacity: 2,
		LogWait:  state.LogWait,
	}).Open(newConnector(&state), nil)
	defer p.Close()

	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var ctx context.Context
			var cancel context.CancelFunc
			switch i % 3 {
			case 0:
				ctx, cancel = context.WithTimeout(context.Background(), time.Millisecond)
			case 1:
				ctx, cancel = context.WithCancel(context.Background())
				go func() { time.Sleep(time.Millisecond); cancel() }()
			default:
				ctx, cancel = context.WithTimeout(context.Background(), 2*time.Second)
			}
			defer cancel()

			conn, err := p.Get(ctx, nil)
			if err == nil && conn != nil {
				p.put(conn)
			}
		}(i)
	}
	wg.Wait()

	require.Zero(t, p.wait.waiting(), "waitlist should be drained")
}
