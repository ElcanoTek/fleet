package handlers

import (
	"testing"
	"time"
)

func BenchmarkRateLimiter_Allow_Alloc(b *testing.B) {
	// Use a limit similar to production (e.g. 20 for login)
	limit := 20
	rl := newRateLimiter(limit, time.Minute)

	ip := "test-ip"

	// Fill the rate limiter to its capacity
	for i := 0; i < limit; i++ {
		rl.Allow(ip)
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		// Calling Allow when full will trigger the cleanup logic
		// which involves slice allocation in the unoptimized version.
		rl.Allow(ip)
	}
}
