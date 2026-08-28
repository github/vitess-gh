package smartconnpool

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestWaiterObservability asserts that the new counters answer the question we
// could not answer during the incident: "did a returner attempt to hand a
// connection to an expired waiter?" -- without inferring it from aggregate
// acquire latency.
func TestWaiterObservability(t *testing.T) {
	const acquireTimeout = 100 * time.Millisecond

	state := &TestState{}
	p := NewPool(&Config[*TestConn]{Capacity: 1, IdleTimeout: time.Hour}).
		Open(newConnector(state), nil)
	defer p.Close()

	held, err := p.Get(context.Background(), nil)
	require.NoError(t, err)

	require.Zero(t, p.Waiters(), "no waiters before the test starts")
	require.Zero(t, p.ExpiredHandoffs())
	require.Zero(t, p.WaitersEvicted())

	type result struct{ err error }
	resc := make(chan result, 1)
	go func() {
		ctx := &expiredCtx{deadline: time.Now().Add(acquireTimeout)}
		_, err := p.Get(ctx, nil)
		resc <- result{err: err}
	}()

	for p.wait.waiting() < 1 {
		time.Sleep(200 * time.Microsecond)
	}
	require.EqualValues(t, 1, p.Waiters(), "Waiters gauge must observe the blocked client")

	time.Sleep(2 * acquireTimeout) // waiter is now expired but still linked

	waitsBefore := p.Metrics.WaitCount()
	held.Recycle()
	r := <-resc

	require.Error(t, r.err, "expired waiter must not receive a connection")
	require.EqualValues(t, 1, p.ExpiredHandoffs(),
		"a returner attempted to hand a connection to an expired waiter")
	require.EqualValues(t, 1, p.WaitersEvicted(),
		"the expired waiter was evicted from the waitlist")
	require.Zero(t, p.Waiters(), "gauge returns to zero after eviction")
	require.Equal(t, waitsBefore, p.Metrics.WaitCount(),
		"success-only wait metric semantics must be preserved")

	t.Logf("Waiters=%d ExpiredHandoffs=%d WaitersEvicted=%d WaitCount=%d",
		p.Waiters(), p.ExpiredHandoffs(), p.WaitersEvicted(), p.Metrics.WaitCount())
}
