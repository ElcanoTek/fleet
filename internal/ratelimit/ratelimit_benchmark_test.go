package ratelimit

import (
	"fmt"
	"testing"
)

// BenchmarkAllow exercises the steady-state hot path of a single key repeatedly
// hitting the limiter. The dominant cost is dropBefore's slice trim, which is
// why this benchmark exists — see the docstring on dropBefore for the
// in-place-vs-allocate tradeoff this number is meant to detect regressions
// against.
func BenchmarkAllow(b *testing.B) {
	l := New(40, 2000)
	const key = "u@x.com"

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		l.Allow(key)
	}
}

// BenchmarkAllow_ManyKeys exercises the map-lookup path with realistic per-key
// buckets so the trim cost isn't measured in isolation from the surrounding work
// the limiter actually does.
func BenchmarkAllow_ManyKeys(b *testing.B) {
	l := New(40, 2000)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		key := fmt.Sprintf("user%d@example.com", i%1000)
		l.Allow(key)
	}
}

func BenchmarkAllow_Parallel(b *testing.B) {
	l := New(40, 2000)

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			key := fmt.Sprintf("user%d@example.com", i%1000)
			l.Allow(key)
			i++
		}
	})
}
