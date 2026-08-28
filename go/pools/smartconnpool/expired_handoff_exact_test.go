package smartconnpool

// Exactness tests for ExpiredHandoffs.
//
// The counter must satisfy exactly one property:
//
//	ExpiredHandoffs increments on a return iff the waiter that the pristine v21
//	selection rule would have chosen was expired.
//
// The pristine rule (865f789e15, waitlist.go tryReturnConnSlow) is:
//
//	target = list.Front()
//	for e := target; e != nil; e = e.Next() {
//	    if e.age > maxAge || e.setting == connSetting { target = e; break }
//	    e.age++
//	}
//
// i.e. the first waiter matching (age > maxAge || setting == connSetting),
// otherwise the front element. It had no notion of contexts, so an expired
// waiter could become the target through any of those three routes.
//
// The two named tests below are the counterexamples that falsified the previous
// "idx == 0 || setting == connSetting" heuristic.

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// legacyMaxAge mirrors the const in tryReturnConnSlow.
const legacyMaxAge = 8

type shape struct {
	expired bool
	age     uint32
	setting *Setting
}

// referencePristineTargetExpired is an INDEPENDENT implementation of the
// pristine rule, written directly from the 865f789e15 source. It deliberately
// shares no code with the production reconstruction.
func referencePristineTargetExpired(shapes []shape, connSetting *Setting) bool {
	if len(shapes) == 0 {
		return false
	}
	target := 0 // list.Front()
	for i := range shapes {
		if shapes[i].age > legacyMaxAge || shapes[i].setting == connSetting {
			target = i
			break
		}
	}
	return shapes[target].expired
}

// runShape drives one return against a synthetic waitlist and reports whether
// ExpiredHandoffs incremented.
func runShape(t *testing.T, shapes []shape, connSetting *Setting) bool {
	t.Helper()
	wl := &waitlist[*TestConn]{}
	wl.init()
	for _, s := range shapes {
		var ctx context.Context = context.Background()
		if s.expired {
			c, cancel := context.WithCancel(context.Background())
			cancel()
			ctx = c
		}
		wl.list.PushBack(waiter[*TestConn]{setting: s.setting, ctx: ctx, age: s.age})
	}

	conn := &Pooled[*TestConn]{Conn: &TestConn{setting: connSetting}}

	before := wl.preventedHandoffs.Load()
	wl.tryReturnConnSlow(conn)
	return wl.preventedHandoffs.Load()-before == 1
}

// TestExpiredHandoffUndercountAgeBranch is Agent 3 counterexample 1.
//
// A live waiter sits at the front, and an expired waiter behind it has
// age > maxAge. Under pristine the age branch makes the EXPIRED waiter the
// target, so this return would have handed a connection to an expired waiter.
// The previous heuristic missed it because the expired waiter was neither at
// idx 0 nor a Setting match.
func TestExpiredHandoffUndercountAgeBranch(t *testing.T) {
	s1, s2 := &Setting{}, &Setting{}
	shapes := []shape{
		{expired: false, age: 0, setting: s2},               // front, live, no match
		{expired: true, age: legacyMaxAge + 1, setting: s2}, // expired, wins on age
	}
	require.True(t, referencePristineTargetExpired(shapes, s1),
		"reference: pristine target must be the expired waiter")
	require.True(t, runShape(t, shapes, s1),
		"ExpiredHandoffs must count the age>maxAge route (undercount counterexample)")
}

// TestExpiredHandoffOvercountFrontNotTarget is Agent 3 counterexample 2.
//
// An expired waiter sits at the front but matches nothing, and a LIVE waiter
// behind it matches the returned connection's Setting. Under pristine the live
// waiter wins, so no bad handoff would have occurred. The previous heuristic
// incremented purely because the expired waiter was at idx 0.
func TestExpiredHandoffOvercountFrontNotTarget(t *testing.T) {
	s1, s2 := &Setting{}, &Setting{}
	shapes := []shape{
		{expired: true, age: 0, setting: s2},  // front, expired, but NOT the target
		{expired: false, age: 0, setting: s1}, // live, wins on Setting match
	}
	require.False(t, referencePristineTargetExpired(shapes, s1),
		"reference: pristine target must be the live Setting match")
	require.False(t, runShape(t, shapes, s1),
		"ExpiredHandoffs must not count when a live waiter would have won (overcount counterexample)")
}

// TestExpiredHandoffExhaustive proves exactness over every waitlist shape up to
// length 4, against the independent reference implementation.
func TestExpiredHandoffExhaustive(t *testing.T) {
	s1, s2 := &Setting{}, &Setting{}
	settings := []*Setting{nil, s1, s2}
	ages := []uint32{0, legacyMaxAge, legacyMaxAge + 1}
	connSettings := []*Setting{nil, s1, s2}

	var atoms []shape
	for _, exp := range []bool{false, true} {
		for _, a := range ages {
			for _, st := range settings {
				atoms = append(atoms, shape{expired: exp, age: a, setting: st})
			}
		}
	}

	var checked, mismatches int
	var build func(cur []shape)
	build = func(cur []shape) {
		if len(cur) > 0 {
			for _, cs := range connSettings {
				want := referencePristineTargetExpired(cur, cs)
				got := runShape(t, cur, cs)
				checked++
				if want != got {
					mismatches++
					if mismatches <= 5 {
						t.Errorf("shape %v connSetting=%v: want ExpiredHandoffs increment=%v, got %v",
							describe(cur), name(cs, s1, s2), want, got)
					}
				}
			}
		}
		if len(cur) == 4 {
			return
		}
		for _, a := range atoms {
			build(append(cur, a))
		}
	}
	build(nil)

	require.Zero(t, mismatches, "ExpiredHandoffs must be exact for every shape")
	t.Logf("exhaustive exactness: %d (shape, connSetting) cases, 0 mismatches", checked)
}

func name(s, s1, s2 *Setting) string {
	switch s {
	case nil:
		return "nil"
	case s1:
		return "S1"
	case s2:
		return "S2"
	}
	return "?"
}

func describe(shapes []shape) string {
	out := ""
	for _, s := range shapes {
		st := "nil"
		if s.setting != nil {
			st = "S"
		}
		out += fmt.Sprintf("[exp=%v age=%d set=%s]", s.expired, s.age, st)
	}
	return out
}
