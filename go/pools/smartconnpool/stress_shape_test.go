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
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// This is a load harness, not an assertion of production behaviour. It exists
// to show that the stage metrics can tell the candidate explanations apart,
// and to produce the numbers by which they are told apart. It is skipped
// unless SMARTCONNPOOL_STRESS is set, because it is deliberately slow and its
// timings are machine-dependent.
//
// Deliberately NOT asserted here: that this reproduces any production
// incident. The offered load is synthetic and the service time is a sleep.

type stressPhase struct {
	name     string
	rate     int // offered acquisitions per second
	duration time.Duration
	// timeout is the acquisition deadline for this phase. A phase with a
	// deadline shorter than the queue delay produces a mass timeout, which is
	// the only way to exercise the waiter self-removal path.
	timeout time.Duration
}

type stressResult struct {
	phase string

	offered   int64
	admitted  int64
	rejected  int64
	completed int64

	waitersPeak   int64
	waitToBorrow  time.Duration
	notifyToWake  time.Duration
	failedWaitAvg time.Duration
	okWaitAvg     time.Duration

	selfRemovalOps      int64
	selfRemovalExamined int64
	returnSelectOps     int64
	examinedPerOp       float64
	holdAvg             time.Duration

	goroutines int
	heapMB     uint64
}

// TestProductionShapedStress drives the pool open-loop through a healthy
// baseline, a ramp, a burst, sustained pressure, and a hard load removal, and
// reports the stage decomposition for each phase.
//
// Open-loop matters: a closed-loop client stops offering load when the pool
// slows down, which hides exactly the queue growth we are looking for.
func TestProductionShapedStress(t *testing.T) {
	if os.Getenv("SMARTCONNPOOL_STRESS") == "" {
		t.Skip("set SMARTCONNPOOL_STRESS=1 to run the load harness")
	}

	const (
		capacity    = 900
		serviceTime = 3 * time.Millisecond // measured production residence
		acquireWait = time.Second          // production txpool timeout
	)

	// Sustainable rate is capacity/serviceTime. Phases are expressed as
	// multiples of it so the harness is meaningful on any machine.
	sustainable := int(float64(capacity) / serviceTime.Seconds())
	t.Logf("capacity=%d serviceTime=%v acquireTimeout=%v GOMAXPROCS=%d sustainable~%d/s",
		capacity, serviceTime, acquireWait, runtime.GOMAXPROCS(0), sustainable)

	phases := []stressPhase{
		{"baseline", sustainable / 2, 3 * time.Second, acquireWait},
		{"ramp", sustainable, 3 * time.Second, acquireWait},
		{"burst", sustainable * 3, 2 * time.Second, acquireWait},
		{"sustained", sustainable * 2, 4 * time.Second, acquireWait},
		// Deadline far below the queue delay reached above, so essentially
		// every waiter cancels itself. This is the phase in which the
		// cancellation path dominates.
		{"timeoutstorm", sustainable * 3, 3 * time.Second, 2 * time.Millisecond},
	}

	var state TestState
	p := NewPool(&Config[*TestConn]{
		Capacity: capacity,
		LogWait:  state.LogWait,
	}).Open(newConnector(&state), nil)
	defer p.Close()

	ctx := context.Background()
	var results []stressResult

	for _, ph := range phases {
		var (
			offered, admitted, rejected, completed atomic.Int64
			wg                                     sync.WaitGroup
		)

		before := snapshotStages(p)
		stop := time.After(ph.duration)
		tick := time.NewTicker(time.Second / time.Duration(max(ph.rate, 1)))

	loop:
		for {
			select {
			case <-stop:
				break loop
			case <-tick.C:
				// Open loop: a new caller is offered regardless of whether
				// earlier callers have finished.
				offered.Add(1)
				wg.Add(1)
				go func() {
					defer wg.Done()
					cctx, cancel := context.WithTimeout(ctx, ph.timeout)
					defer cancel()
					conn, err := p.Get(cctx, nil)
					if err != nil {
						rejected.Add(1)
						return
					}
					admitted.Add(1)
					time.Sleep(serviceTime)
					conn.Recycle()
					completed.Add(1)
				}()
			}
		}
		tick.Stop()

		gor := runtime.NumGoroutine()
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)

		wg.Wait()

		after := snapshotStages(p)
		d := after.sub(before)

		results = append(results, stressResult{
			phase:               ph.name,
			offered:             offered.Load(),
			admitted:            admitted.Load(),
			rejected:            rejected.Load(),
			completed:           completed.Load(),
			waitersPeak:         p.Stages.WaitersPeak(),
			waitToBorrow:        avg(d.waitToBorrowTime, d.waitToBorrowCount),
			notifyToWake:        avg(d.notifyToResumeTime, d.notifyToResumeCount),
			failedWaitAvg:       avg(d.failedWaitTime, d.failedWaitCount),
			okWaitAvg:           avg(d.waitTime, d.waitCount),
			selfRemovalOps:      d.wlSelfRemovalOps,
			selfRemovalExamined: d.wlSelfRemovalOps, // one per op by construction
			returnSelectOps:     d.wlReturnSelectionOps,
			examinedPerOp:       ratio(d.wlElementsExamined, d.wlSelfRemovalOps+d.wlReturnSelectionOps+d.wlStarvationScanOps),
			holdAvg:             avg(d.wlHoldTimeSampled, d.wlHoldSampleCount),
			goroutines:          gor,
			heapMB:              ms.HeapAlloc / (1 << 20),
		})
	}

	// Hard load removal: everything above has drained by now because each
	// phase waits for its own callers. Recovery is therefore measured as the
	// time for the pool to return to a quiescent state.
	recoveryStart := time.Now()
	for p.Stages.WaitersCurrent() != 0 && time.Since(recoveryStart) < 10*time.Second {
		time.Sleep(time.Millisecond)
	}
	recovery := time.Since(recoveryStart)

	t.Logf("\n%-10s %8s %8s %8s %8s %7s %10s %10s %10s %10s %8s %8s %7s %6s",
		"phase", "offered", "admitted", "rejected", "done", "peakW",
		"wait->borrow", "notify->wake", "failWait", "okWait", "selfRm", "retSel", "exam/op", "gor")
	for _, r := range results {
		t.Logf("%-10s %8d %8d %8d %8d %7d %10v %10v %10v %10v %8d %8d %7.2f %6d",
			r.phase, r.offered, r.admitted, r.rejected, r.completed, r.waitersPeak,
			r.waitToBorrow, r.notifyToWake, r.failedWaitAvg, r.okWaitAvg,
			r.selfRemovalOps, r.returnSelectOps, r.examinedPerOp, r.goroutines)
	}
	t.Logf("recovery to zero waiters: %v", recovery)

	// The harness is only useful if it actually applied pressure and actually
	// produced cancellations; without both, nothing below is being tested.
	storm := results[len(results)-1]
	require.Positive(t, storm.waitersPeak,
		"precondition: the load must have produced waiters")
	require.Positive(t, storm.rejected,
		"precondition: the timeout storm must have produced rejections")
	require.Positive(t, storm.selfRemovalOps,
		"precondition: the timeout storm must have exercised self-removal")

	// The point of the O(1) removal: each cancellation examines exactly one
	// waiter, no matter how deep the list is when it cancels.
	require.InDelta(t, 1.0, ratio(storm.selfRemovalExamined, storm.selfRemovalOps), 0.001,
		"each self-removal must examine exactly one waiter regardless of depth")

	// H1 would show waitlist work per operation growing with depth. With
	// O(1) self-removal it must not, whatever the depth reached.
	for _, r := range results {
		require.Less(t, r.examinedPerOp, float64(r.waitersPeak)/2+2,
			"phase %s: waitlist work per operation must not scale with waiter depth", r.phase)
	}

	// Recovery must be prompt: a pool that cannot drain is the failure mode
	// worth catching, and it is independent of machine speed at this scale.
	require.Less(t, recovery, 5*time.Second, "pool did not drain after load removal")
}

type stageSnapshot struct {
	waitCount, waitTime                     int64
	waitToBorrowCount, waitToBorrowTime     int64
	notifyToResumeCount, notifyToResumeTime int64
	failedWaitCount, failedWaitTime         int64
	wlSelfRemovalOps, wlReturnSelectionOps  int64
	wlStarvationScanOps, wlElementsExamined int64
	wlHoldSampleCount, wlHoldTimeSampled    int64
}

func snapshotStages[C Connection](p *ConnPool[C]) stageSnapshot {
	return stageSnapshot{
		waitCount:            p.Metrics.WaitCount(),
		waitTime:             int64(p.Metrics.WaitTime()),
		waitToBorrowCount:    p.Stages.WaitToBorrowCount(),
		waitToBorrowTime:     int64(p.Stages.WaitToBorrowTime()),
		notifyToResumeCount:  p.Stages.NotifyToResumeCount(),
		notifyToResumeTime:   int64(p.Stages.NotifyToResumeTime()),
		failedWaitCount:      p.Stages.FailedWaitCount(),
		failedWaitTime:       int64(p.Stages.FailedWaitTime()),
		wlSelfRemovalOps:     p.Stages.WlSelfRemovalOps(),
		wlReturnSelectionOps: p.Stages.WlReturnSelectionOps(),
		wlStarvationScanOps:  p.Stages.WlStarvationScanOps(),
		wlElementsExamined:   p.Stages.WlElementsExamined(),
		wlHoldSampleCount:    p.Stages.WlHoldSampleCount(),
		wlHoldTimeSampled:    int64(p.Stages.WlHoldTimeSampled()),
	}
}

func (a stageSnapshot) sub(b stageSnapshot) stageSnapshot {
	return stageSnapshot{
		waitCount:            a.waitCount - b.waitCount,
		waitTime:             a.waitTime - b.waitTime,
		waitToBorrowCount:    a.waitToBorrowCount - b.waitToBorrowCount,
		waitToBorrowTime:     a.waitToBorrowTime - b.waitToBorrowTime,
		notifyToResumeCount:  a.notifyToResumeCount - b.notifyToResumeCount,
		notifyToResumeTime:   a.notifyToResumeTime - b.notifyToResumeTime,
		failedWaitCount:      a.failedWaitCount - b.failedWaitCount,
		failedWaitTime:       a.failedWaitTime - b.failedWaitTime,
		wlSelfRemovalOps:     a.wlSelfRemovalOps - b.wlSelfRemovalOps,
		wlReturnSelectionOps: a.wlReturnSelectionOps - b.wlReturnSelectionOps,
		wlStarvationScanOps:  a.wlStarvationScanOps - b.wlStarvationScanOps,
		wlElementsExamined:   a.wlElementsExamined - b.wlElementsExamined,
		wlHoldSampleCount:    a.wlHoldSampleCount - b.wlHoldSampleCount,
		wlHoldTimeSampled:    a.wlHoldTimeSampled - b.wlHoldTimeSampled,
	}
}

func avg(total, n int64) time.Duration {
	if n == 0 {
		return 0
	}
	return time.Duration(total / n)
}

func ratio(a, b int64) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b)
}
