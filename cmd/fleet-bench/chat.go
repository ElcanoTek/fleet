package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// chatOptions holds the resolved chat-load configuration.
type chatOptions struct {
	server      string
	email       string
	token       string
	message     string
	concurrency int
	duration    time.Duration
	timeout     time.Duration
}

// runChat parses flags, resolves connection config, and runs the concurrent
// chat-turn load, printing a report.
func runChat(argv []string) error {
	fs := flag.NewFlagSet("chat", flag.ContinueOnError)
	server := fs.String("server", "", "chat server base URL (default $FLEET_CHAT_URL, else http://$FLEET_SERVER_ADDR, else http://127.0.0.1:8080)")
	email := fs.String("email", "", "authenticated user email sent as X-User-Email (default $FLEET_BENCH_EMAIL / $E2E_TEST_EMAIL)")
	tokenEnv := fs.String("token-env", "FLEET_SERVER_TOKEN", "env var holding the shared server token (X-Chat-Server-Token) — never passed on argv")
	message := fs.String("message", "[[echo:load-test reply]] respond briefly", "message body sent each turn (use an [[echo:…]] / [[scenario:…]] marker for a deterministic fake-LLM reply)")
	concurrency := fs.Int("concurrency", 10, "number of concurrent workers")
	duration := fs.Duration("duration", 30*time.Second, "how long to sustain the load")
	timeout := fs.Duration("timeout", 120*time.Second, "per-turn timeout")
	if err := fs.Parse(argv); err != nil {
		return err
	}

	opts := chatOptions{
		server:      resolveServer(*server),
		email:       resolveEmail(*email),
		token:       strings.TrimSpace(os.Getenv(*tokenEnv)),
		message:     *message,
		concurrency: *concurrency,
		duration:    *duration,
		timeout:     *timeout,
	}
	if opts.token == "" {
		return fmt.Errorf("server token not set: export %s (the shared X-Chat-Server-Token)", *tokenEnv)
	}
	if opts.email == "" {
		return fmt.Errorf("user email required: pass --email or set FLEET_BENCH_EMAIL / E2E_TEST_EMAIL")
	}
	if opts.concurrency < 1 {
		return fmt.Errorf("--concurrency must be >= 1")
	}

	report := runChatLoad(opts)
	report.print(os.Stdout, opts)
	return nil
}

func resolveServer(flagVal string) string {
	if v := strings.TrimSpace(flagVal); v != "" {
		return strings.TrimRight(v, "/")
	}
	if v := strings.TrimSpace(os.Getenv("FLEET_CHAT_URL")); v != "" {
		return strings.TrimRight(v, "/")
	}
	if v := strings.TrimSpace(os.Getenv("FLEET_SERVER_ADDR")); v != "" {
		if !strings.HasPrefix(v, "http") {
			if strings.HasPrefix(v, ":") {
				v = "127.0.0.1" + v
			}
			v = "http://" + v
		}
		return strings.TrimRight(v, "/")
	}
	return "http://127.0.0.1:8080"
}

func resolveEmail(flagVal string) string {
	for _, v := range []string{flagVal, os.Getenv("FLEET_BENCH_EMAIL"), os.Getenv("E2E_TEST_EMAIL")} {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

// chatReport aggregates the outcome of a chat-load run.
type chatReport struct {
	latencies  []time.Duration
	turns      int64
	errors     int64
	wall       time.Duration
	goroutines int // net growth (after - before)
	heapGrowth int64
}

// runChatLoad spawns opts.concurrency workers, each posting chat turns and
// reading the SSE stream to a terminal event for opts.duration, and returns the
// aggregated report. Goroutine + heap deltas are sampled before/after (a
// monotonic climb across runs signals a per-turn leak).
func runChatLoad(opts chatOptions) chatReport {
	var memBefore runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&memBefore)
	goroutinesBefore := runtime.NumGoroutine()

	client := &http.Client{Timeout: opts.timeout}
	ctx, cancel := context.WithTimeout(context.Background(), opts.duration)
	defer cancel()

	var (
		mu        sync.Mutex
		latencies []time.Duration
		turns     atomic.Int64
		errs      atomic.Int64
		wg        sync.WaitGroup
	)
	start := time.Now()
	for i := 0; i < opts.concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				lat, err := oneChatTurn(ctx, client, opts)
				// A turn cut short by the duration deadline (ctx expired mid-turn)
				// is the graceful stop, not a failure — don't count it as an error.
				if ctx.Err() != nil {
					break
				}
				if err != nil {
					errs.Add(1)
					continue
				}
				turns.Add(1)
				mu.Lock()
				latencies = append(latencies, lat)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	wall := time.Since(start)

	runtime.GC()
	var memAfter runtime.MemStats
	runtime.ReadMemStats(&memAfter)

	return chatReport{
		latencies:  latencies,
		turns:      turns.Load(),
		errors:     errs.Load(),
		wall:       wall,
		goroutines: runtime.NumGoroutine() - goroutinesBefore,
		heapGrowth: int64(memAfter.HeapAlloc) - int64(memBefore.HeapAlloc),
	}
}

// oneChatTurn posts a single chat turn and reads its SSE stream to the terminal
// event, returning the full turn latency.
func oneChatTurn(ctx context.Context, client *http.Client, opts chatOptions) (time.Duration, error) {
	body, _ := json.Marshal(map[string]any{
		"conversation_id": "", // new conversation each turn
		"message":         opts.message,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, opts.server+"/chat", bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Chat-Server-Token", opts.token)
	req.Header.Set("X-User-Email", opts.email)

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("POST /chat: status %d", resp.StatusCode)
	}
	event, err := readTurnToTerminal(resp.Body)
	if err != nil {
		return 0, fmt.Errorf("read SSE: %w", err)
	}
	if event == "turn.error" {
		return 0, fmt.Errorf("turn ended in error")
	}
	return time.Since(start), nil
}

func (r chatReport) print(w io.Writer, opts chatOptions) {
	tps := 0.0
	if r.wall > 0 {
		tps = float64(r.turns) / r.wall.Seconds()
	}
	fmt.Fprintf(w, "fleet-bench chat — %s, %d workers, %s\n", opts.server, opts.concurrency, opts.duration)
	fmt.Fprintf(w, "  turns:        %d ok, %d errors\n", r.turns, r.errors)
	fmt.Fprintf(w, "  throughput:   %.1f turns/sec (wall %s)\n", tps, r.wall.Round(time.Millisecond))
	fmt.Fprintf(w, "  latency p50:  %s\n", percentile(r.latencies, 50).Round(time.Millisecond))
	fmt.Fprintf(w, "  latency p95:  %s\n", percentile(r.latencies, 95).Round(time.Millisecond))
	fmt.Fprintf(w, "  latency p99:  %s\n", percentile(r.latencies, 99).Round(time.Millisecond))
	fmt.Fprintf(w, "  latency mean: %s\n", mean(r.latencies).Round(time.Millisecond))
	fmt.Fprintf(w, "  goroutine Δ:  %+d (net, after GC — a large positive Δ hints at a per-turn leak)\n", r.goroutines)
	fmt.Fprintf(w, "  heap Δ:       %+d bytes (after GC)\n", r.heapGrowth)
}
