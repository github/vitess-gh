package smartconnpool

// Measures the cost of the tryReturnConnSlow critical section, so the exact
// ExpiredHandoffs reconstruction can be shown not to widen it materially.
// The scan is forced to traverse every waiter (no Setting match, no ageing
// match), which is the worst case for the added accounting.

import (
	"context"
	"fmt"
	"testing"
)

func benchWaitlist(n int) *waitlist[*TestConn] {
	wl := &waitlist[*TestConn]{}
	wl.init()
	for i := 0; i < n; i++ {
		wl.list.PushBack(waiter[*TestConn]{
			setting: &Setting{}, // never equal to the returned conn's nil Setting
			ctx:     context.Background(),
		})
	}
	return wl
}

func BenchmarkTryReturnConnSlow(b *testing.B) {
	for _, n := range []int{1, 8, 64, 512} {
		b.Run(fmt.Sprintf("waiters=%d", n), func(b *testing.B) {
			conn := &Pooled[*TestConn]{Conn: &TestConn{}}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				// Rebuild outside the timer: tryReturnConnSlow ages the waiters
				// it skips, so a reused list would trip age > maxAge after a few
				// iterations and break out of the scan immediately, silently
				// measuring a 1-element scan instead of an n-element one.
				b.StopTimer()
				wl := benchWaitlist(n)
				b.StartTimer()
				wl.tryReturnConnSlow(conn)
			}
		})
	}
}
