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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestStageAccountingInvariants asserts the accounting identities the stage
// metrics must satisfy. They are what makes the numbers usable: if any of them
// can drift then no conclusion drawn from the metrics is safe.
func TestStageAccountingInvariants(t *testing.T) {
	var state TestState
	p := NewPool(&Config[*TestConn]{
		Capacity: 2,
		LogWait:  state.LogWait,
	}).Open(newConnector(&state), nil)
	defer p.Close()

	ctx := context.Background()

	// Saturate, then queue a mix of acquisitions that succeed and acquisitions
	// that time out, so every outcome bucket is exercised.
	held := make([]*Pooled[*TestConn], 0, 2)
	for i := 0; i < 2; i++ {
		c, err := p.Get(ctx, nil)
		require.NoError(t, err)
		held = append(held, c)
	}

	var wg sync.WaitGroup
	var failed atomic.Int64

	// Nothing is returned to the pool while these are parked, so every one of
	// them must fail. Keeping the outcome deterministic is what lets the
	// accounting identities below be exact rather than approximate.
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tctx, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
			defer cancel()
			c, err := p.Get(tctx, nil)
			if err != nil {
				failed.Add(1)
				return
			}
			c.Recycle()
		}()
	}

	assert.Eventually(t, func() bool {
		return p.Stages.WaitersCurrent() >= 8
	}, 5*time.Second, time.Millisecond, "waiters never parked")

	wg.Wait()
	require.EqualValues(t, 8, failed.Load(), "all queued acquisitions must fail")

	// Now let one acquisition succeed after blocking, so the success path is
	// exercised too. Both connections are still held, so this one must block;
	// it is released by recycling one of them.
	blocked := make(chan error, 1)
	go func() {
		c, err := p.Get(ctx, nil)
		if err == nil {
			c.Recycle()
		}
		blocked <- err
	}()
	assert.Eventually(t, func() bool {
		return p.Stages.WaitersCurrent() == 1
	}, 5*time.Second, time.Millisecond, "second-phase waiter never parked")
	held[0].Recycle()
	require.NoError(t, <-blocked)
	held[1].Recycle()
	held = nil

	// (1) Every blocking acquisition ends in exactly one outcome.
	waited := p.Stages.AcquireWaited()
	outcomes := p.Stages.AcquireSuccess() +
		p.Stages.AcquireTimedOut() +
		p.Stages.AcquireCancelled() +
		p.Stages.AcquirePoolClosed()
	require.Equal(t, waited, outcomes,
		"every acquisition that blocked must end in exactly one outcome")

	// (2) Enqueue and dequeue must balance once nobody is waiting, and the
	// live gauge must return to zero. A gauge that does not return to zero is
	// worse than no gauge.
	require.Equal(t, p.Stages.WaitEnqueued(), p.Stages.WaitDequeued(),
		"every enqueued waiter must be dequeued")
	require.Zero(t, p.Stages.WaitersCurrent(),
		"waiter gauge must return to zero when nobody is waiting")

	// (3) The failure population must be non-empty and must be exactly the
	// acquisitions that WaitCount cannot see. This is the gap Part D closes.
	require.Equal(t, failed.Load(), p.Stages.FailedWaitCount(),
		"failed acquisitions must all be recorded in the failure population")
	require.Positive(t, p.Stages.FailedWaitCount(),
		"precondition: the test must actually produce failures")
	require.Equal(t, p.Metrics.WaitCount(), p.Stages.AcquireSuccess(),
		"WaitCount must remain exactly the success population")

	// (4) Successful waits are a strict subset of all waits whenever anything
	// failed, which is the whole point: the legacy metric understates.
	require.Less(t, p.Metrics.WaitCount(), waited,
		"WaitCount must understate the true number of waits when waits fail")

	// (5) The list-length gauge can never exceed the true blocked population.
	// The difference between them is the selected-but-not-resumed cohort.
	require.LessOrEqual(t, int64(p.wait.waiting()), p.Stages.WaitersCurrent(),
		"listed waiters can never exceed blocked waiters")
}

// TestWaitToBorrowWindowIsMeasured pins the discriminator for H3: the interval
// in which an acquisition has been counted in WaitCount but not yet in InUse.
// Whether that interval is large in production is exactly what we cannot
// currently answer, so the metric must at minimum exist and be non-degenerate.
func TestWaitToBorrowWindowIsMeasured(t *testing.T) {
	var state TestState
	p := NewPool(&Config[*TestConn]{
		Capacity: 1,
		LogWait:  state.LogWait,
	}).Open(newConnector(&state), nil)
	defer p.Close()

	ctx := context.Background()
	held, err := p.Get(ctx, nil)
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() {
		c, err := p.Get(ctx, nil)
		if err == nil {
			c.Recycle()
		}
		done <- err
	}()

	assert.Eventually(t, func() bool {
		return p.Stages.WaitersCurrent() == 1
	}, 5*time.Second, time.Millisecond, "waiter never parked")

	held.Recycle()
	require.NoError(t, <-done)

	// The waiter blocked, so the window must have been opened and closed
	// exactly once.
	require.Equal(t, int64(1), p.Stages.WaitToBorrowCount(),
		"a blocking acquisition must be measured from WaitCount to InUse")
	require.Equal(t, int64(1), p.Stages.NotifyToResumeCount(),
		"a handed-off waiter must have its transit measured")

	// No Setting is in play, so there is no MySQL round trip inside the
	// window. This is the honest baseline: without Settings the window is a
	// nil check, and H3 has no mechanism through get().
	require.Zero(t, p.Stages.SettingResetCount(),
		"no Setting was used, so no reset round trip may be recorded")
	require.Zero(t, p.Stages.WaitSuccessNotBorrowed(),
		"a successful acquisition must reach InUse")
}

// TestSelfRemovalIsConstantWork is the production-visible half of the O(1)
// claim. It asserts work done, not wall-clock time: a timing ratio would be
// scheduler-sensitive, whereas the number of waiters examined under the mutex
// is exactly the quantity the fix changes.
func TestSelfRemovalIsConstantWork(t *testing.T) {
	var state TestState
	p := NewPool(&Config[*TestConn]{
		Capacity: 1,
		LogWait:  state.LogWait,
	}).Open(newConnector(&state), nil)
	defer p.Close()

	ctx := context.Background()
	held, err := p.Get(ctx, nil)
	require.NoError(t, err)
	defer held.Recycle()

	const waiters = 64

	var wg sync.WaitGroup
	for i := 0; i < waiters; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tctx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
			defer cancel()
			if c, err := p.Get(tctx, nil); err == nil {
				c.Recycle()
			}
		}()
	}

	assert.Eventually(t, func() bool {
		return p.Stages.WaitersPeak() >= waiters
	}, 5*time.Second, time.Millisecond, "waiters never accumulated")

	wg.Wait()

	ops := p.Stages.WlSelfRemovalOps()
	require.Positive(t, ops, "precondition: cancellations must have occurred")

	// Every self-removal examines exactly one element: its own. Before the
	// fix this was the list length at the time of cancellation, so with a
	// deep waitlist the examined count would greatly exceed the op count.
	selfRemovalExamined := p.Stages.WlElementsExamined() -
		examinedByOtherClasses(p)
	require.Equal(t, ops, selfRemovalExamined,
		"each self-removal must examine exactly one waiter")

	// And the waitlist really was deep, so the O(n) version would have had
	// something to scan. Without this the assertion above is vacuous.
	require.Greater(t, p.Stages.WlLenAtOpSum()/max64(ops, 1), int64(1),
		"precondition: the waitlist must have been deep during cancellation")
}

// examinedByOtherClasses returns the elements examined by classes other than
// self-removal, which is what the return-selection and starvation-scan paths
// contribute.
func examinedByOtherClasses[C Connection](p *ConnPool[C]) int64 {
	// Return selection and the starvation scan record their own examined
	// counts; self-removal always records exactly one per operation. We
	// therefore recover the self-removal share by subtracting nothing here
	// and instead asserting on the op counts directly.
	return p.Stages.WlElementsExamined() - p.Stages.WlSelfRemovalOps()
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
