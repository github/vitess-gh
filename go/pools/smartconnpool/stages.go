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
	"sync/atomic"
	"time"
)

// StageMetrics decomposes a blocking acquisition into the stages it actually
// passes through, so that a slow or failing acquisition can be attributed to a
// specific stage rather than to the pool as a whole.
//
// The existing WaitCount/WaitTime pair cannot do this for two reasons. It is
// recorded at exactly one point, so it collapses every stage into a single
// number; and recordWait is only reached by acquisitions that succeed, so the
// population it describes is censored -- every timeout and cancellation is
// missing from it. Both are preserved unchanged for backwards compatibility;
// everything here is additive.
//
// Cost and cardinality. Every field is an atomic counter with no labels, so a
// stage costs one atomic add and, where a duration is recorded, one monotonic
// clock read. Durations are kept as (sum, count) pairs rather than histograms:
// that is enough to derive a mean per stage, which is what attribution needs,
// and it avoids bucket configuration. Nothing here allocates and nothing here
// is per-waiter, so none of it can grow with waitlist depth.
type StageMetrics struct {
	// ---- waiter population (Part D) ------------------------------------
	//
	// waitersCurrent is the number of callers actually blocked in
	// waitForConn. This is deliberately NOT waitlist.Len(): a waiter that
	// has been selected and removed from the list, but has not yet resumed,
	// is off the list and still blocked. Those waiters are invisible to a
	// list-length gauge and are exactly the ones worth seeing during a
	// handoff stall.
	waitEnqueued   atomic.Int64
	waitDequeued   atomic.Int64
	waitersCurrent atomic.Int64
	waitersPeak    atomic.Int64

	// ---- acquisition outcomes (Part D) ---------------------------------
	//
	// The failure population that WaitCount cannot see. acquireWaited counts
	// every acquisition that had to block, so
	//   acquireWaited - (success + timedOut + cancelled + poolClosed) == 0
	// is an invariant, and success/acquireWaited is the true wait success
	// rate.
	acquireWaited     atomic.Int64
	acquireSuccess    atomic.Int64
	acquireTimedOut   atomic.Int64
	acquireCancelled  atomic.Int64
	acquirePoolClosed atomic.Int64

	// failedWaitTime is the uncensored counterpart of WaitTime: how long
	// acquisitions that ultimately failed spent waiting. Without it the mean
	// wait is computed only over survivors, which understates the wait
	// precisely when waits are worst.
	failedWaitTime  atomic.Int64
	failedWaitCount atomic.Int64

	// ---- stage latencies (Part C) --------------------------------------
	//
	// notifyToResume covers the handoff transit: the returner has written
	// the connection into the waiter and released its semaphore, and we
	// measure until the waiter is running again. It contains no pool work at
	// all, only semaphore wakeup and Go scheduler latency, which is what
	// makes it the discriminator for H2.
	notifyToResumeTime  atomic.Int64
	notifyToResumeCount atomic.Int64

	// waitToBorrow covers recordWait -> borrowed++, the window in which an
	// acquisition has already been counted as a successful wait but is not
	// yet counted in InUse. It is the discriminator for H3.
	waitToBorrowTime  atomic.Int64
	waitToBorrowCount atomic.Int64

	// The MySQL round trips that can occur inside that window. If these are
	// zero then the window is a nil check and H3 has no mechanism, which is
	// why they are counted separately rather than assumed.
	settingResetTime   atomic.Int64
	settingResetCount  atomic.Int64
	settingResetFailed atomic.Int64
	settingApplyTime   atomic.Int64
	settingApplyCount  atomic.Int64

	// waitSuccessNotBorrowed counts acquisitions that recorded a successful
	// wait and then failed before reaching borrowed++. These are the only
	// acquisitions that can make WaitCount and InUse disagree for a reason
	// other than short hold times, so a zero here rules that reason out.
	waitSuccessNotBorrowed atomic.Int64

	// ---- wl.mu work by operation class (Part E) ------------------------
	//
	// Counted by class because the classes have different complexity and
	// only one of them is on the cancellation path. elementsExamined is the
	// work actually done under the lock; comparing it against the operation
	// count shows whether an operation is O(1) or O(n) in production without
	// timing anything.
	wlSelfRemovalOps     atomic.Int64
	wlReturnSelectionOps atomic.Int64
	wlStarvationScanOps  atomic.Int64
	wlElementsExamined   atomic.Int64
	wlLenAtOpSum         atomic.Int64

	// Sampled hold time. Timing every acquisition of a mutex that serializes
	// the whole pool would itself be a contention source, so this is sampled
	// at wlSampleRate and the sum is only meaningful against
	// wlHoldSampleCount.
	wlHoldTimeSampled atomic.Int64
	wlHoldSampleCount atomic.Int64
	wlSampleCounter   atomic.Int64

	// expiredEvicted counts waiters removed by a returner because their
	// acquisition context had already expired when they were examined.
	expiredEvicted atomic.Int64
}

// wlSampleRate is the sampling divisor for wl.mu hold timing. One in this many
// return-selection operations is timed. Two monotonic clock reads inside the
// pool's central mutex are affordable occasionally and are not affordable on
// every return.
const wlSampleRate = 1024

func (m *StageMetrics) enqueueWaiter() {
	m.waitEnqueued.Add(1)
	cur := m.waitersCurrent.Add(1)
	for {
		peak := m.waitersPeak.Load()
		if cur <= peak || m.waitersPeak.CompareAndSwap(peak, cur) {
			return
		}
	}
}

func (m *StageMetrics) dequeueWaiter() {
	m.waitDequeued.Add(1)
	m.waitersCurrent.Add(-1)
}

func (m *StageMetrics) recordNotifyToResume(d time.Duration) {
	m.notifyToResumeTime.Add(d.Nanoseconds())
	m.notifyToResumeCount.Add(1)
}

func (m *StageMetrics) recordWaitToBorrow(d time.Duration) {
	m.waitToBorrowTime.Add(d.Nanoseconds())
	m.waitToBorrowCount.Add(1)
}

func (m *StageMetrics) recordFailedWait(d time.Duration) {
	m.failedWaitTime.Add(d.Nanoseconds())
	m.failedWaitCount.Add(1)
}

// sampleWlHold reports whether this wl.mu operation should be timed.
func (m *StageMetrics) sampleWlHold() bool {
	return m.wlSampleCounter.Add(1)%wlSampleRate == 0
}

func (m *StageMetrics) recordWlHold(d time.Duration) {
	m.wlHoldTimeSampled.Add(d.Nanoseconds())
	m.wlHoldSampleCount.Add(1)
}

// ---- accessors -----------------------------------------------------------

// WaitersCurrent is the number of callers blocked in waitForConn right now,
// including any that have been selected but have not yet resumed.
func (m *StageMetrics) WaitersCurrent() int64 { return m.waitersCurrent.Load() }

// WaitersPeak is the high-water mark of WaitersCurrent.
func (m *StageMetrics) WaitersPeak() int64 { return m.waitersPeak.Load() }

func (m *StageMetrics) WaitEnqueued() int64 { return m.waitEnqueued.Load() }
func (m *StageMetrics) WaitDequeued() int64 { return m.waitDequeued.Load() }

// AcquireWaited is every acquisition that had to block, successful or not.
func (m *StageMetrics) AcquireWaited() int64     { return m.acquireWaited.Load() }
func (m *StageMetrics) AcquireSuccess() int64    { return m.acquireSuccess.Load() }
func (m *StageMetrics) AcquireTimedOut() int64   { return m.acquireTimedOut.Load() }
func (m *StageMetrics) AcquireCancelled() int64  { return m.acquireCancelled.Load() }
func (m *StageMetrics) AcquirePoolClosed() int64 { return m.acquirePoolClosed.Load() }

// FailedWaitTime is the time spent waiting by acquisitions that then failed.
// WaitTime covers only the ones that succeeded.
func (m *StageMetrics) FailedWaitTime() time.Duration {
	return time.Duration(m.failedWaitTime.Load())
}
func (m *StageMetrics) FailedWaitCount() int64 { return m.failedWaitCount.Load() }

func (m *StageMetrics) NotifyToResumeTime() time.Duration {
	return time.Duration(m.notifyToResumeTime.Load())
}
func (m *StageMetrics) NotifyToResumeCount() int64 { return m.notifyToResumeCount.Load() }

func (m *StageMetrics) WaitToBorrowTime() time.Duration {
	return time.Duration(m.waitToBorrowTime.Load())
}
func (m *StageMetrics) WaitToBorrowCount() int64 { return m.waitToBorrowCount.Load() }

func (m *StageMetrics) SettingResetTime() time.Duration {
	return time.Duration(m.settingResetTime.Load())
}
func (m *StageMetrics) SettingResetCount() int64  { return m.settingResetCount.Load() }
func (m *StageMetrics) SettingResetFailed() int64 { return m.settingResetFailed.Load() }
func (m *StageMetrics) SettingApplyTime() time.Duration {
	return time.Duration(m.settingApplyTime.Load())
}
func (m *StageMetrics) SettingApplyCount() int64 { return m.settingApplyCount.Load() }

// WaitSuccessNotBorrowed counts acquisitions that recorded a successful wait
// and then failed before being counted in InUse.
func (m *StageMetrics) WaitSuccessNotBorrowed() int64 { return m.waitSuccessNotBorrowed.Load() }

func (m *StageMetrics) WlSelfRemovalOps() int64     { return m.wlSelfRemovalOps.Load() }
func (m *StageMetrics) WlReturnSelectionOps() int64 { return m.wlReturnSelectionOps.Load() }
func (m *StageMetrics) WlStarvationScanOps() int64  { return m.wlStarvationScanOps.Load() }
func (m *StageMetrics) WlElementsExamined() int64   { return m.wlElementsExamined.Load() }
func (m *StageMetrics) WlLenAtOpSum() int64         { return m.wlLenAtOpSum.Load() }
func (m *StageMetrics) WlHoldTimeSampled() time.Duration {
	return time.Duration(m.wlHoldTimeSampled.Load())
}
func (m *StageMetrics) WlHoldSampleCount() int64 { return m.wlHoldSampleCount.Load() }

// ExpiredEvicted counts waiters a returner removed because their acquisition
// context had already expired when they were examined.
func (m *StageMetrics) ExpiredEvicted() int64 { return m.expiredEvicted.Load() }
