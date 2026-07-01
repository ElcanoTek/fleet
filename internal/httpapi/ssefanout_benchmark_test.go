package httpapi

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// BenchmarkSSEFanOut measures the per-event fan-out cost of the turn buffer —
// the hot path when one streaming turn is watched by N concurrent SSE
// subscribers (#296). It attaches N drainers, then times Emit across the fan-out
// width. Pure in-process (no server/DB); always runs.
func BenchmarkSSEFanOut(b *testing.B) {
	for _, n := range []int{1, 10, 100} {
		b.Run(fmt.Sprintf("subscribers=%d", n), func(b *testing.B) {
			buf := newTurnBuffer("conv-bench", "turn-bench")
			ctx, cancel := context.WithCancel(context.Background())

			var wg sync.WaitGroup
			for i := 0; i < n; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					// Attach blocks, draining events to its recorder until Finish/cancel.
					_ = buf.Attach(ctx, 0, newRecorder(), nil)
				}()
			}
			// Barrier: wait until all N subscribers are attached before timing.
			deadline := time.Now().Add(5 * time.Second)
			for buf.subscriberCount() < n {
				if time.Now().After(deadline) {
					b.Fatalf("only %d/%d subscribers attached", buf.subscriberCount(), n)
				}
				time.Sleep(time.Millisecond)
			}

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				buf.Emit("text.delta", map[string]any{"text": "chunk"})
			}
			b.StopTimer()

			buf.Finish()
			cancel()
			wg.Wait()
		})
	}
}
