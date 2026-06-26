package mcpbroker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/agentcore"
)

// fakeBroker is the server-side agentcore.MCPBroker the Server wraps in tests.
// Configure text/isErr/err for the result; set release to make calls block (so a
// test can observe cancellation/concurrency); started is closed when the first
// call begins.
type fakeBroker struct {
	mu         sync.Mutex
	calls      int
	lastServer string
	lastTool   string
	lastArgs   map[string]any
	ctxErr     error

	text  string
	isErr bool
	err   error

	echoTagAsText bool          // return args["tag"] as the text (for correlation tests)
	release       chan struct{} // if non-nil, block until closed or ctx cancels
	started       chan struct{} // closed (once) when a call begins
	startedOnce   sync.Once

	// discovery doubles
	tools             []ToolDescriptor
	accounts          []string
	listErr           error
	lastAccountServer string
	lastBaseVars      []string
}

var (
	_ agentcore.MCPBroker = (*fakeBroker)(nil)
	_ Backend             = (*fakeBroker)(nil)
)

func (b *fakeBroker) CallMCP(ctx context.Context, server, tool string, args map[string]any) (string, bool, error) {
	b.mu.Lock()
	b.calls++
	b.lastServer, b.lastTool, b.lastArgs = server, tool, args
	b.mu.Unlock()

	if b.started != nil {
		b.startedOnce.Do(func() { close(b.started) })
	}
	if b.release != nil {
		select {
		case <-b.release:
		case <-ctx.Done():
			b.mu.Lock()
			b.ctxErr = ctx.Err()
			b.mu.Unlock()
			return "", false, ctx.Err()
		}
	}
	if b.echoTagAsText {
		return fmt.Sprint(args["tag"]), b.isErr, b.err
	}
	return b.text, b.isErr, b.err
}

func (b *fakeBroker) observedCtxErr() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.ctxErr
}

func (b *fakeBroker) ListTools(context.Context) ([]ToolDescriptor, error) {
	return b.tools, b.listErr
}

func (b *fakeBroker) ListAccounts(_ context.Context, server string, baseVars []string) ([]string, error) {
	b.mu.Lock()
	b.lastAccountServer = server
	b.lastBaseVars = baseVars
	b.mu.Unlock()
	return b.accounts, b.listErr
}

// loopback wires a Client and a Server over an in-memory net.Pipe (no real
// process), serving against broker. The returned cleanup closes both ends and
// waits for the server loop to exit.
func loopback(t *testing.T, backend Backend) *Client {
	t.Helper()
	cConn, sConn := net.Pipe()
	srv := NewServer(backend)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = srv.Serve(ctx, sConn); close(done) }()
	client := NewClient(cConn)
	t.Cleanup(func() {
		_ = client.Close()
		cancel()
		<-done
	})
	return client
}

func TestClientServer_CallRoundTrip(t *testing.T) {
	fake := &fakeBroker{text: "hello-result"}
	client := loopback(t, fake)

	text, isErr, err := client.CallMCP(context.Background(), "deal_sheet", "lookup", map[string]any{"q": "x"})
	if err != nil {
		t.Fatalf("CallMCP err: %v", err)
	}
	if text != "hello-result" || isErr {
		t.Fatalf("(text=%q, isErr=%v), want (hello-result, false)", text, isErr)
	}
	if fake.lastServer != "deal_sheet" || fake.lastTool != "lookup" || fake.lastArgs["q"] != "x" {
		t.Fatalf("server got (%q, %q, %v), want the forwarded server/tool/args", fake.lastServer, fake.lastTool, fake.lastArgs)
	}
}

func TestClientServer_IsErrorPassthrough(t *testing.T) {
	client := loopback(t, &fakeBroker{text: "tool said no", isErr: true})

	text, isErr, err := client.CallMCP(context.Background(), "s", "t", nil)
	if err != nil {
		t.Fatalf("a tool-level isError must NOT be a transport error, got err=%v", err)
	}
	if !isErr || text != "tool said no" {
		t.Fatalf("(text=%q, isErr=%v), want (tool said no, true)", text, isErr)
	}
}

func TestClientServer_TransportErrorBecomesGoError(t *testing.T) {
	client := loopback(t, &fakeBroker{err: errors.New("upstream down")})

	_, isErr, err := client.CallMCP(context.Background(), "s", "t", nil)
	if err == nil || isErr {
		t.Fatalf("(isErr=%v, err=%v), want a transport Go error and isErr=false", isErr, err)
	}
	if err.Error() != "upstream down" {
		t.Fatalf("err = %q, want the server-side error string round-tripped", err.Error())
	}
}

// TestClientServer_ConcurrentCallsCorrelate fires many calls at once, each tagged,
// and asserts each caller gets ITS OWN tag back — proving responses route by ID.
func TestClientServer_ConcurrentCallsCorrelate(t *testing.T) {
	client := loopback(t, &fakeBroker{echoTagAsText: true})

	const n = 24
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tag := fmt.Sprintf("tag-%d", i)
			text, _, err := client.CallMCP(context.Background(), "s", "t", map[string]any{"tag": tag})
			if err != nil {
				errs <- fmt.Errorf("call %d: %w", i, err)
				return
			}
			if text != tag {
				errs <- fmt.Errorf("call %d got %q, want its own tag %q (response mis-correlated)", i, text, tag)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

// TestClientServer_ContextCancelStopsCall: cancelling the caller's context returns
// promptly AND propagates to the server so the in-flight MCP call is cancelled.
func TestClientServer_ContextCancelStopsCall(t *testing.T) {
	fake := &fakeBroker{release: make(chan struct{}), started: make(chan struct{})}
	client := loopback(t, fake)
	defer close(fake.release) // unblock the server-side call on cleanup

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, _, err := client.CallMCP(ctx, "s", "t", nil)
		result <- err
	}()

	select {
	case <-fake.started:
	case <-time.After(2 * time.Second):
		t.Fatal("server-side call never started")
	}
	cancel()

	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("CallMCP returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("CallMCP did not return promptly after context cancellation")
	}

	// The cancel must have reached the server: poll until the fake observes it.
	deadline := time.Now().Add(2 * time.Second)
	for fake.observedCtxErr() == nil {
		if time.Now().After(deadline) {
			t.Fatal("server-side call's context was never cancelled (cancel did not propagate over the wire)")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestClient_Ping(t *testing.T) {
	client := loopback(t, &fakeBroker{})
	if err := client.Ping(context.Background()); err != nil {
		t.Fatalf("Ping err: %v", err)
	}
}

func TestClientServer_ListTools(t *testing.T) {
	client := loopback(t, &fakeBroker{tools: []ToolDescriptor{
		{Server: "deal_sheet", Tool: "lookup", Description: "d", InputSchema: map[string]any{"type": "object"}},
		{Server: "email", Tool: "send"},
	}})

	got, err := client.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(got) != 2 || got[0].Server != "deal_sheet" || got[0].Tool != "lookup" || got[1].Tool != "send" {
		t.Fatalf("ListTools = %+v, want the catalog round-tripped", got)
	}
	if got[0].InputSchema["type"] != "object" {
		t.Fatalf("InputSchema lost on the wire: %+v", got[0].InputSchema)
	}
}

func TestClientServer_ListAccounts(t *testing.T) {
	fake := &fakeBroker{accounts: []string{"client_a", "client_b"}}
	client := loopback(t, fake)

	got, err := client.ListAccounts(context.Background(), "magnite_mcp", []string{"MAGNITE_USERNAME", "MAGNITE_PASSWORD"})
	if err != nil {
		t.Fatalf("ListAccounts: %v", err)
	}
	if len(got) != 2 || got[0] != "client_a" || got[1] != "client_b" {
		t.Fatalf("ListAccounts = %v, want the names round-tripped", got)
	}
	// The server (broker) is the one that resolves accounts, so it must receive the
	// target server + base vars.
	if fake.lastAccountServer != "magnite_mcp" || len(fake.lastBaseVars) != 2 {
		t.Fatalf("broker got (server=%q, baseVars=%v), want them forwarded", fake.lastAccountServer, fake.lastBaseVars)
	}
}

// TestClient_ClosedFailsCalls: after Close, new calls fail fast; and a call
// outstanding when the connection dies returns an error rather than hanging.
func TestClient_ClosedFailsCalls(t *testing.T) {
	t.Run("call after close", func(t *testing.T) {
		client := loopback(t, &fakeBroker{text: "x"})
		_ = client.Close()
		if _, _, err := client.CallMCP(context.Background(), "s", "t", nil); err == nil {
			t.Fatal("CallMCP after Close should fail")
		}
	})

	t.Run("outstanding call when conn dies", func(t *testing.T) {
		fake := &fakeBroker{release: make(chan struct{}), started: make(chan struct{})}
		defer close(fake.release)
		client := loopback(t, fake)

		result := make(chan error, 1)
		go func() {
			_, _, err := client.CallMCP(context.Background(), "s", "t", nil)
			result <- err
		}()
		<-fake.started
		_ = client.Close() // kill the connection mid-call

		select {
		case err := <-result:
			if err == nil {
				t.Fatal("an outstanding call should error when the client closes")
			}
		case <-time.After(2 * time.Second):
			t.Fatal("outstanding call hung after Close")
		}
	})
}

// TestServer_UnknownMethod drives the server's default branch directly (the Client
// never sends an unknown method): an unrecognized method gets an error response.
func TestServer_UnknownMethod(t *testing.T) {
	cConn, sConn := net.Pipe()
	srv := NewServer(&fakeBroker{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { _ = srv.Serve(ctx, sConn); close(done) }()
	defer func() { _ = cConn.Close(); <-done }()

	// Hand-encode a bogus-method request and read the raw response.
	if err := json.NewEncoder(cConn).Encode(request{ID: 7, Method: "bogus"}); err != nil {
		t.Fatalf("encode: %v", err)
	}
	var resp response
	if err := json.NewDecoder(cConn).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ID != 7 || resp.Err == "" {
		t.Fatalf("resp = %+v, want ID 7 with a non-empty Err for the unknown method", resp)
	}
}
