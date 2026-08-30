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
)

// TestMassTimeoutAB is written to compile and run unchanged on the pristine
// tree and on the fixed tree, so the two can be compared directly. It uses no
// symbol that the fix introduces.
//
// It drives a mass timeout at a controlled waitlist depth: one connection is
// held, N callers queue behind it with a short deadline, and they all expire
// at roughly the same time. On pristine each expiring waiter scans the list to
// find itself, under the mutex that also serialises every return; with the fix
// it removes itself directly. The quantity reported is wall-clock time to
// drain, which is the only thing both trees can report.
//
// Measured result, and the reason no performance claim is made from it: drain
// time here is bounded below by the acquisition deadline, and across repeated
// runs at depths up to 64000 the median drain is the same on both trees. The
// pristine tree does show occasional large excursions that the fixed tree did
// not produce, but a handful of runs cannot establish a tail difference, so
// this harness must not be cited as evidence of a throughput improvement. The
// complexity change is established instead by the work actually done under the
// mutex, which TestProductionShapedStress reports as elements examined per
// waitlist operation.
func TestMassTimeoutAB(t *testing.T) {
	if os.Getenv("SMARTCONNPOOL_AB") == "" {
		t.Skip("set SMARTCONNPOOL_AB=1 to run the A/B harness")
	}

	depths := []int{8000, 16000, 32000, 64000}

	t.Logf("GOMAXPROCS=%d", runtime.GOMAXPROCS(0))
	t.Logf("%8s %12s %12s %10s", "depth", "drain", "per-waiter", "goroutines")

	for _, depth := range depths {
		var state TestState
		p := NewPool(&Config[*TestConn]{
			Capacity: 1,
			LogWait:  state.LogWait,
		}).Open(newConnector(&state), nil)

		ctx := context.Background()
		held, err := p.Get(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}

		var queued atomic.Int64
		var wg sync.WaitGroup
		start := make(chan struct{})

		for i := 0; i < depth; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				cctx, cancel := context.WithTimeout(ctx, 400*time.Millisecond)
				defer cancel()
				queued.Add(1)
				if c, err := p.Get(cctx, nil); err == nil {
					c.Recycle()
				}
			}()
		}

		close(start)

		// Wait until the waitlist is actually at depth before timing.
		deadline := time.Now().Add(10 * time.Second)
		for p.wait.waiting() < depth-1 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		reached := p.wait.waiting()

		gor := runtime.NumGoroutine()
		t0 := time.Now()
		wg.Wait()
		drain := time.Since(t0)

		held.Recycle()
		p.Close()

		t.Logf("%8d %12v %12v %10d  (reached depth %d)",
			depth, drain, drain/time.Duration(max(depth, 1)), gor, reached)
	}
}
