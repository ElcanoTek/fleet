package httpapi

import (
	"fmt"
	"testing"
)

// BenchmarkRateLimiterAllow exercises the steady-state hot path of a
// single user repeatedly hitting the limiter. The dominant cost is
// dropBefore's slice trim, which is why this benchmark exists — see the
// docstring on dropBefore for the in-place-vs-allocate tradeoff this
// number is meant to detect regressions against.
func BenchmarkRateLimiterAllow(b *testing.B) {
	r := newRateLimiter(40, 2000)
	const email = "u@x.com"

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r.allow(email)
	}
}

// BenchmarkRateLimiterAllow_ManyUsers exercises the map-lookup path with
// realistic per-user buckets so the trim cost isn't measured in
// isolation from the surrounding work the limiter actually does.
func BenchmarkRateLimiterAllow_ManyUsers(b *testing.B) {
	r := newRateLimiter(40, 2000)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		email := fmt.Sprintf("user%d@example.com", i%1000)
		r.allow(email)
	}
}

func BenchmarkRateLimiterAllow_Parallel(b *testing.B) {
	r := newRateLimiter(40, 2000)

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			email := fmt.Sprintf("user%d@example.com", i%1000)
			r.allow(email)
			i++
		}
	})
}
