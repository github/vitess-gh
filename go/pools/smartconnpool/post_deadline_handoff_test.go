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

// expiredCtx becomes expired (Err() != nil) once its deadline passes, but its
// Done() channel never fires.
//
// This deterministically pins the pool-side state under test: a waiter whose
// acquisition deadline has already passed but which is still linked in the
// waitlist, because its own goroutine has not yet executed, or has not yet won,
// its self-removal under wl.mu.
//
// Using a real context.WithTimeout would reach the same state only by winning a
// race against the returner. Suppressing Done() removes the race without changing
// anything the returner can observe: tryReturnConnSlow only ever reads
// waiter.ctx, never the waiter's Done channel.
//
// Scope: this proves what the pool does IF an expired waiter is still linked
// when a returner examines it. It says nothing about how often real workloads
// reach that state; TestExpiredWaiterEvictionIsRaceSafe exercises the same
// invariant with real contexts and the real race.
type expiredCtx struct{ deadline time.Time }

func (c *expiredCtx) Deadline() (time.Time, bool) { return c.deadline, true }
func (c *expiredCtx) Done() <-chan struct{}       { return nil }
func (c *expiredCtx) Value(any) any               { return nil }
func (c *expiredCtx) Err() error {
	if time.Now().After(c.deadline) {
		return context.DeadlineExceeded
	}
	return nil
}

// TestPostDeadlineHandoff asserts the invariant that upstream vitessio/vitess
// #20308 restores: the pool must never hand a connection to a waiter whose
// acquisition context has already expired.
//
// Against the unfixed base this test FAILS, showing the observable shape of the
// defect:
//   - Get() returns success after its acquisition deadline
//   - the success-only WaitTime/WaitCount metric therefore exceeds the deadline
//   - and no connection is closed or re-dialled while it happens
func TestPostDeadlineHandoff(t *testing.T) {
	// scaled equivalent of the production --queryserver-config-txpool-timeout=1s
	const acquireTimeout = 100 * time.Millisecond

	var state TestState
	p := NewPool(&Config[*TestConn]{
		Capacity:    1,
		IdleTimeout: time.Minute,
		LogWait:     state.LogWait,
	}).Open(newConnector(&state), nil)
	defer p.Close()

	// Occupy the only connection so the next caller must enqueue as a waiter.
	held, err := p.Get(context.Background(), nil)
	require.NoError(t, err)
	dialsAfterWarmup := state.open.Load()

	type result struct {
		conn    *Pooled[*TestConn]
		err     error
		elapsed time.Duration
		ctxErr  error
	}
	resc := make(chan result, 1)

	ctx := &expiredCtx{deadline: time.Now().Add(acquireTimeout)}
	go func() {
		start := time.Now()
		c, err := p.Get(ctx, nil)
		resc <- result{conn: c, err: err, elapsed: time.Since(start), ctxErr: ctx.Err()}
	}()

	// Wait until the caller is actually parked on the waitlist.
	deadline := time.Now().Add(5 * time.Second)
	for p.wait.waiting() == 0 {
		require.True(t, time.Now().Before(deadline), "waiter never enqueued")
		time.Sleep(time.Millisecond)
	}

	// Let the acquisition deadline pass while the waiter is parked.
	time.Sleep(2 * acquireTimeout)
	require.Error(t, ctx.Err(), "precondition: waiter ctx must be expired before the return")
	require.Equal(t, 1, p.wait.waiting(), "precondition: expired waiter is still on the waitlist")

	// The returner hands the connection back. Front of the list is the expired waiter.
	waitsBefore := p.Metrics.WaitCount()
	held.Recycle()

	r := <-resc

	t.Logf("Get -> err=%v elapsed=%v ctxErr=%v | WaitCount=%d WaitTime=%v | dials=%d closes=%d",
		r.err, r.elapsed, r.ctxErr,
		p.Metrics.WaitCount(), p.Metrics.WaitTime(),
		state.open.Load(), state.close.Load())

	// (1) The pool must not report success to a waiter that had already expired.
	if r.err == nil && r.conn != nil {
		mean := time.Duration(0)
		if n := p.Metrics.WaitCount(); n > 0 {
			mean = time.Duration(int64(p.Metrics.WaitTime()) / n)
		}
		r.conn.Recycle()
		t.Fatalf("POST-DEADLINE HANDOFF: pool delivered a connection to a waiter whose "+
			"acquisition context expired %v earlier.\n"+
			"  Get() returned SUCCESS after %v (acquisition deadline %v)\n"+
			"  success-only metric mean wait = %v  > deadline %v  <-- the defect\n"+
			"  dials during handoff = %d (no redial)  closes = %d",
			r.elapsed-acquireTimeout, r.elapsed, acquireTimeout,
			mean, acquireTimeout,
			state.open.Load()-dialsAfterWarmup, state.close.Load())
	}

	// (2) With the fix, the expired waiter is evicted and the acquisition fails.
	require.Error(t, r.err, "expired waiter must not receive a connection")
	require.Nil(t, r.conn)

	// (3) Evicting the expired waiter must NOT pollute the success-only wait
	// metric. WaitCount/WaitTime have always counted successful acquisitions
	// only; a failed eviction must leave them untouched.
	require.Equal(t, waitsBefore, p.Metrics.WaitCount(),
		"evicted expired waiter must not increment WaitCount (success-only metric)")

	// (4) Either way, no connection is closed or re-dialled by this path. The
	// defect wastes a handoff; it does not churn connections, which is why it
	// leaves no trace in MySQL's Connections counter.
	require.Equal(t, dialsAfterWarmup, state.open.Load(), "no redial must occur")
	require.Zero(t, state.close.Load(), "no connection close must occur")
}

// TestInteriorExpiredWaiterNotServed covers the case that a prefix-only
// eviction would miss. Production deadlines are heterogeneous, so an expired
// waiter can sit BEHIND a live one. tryReturnConnSlow picks its target by
// `age > maxAge || setting == connSetting`, so an interior expired waiter whose
// Setting matches the returned connection can be selected ahead of the live
// waiter at the front of the list.
//
// The selection loop does not stop at the first live waiter: it records that
// waiter as a fallback target and keeps going until it finds a better match
// (`age > maxAge || setting == connSetting`) or reaches the end. Every waiter it
// examines on the way is checked for expiry. This test asserts that behaviour:
// the interior expired waiter must be evicted, and the live waiter must receive
// the connection.
func TestInteriorExpiredWaiterNotServed(t *testing.T) {
	settingA := NewSetting("set workload='olap'", "set workload='oltp'")

	state := &TestState{}
	p := NewPool(&Config[*TestConn]{Capacity: 1, IdleTimeout: time.Hour}).
		Open(newConnector(state), nil)
	defer p.Close()

	// Hold the only connection and stamp settingA on it, so the connection that
	// is later returned carries settingA.
	held, err := p.Get(context.Background(), settingA)
	require.NoError(t, err)

	var liveServed, expiredServed atomic.Int64
	var wg sync.WaitGroup

	// (1) LIVE waiter, no Setting -> occupies the FRONT of the waitlist.
	wg.Add(1)
	go func() {
		defer wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if c, err := p.Get(ctx, nil); err == nil && c != nil {
			liveServed.Add(1)
			c.Recycle()
		}
	}()
	require.Eventually(t, func() bool {
		return p.wait.waiting() >= 1
	}, 30*time.Second, 200*time.Microsecond, "live waiter never enqueued")

	// (2) EXPIRED waiter, Setting == settingA -> sits BEHIND the live waiter,
	// and is the better `setting == connSetting` match for the returned conn.
	wg.Add(1)
	go func() {
		defer wg.Done()
		ctx := &expiredCtx{deadline: time.Now().Add(50 * time.Millisecond)}
		if c, err := p.Get(ctx, settingA); err == nil && c != nil {
			expiredServed.Add(1)
			c.Recycle()
		}
	}()
	require.Eventually(t, func() bool {
		return p.wait.waiting() >= 2
	}, 30*time.Second, 200*time.Microsecond, "expired waiter never enqueued")
	time.Sleep(100 * time.Millisecond) // waiter 2 is now expired, still linked

	held.Recycle()
	wg.Wait()

	t.Logf("live served=%d  expired served=%d", liveServed.Load(), expiredServed.Load())
	require.Zero(t, expiredServed.Load(),
		"interior expired waiter must not receive a connection")
	require.Equal(t, int64(1), liveServed.Load(),
		"live waiter must receive the connection instead")
}
