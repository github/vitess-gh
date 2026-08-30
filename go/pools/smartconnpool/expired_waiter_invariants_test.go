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
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vitess.io/vitess/go/list"
	vtrpcpb "vitess.io/vitess/go/vt/proto/vtrpc"
	"vitess.io/vitess/go/vt/vterrors"
)

// TestEvictionPreservesConnectionLifecycle asserts that evicting expired
// waiters is purely a routing decision: the returned connection is handed to a
// live waiter untouched, so the pool performs no extra dial, no close, and its
// active count does not move.
func TestEvictionPreservesConnectionLifecycle(t *testing.T) {
	var state TestState

	p := NewPool(&Config[*TestConn]{
		Capacity: 1,
		LogWait:  state.LogWait,
	}).Open(newConnector(&state), nil)
	defer p.Close()

	held, err := p.Get(context.Background(), nil)
	require.NoError(t, err)
	heldNum := held.Conn.num

	deadline := time.Now().Add(time.Second)

	// Three expired waiters ahead of the single live waiter, so the returner
	// has to scan past an expired prefix to reach the live target.
	kinds := []bool{true, true, true, false}

	type outcome struct {
		expired bool
		gotConn bool
		num     int64
	}
	results := make([]outcome, len(kinds))

	var (
		wg sync.WaitGroup
		mu sync.Mutex
	)
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
				mu.Lock()
				results[i].gotConn = true
				results[i].num = conn.Conn.num
				mu.Unlock()
				p.put(conn)
			}
		}()

		require.Eventuallyf(t, func() bool {
			return p.wait.waiting() == before+1
		}, 5*time.Second, time.Millisecond, "waiter %d never enqueued", i)
	}

	time.Sleep(time.Until(deadline) + 100*time.Millisecond)

	// Snapshot the lifecycle counters immediately before the handoff.
	opensBefore := state.open.Load()
	closesBefore := state.close.Load()
	activeBefore := p.Active()
	capacityBefore := p.Capacity()

	p.put(held)

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("waiters did not finish; connection handoff stalled")
	}

	mu.Lock()
	defer mu.Unlock()

	// The connection must have gone to the live waiter and to nobody else.
	// Without this the lifecycle assertions below would also hold on the
	// pre-fix code, which satisfies them by handing the connection to an
	// expired waiter that then returns it.
	for i, r := range results {
		if r.expired {
			require.Falsef(t, r.gotConn,
				"expired waiter %d received a connection; its acquisition had already expired", i)
			continue
		}
		require.Truef(t, r.gotConn, "live waiter %d was starved of a connection", i)
		assert.Equalf(t, heldNum, r.num,
			"live waiter %d must receive the very same physical connection that was returned", i)
	}

	assert.Equal(t, opensBefore, state.open.Load(),
		"evicting expired waiters must not trigger a new dial")
	assert.Equal(t, closesBefore, state.close.Load(),
		"evicting expired waiters must not close the returned connection")
	assert.Equal(t, activeBefore, p.Active(),
		"Active must be unchanged: eviction does not create or destroy connections")
	assert.Equal(t, capacityBefore, p.Capacity(), "Capacity must be unchanged")

	// The connection survived the eviction path and the pool is still usable.
	reused, err := p.Get(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, heldNum, reused.Conn.num, "the connection must remain reusable")
	assert.False(t, reused.Conn.IsClosed(), "the connection must not have been closed")
	p.put(reused)
}

// TestEvictedWaiterErrorIdentity asserts that a waiter evicted because its
// context had already expired observes exactly the same error as a waiter that
// times out on its own, so the client-visible error classification is
// unchanged by this fix.
func TestEvictedWaiterErrorIdentity(t *testing.T) {
	// Control: a waiter that reaches its own deadline with nobody returning a
	// connection. This is the pre-existing timeout path.
	t.Run("control_self_timeout", func(t *testing.T) {
		var state TestState
		p := NewPool(&Config[*TestConn]{
			Capacity: 1,
			LogWait:  state.LogWait,
		}).Open(newConnector(&state), nil)
		defer p.Close()

		held, err := p.Get(context.Background(), nil)
		require.NoError(t, err)
		defer p.put(held)

		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()

		conn, err := p.Get(ctx, nil)
		require.Nil(t, conn)
		assertIsPoolTimeout(t, err)
	})

	// Subject: a waiter whose context expires while parked and which is then
	// evicted by the returner. It must be indistinguishable from the control.
	t.Run("evicted_by_returner", func(t *testing.T) {
		var state TestState
		p := NewPool(&Config[*TestConn]{
			Capacity: 1,
			LogWait:  state.LogWait,
		}).Open(newConnector(&state), nil)
		defer p.Close()

		held, err := p.Get(context.Background(), nil)
		require.NoError(t, err)

		deadline := time.Now().Add(500 * time.Millisecond)

		var (
			wg      sync.WaitGroup
			mu      sync.Mutex
			gotErr  error
			gotConn *Pooled[*TestConn]
		)
		before := p.wait.waiting()
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, err := p.Get(newLateExpiryCtx(deadline), nil)
			mu.Lock()
			gotConn, gotErr = conn, err
			mu.Unlock()
		}()
		require.Eventually(t, func() bool {
			return p.wait.waiting() == before+1
		}, 5*time.Second, time.Millisecond, "waiter never enqueued")

		time.Sleep(time.Until(deadline) + 100*time.Millisecond)
		p.put(held)
		wg.Wait()

		mu.Lock()
		defer mu.Unlock()
		require.Nil(t, gotConn, "an evicted waiter must not receive a connection")
		assertIsPoolTimeout(t, gotErr)
	})
}

func assertIsPoolTimeout(t *testing.T, err error) {
	t.Helper()

	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrTimeout), "want ErrTimeout, got %v", err)
	assert.Same(t, ErrTimeout, err, "the caller must observe the ErrTimeout sentinel itself")
	assert.Equal(t, vtrpcpb.Code_RESOURCE_EXHAUSTED, vterrors.Code(err),
		"the vtrpc classification the client sees must stay RESOURCE_EXHAUSTED")
}

// pristineTarget reproduces the pre-fix selection rule for a list that
// contains no expired waiters, and reports the index of the chosen waiter plus
// the ages the pre-fix code would have left behind.
//
//	target = list.Front()
//	for e := target; e != nil; e = e.Next() {
//	    if e.age > maxAge || e.setting == connSetting { target = e; break }
//	    e.age++
//	}
func pristineTarget(settings []*Setting, ages []uint32, connSetting *Setting) (int, []uint32) {
	const maxAge = 8

	finalAges := append([]uint32(nil), ages...)
	if len(settings) == 0 {
		return -1, finalAges
	}

	target := 0
	for i := range settings {
		if finalAges[i] > maxAge || settings[i] == connSetting {
			target = i
			break
		}
		finalAges[i]++
	}
	return target, finalAges
}

// TestLiveWaiterSelectionMatchesPristine proves that when no waiter has an
// expired context the fix is a behavioural no-op: it selects the same waiter
// and leaves the same ages behind as the pre-fix code. This is what preserves
// FIFO ordering, the Setting preference and the anti-starvation ageing.
func TestLiveWaiterSelectionMatchesPristine(t *testing.T) {
	s1 := NewSetting("s1", "")
	s2 := NewSetting("s2", "")

	settingChoices := []*Setting{nil, s1, s2}
	ageChoices := []uint32{0, 9} // straddles maxAge = 8

	var shapes int
	for length := 1; length <= 4; length++ {
		settingIdx := make([]int, length)
		ageIdx := make([]int, length)

		for {
			settings := make([]*Setting, length)
			ages := make([]uint32, length)
			for i := range settings {
				settings[i] = settingChoices[settingIdx[i]]
				ages[i] = ageChoices[ageIdx[i]]
			}

			for _, connSetting := range settingChoices {
				shapes++
				name := fmt.Sprintf("len%d/%v/%v/conn=%v", length, settingIdx, ageIdx, connSetting)
				gotIdx, gotAges := runLiveSelection(t, settings, ages, connSetting)
				wantIdx, wantAges := pristineTarget(settings, ages, connSetting)

				require.Equalf(t, wantIdx, gotIdx, "%s: selected a different waiter than the pre-fix rule", name)
				require.Equalf(t, wantAges, gotAges, "%s: left different ages than the pre-fix rule", name)
			}

			if !nextOdometer(settingIdx, len(settingChoices), ageIdx, len(ageChoices)) {
				break
			}
		}
	}
	t.Logf("verified %d all-live waitlist shapes", shapes)
}

// runLiveSelection builds a waitlist of live waiters, runs the return path over
// it once and reports which waiter was handed the connection along with the
// ages left on the list.
func runLiveSelection(t *testing.T, settings []*Setting, ages []uint32, connSetting *Setting) (int, []uint32) {
	t.Helper()

	var wl waitlist[*TestConn]
	wl.init(&StageMetrics{})

	elems := make([]*list.Element[waiter[*TestConn]], len(settings))
	for i := range settings {
		elems[i] = wl.list.PushBack(waiter[*TestConn]{
			setting: settings[i],
			ctx:     context.Background(), // live: never expired
			age:     ages[i],
		})
	}

	conn := &Pooled[*TestConn]{Conn: &TestConn{setting: connSetting}}
	require.True(t, wl.tryReturnConnSlow(conn), "a list of live waiters must always accept the connection")

	selected := -1
	finalAges := make([]uint32, len(settings))
	for i, e := range elems {
		finalAges[i] = e.Value.age
		if e.Value.conn == conn {
			require.Equal(t, -1, selected, "at most one waiter may receive the connection")
			selected = i
		}
	}
	require.NotEqual(t, -1, selected, "some waiter must have received the connection")
	require.Equal(t, len(settings)-1, wl.waiting(), "exactly one waiter must be removed from the list")

	return selected, finalAges
}

// nextOdometer advances two parallel digit vectors over their radices and
// reports whether a new combination was produced.
func nextOdometer(a []int, radixA int, b []int, radixB int) bool {
	for i := len(a) - 1; i >= 0; i-- {
		a[i]++
		if a[i] < radixA {
			return true
		}
		a[i] = 0
	}
	for i := len(b) - 1; i >= 0; i-- {
		b[i]++
		if b[i] < radixB {
			return true
		}
		b[i] = 0
	}
	return false
}
