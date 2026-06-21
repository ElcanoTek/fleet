package handlers

import (
	"fmt"
	"testing"
	"time"
)

func BenchmarkRateLimiter_Allow_LargeMap(b *testing.B) {
	// Setup a rate limiter
	rl := newRateLimiter(10, time.Minute)

	// Fill with FRESH entries so they are not deleted during cleanup check
	// This forces the cleanup loop to iterate but not delete, maintaining map size.
	now := time.Now()
	for i := 0; i < 100000; i++ {
		ip := fmt.Sprintf("ip-%d", i)
		rl.requests[ip] = []time.Time{now}
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Use a distinct IP for the request to avoid hitting the limit of the test IPs
		// checks allow for "requesting-ip"
		rl.Allow("requesting-ip")
	}
}
