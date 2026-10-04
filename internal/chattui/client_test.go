package chattui

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseSSE(t *testing.T) {
	stream := strings.Join([]string{
		": heartbeat comment (ignored)",
		"id: 1",
		"event: conversation",
		`data: {"id":"conv-123","title":"hi"}`,
		"",
		"event: text.delta",
		`data: {"text":"Hello "}`,
		"",
		"event: text.delta",
		`data: {"text":"world"}`,
		"",
		"event: tool.call",
		`data: {"name":"bash","id":"c1"}`,
		"",
		"event: turn.completed",
		`data: {"model":"x/y"}`,
		"", // trailing blank
	}, "\n")

	var got []Event
	if err := parseSSE(strings.NewReader(stream), func(e Event) { got = append(got, e) }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 {
		t.Fatalf("got %d events, want 5: %+v", len(got), got)
	}
	if got[0].Name != "conversation" || got[0].Str("id") != "conv-123" || got[0].ID != "1" {
		t.Errorf("conversation frame wrong: %+v", got[0])
	}
	if got[1].Str("text") != "Hello " || got[2].Str("text") != "world" {
		t.Errorf("text deltas wrong: %q %q", got[1].Str("text"), got[2].Str("text"))
	}
	if got[3].Name != "tool.call" || got[3].Str("name") != "bash" {
		t.Errorf("tool.call wrong: %+v", got[3])
	}
}

// fakeSSEServer streams a canned turn and records the auth headers it received.
func fakeSSEServer(t *testing.T, gotHeaders *http.Header) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*gotHeaders = r.Header.Clone()
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		write := func(s string) { _, _ = io.WriteString(w, s); fl.Flush() }
		write("event: conversation\ndata: {\"id\":\"conv-xyz\"}\n\n")
		write("event: tool.call\ndata: {\"name\":\"python\",\"id\":\"c1\"}\n\n")
		write("event: text.delta\ndata: {\"text\":\"the answer is 42\"}\n\n")
		write("event: turn.completed\ndata: {\"model\":\"m\"}\n\n")
	}))
}

func TestClientStream_SendsAuthAndStreams(t *testing.T) {
	var hdr http.Header
	srv := fakeSSEServer(t, &hdr)
	defer srv.Close()

	c := NewClient(Config{ServerURL: srv.URL, Email: "u@x.co", Token: "sekret"})
	var names []string
	convID, err := c.Stream(context.Background(), "what is 6*7?", "", func(e Event) {
		names = append(names, e.Name)
	})
	if err != nil {
		t.Fatal(err)
	}
	if hdr.Get("X-Chat-Server-Token") != "sekret" || hdr.Get("X-User-Email") != "u@x.co" {
		t.Errorf("auth headers not sent: token=%q email=%q", hdr.Get("X-Chat-Server-Token"), hdr.Get("X-User-Email"))
	}
	if hdr.Get("Accept") != "text/event-stream" {
		t.Errorf("Accept = %q, want text/event-stream", hdr.Get("Accept"))
	}
	if convID != "conv-xyz" {
		t.Errorf("convID = %q, want conv-xyz (from the conversation frame)", convID)
	}
	want := []string{"conversation", "tool.call", "text.delta", "turn.completed"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("events = %v, want %v", names, want)
	}
}

func TestRunOneShot_StreamsTextToStdout(t *testing.T) {
	var hdr http.Header
	srv := fakeSSEServer(t, &hdr)
	defer srv.Close()

	var out, errOut bytes.Buffer
	code := runOneShot(NewClient(Config{ServerURL: srv.URL, Email: "u@x.co", Token: "sekret"}), "", "what is 6*7?", strings.NewReader(""), &out, &errOut)
	if code != 0 {
		t.Fatalf("exit %d; stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "the answer is 42") {
		t.Errorf("stdout missing reply: %q", out.String())
	}
	if !strings.Contains(errOut.String(), "python") {
		t.Errorf("tool-call progress should go to stderr: %q", errOut.String())
	}
	if !strings.Contains(errOut.String(), "conversation: conv-xyz") {
		t.Errorf("conversation id should be reported on stderr for resuming: %q", errOut.String())
	}
	if strings.Contains(out.String(), "conv-xyz") {
		t.Errorf("conversation id must NOT leak into stdout (script capture): %q", out.String())
	}
}

func TestRunOneShot_TextReplaceDropsSupersededDraft(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: conversation\ndata: {\"id\":\"c\"}\n\n")
		io.WriteString(w, "event: text.delta\ndata: {\"text\":\"DRAFT_SHOULD_VANISH\"}\n\n")
		io.WriteString(w, "event: text.delta\ndata: {\"text\":\"final answer\"}\n\n")
		io.WriteString(w, "event: text.replace\ndata: {\"text\":\"final answer\"}\n\n")
		io.WriteString(w, "event: turn.completed\ndata: {}\n\n")
	}))
	defer srv.Close()
	var out, errOut bytes.Buffer
	if code := runOneShot(NewClient(Config{ServerURL: srv.URL}), "", "go", strings.NewReader(""), &out, &errOut); code != 0 {
		t.Fatal(code, errOut.String())
	}
	if strings.Contains(out.String(), "DRAFT_SHOULD_VANISH") {
		t.Fatalf("one-shot stdout kept the retracted draft: %q", out.String())
	}
	if !strings.Contains(out.String(), "final answer") {
		t.Fatalf("one-shot stdout missing final answer: %q", out.String())
	}
}

func TestClientStream_403ErrorRedactsToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer srv.Close()
	c := NewClient(Config{ServerURL: srv.URL, Email: "u@x.co", Token: "super-secret-token"})
	_, err := c.Stream(context.Background(), "hi", "", func(Event) {})
	if err == nil {
		t.Fatal("want error on 403")
	}
	if strings.Contains(err.Error(), "super-secret-token") {
		t.Errorf("error must NOT leak the token: %v", err)
	}
}

// A refusal body is quoted into the error (and `fleet acp` hands that to a
// client that may log it), so a proxy page echoing the request headers must
// not carry the token through. Every non-200 branch quotes the same redacted
// excerpt.
func TestClientStream_ErrorBodyRedactsEchoedToken(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{
			name:   "403 proxy debug page",
			status: http.StatusForbidden,
			body:   "<html><body><h1>403 Forbidden</h1><pre>X-Chat-Server-Token: super-secret-token\nAuthorization: Bearer super-secret-token</pre></body></html>",
		},
		{
			name:   "401 echoing the header",
			status: http.StatusUnauthorized,
			body:   "unauthorized: X-Chat-Server-Token: super-secret-token",
		},
		{
			name:   "502 echoing the header",
			status: http.StatusBadGateway,
			body:   "upstream refused; request had X-Chat-Server-Token: super-secret-token",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()
			c := NewClient(Config{ServerURL: srv.URL, Email: "u@x.co", Token: "super-secret-token"})
			_, err := c.Stream(context.Background(), "hi", "", func(Event) {})
			var se *StatusError
			if !errors.As(err, &se) || se.Code != tt.status {
				t.Fatalf("err = %#v, want *StatusError %d", err, tt.status)
			}
			if strings.Contains(err.Error(), "super-secret-token") || strings.Contains(err.Error(), "Bearer super-secret") {
				t.Errorf("error must NOT leak the token: %v", err)
			}
			if !strings.Contains(err.Error(), "X-Chat-Server-Token: [redacted]") {
				t.Errorf("error should quote the body with the token redacted: %v", err)
			}
		})
	}
}

// The body read stops at 512 bytes, so an echoed token can be cut in half
// there, where the whole-token match cannot see it. The token here starts at
// byte 500, leaving its first 12 bytes in the excerpt. The padding is spaces
// so the 403 branch's whitespace-collapsing excerpt pulls the cut token up
// next to the header name instead of past its 160-rune cap. The assertion is
// on token[:4]: every cut this body produces is at least that long, and "s",
// "su" and "sup" are too short to tell a leak apart from ordinary text.
func TestClientStream_ErrorBodyDropsTokenCutByReadCap(t *testing.T) {
	const token = "super-secret-token"
	header := "X-Chat-Server-Token:"
	straddling := header + strings.Repeat(" ", 500-len(header)) + token + "\n</pre></body></html>"
	tests := []struct {
		name     string
		status   int
		body     string
		want     string // a substring the error must still carry
		wantLeak bool   // a body under the cap is not cut, so its tail is kept
	}{
		{name: "403 token straddles the cap", status: http.StatusForbidden, body: straddling, want: header},
		{name: "502 token straddles the cap", status: http.StatusBadGateway, body: straddling, want: header},
		{
			name:     "short body ending in a token prefix is not trimmed",
			status:   http.StatusForbidden,
			body:     "request rejected for super-secret",
			want:     "request rejected for super-secret",
			wantLeak: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()
			_, err := NewClient(Config{ServerURL: srv.URL, Email: "u@x.co", Token: token}).Stream(context.Background(), "hi", "", func(Event) {})
			var se *StatusError
			if !errors.As(err, &se) || se.Code != tt.status {
				t.Fatalf("err = %#v, want *StatusError %d", err, tt.status)
			}
			if got := strings.Contains(err.Error(), token[:4]); got != tt.wantLeak {
				t.Errorf("error contains %q = %v, want %v: %v", token[:4], got, tt.wantLeak, err)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error should still quote %q: %v", tt.want, err)
			}
		})
	}
}

// Every cut length, down to a single byte, is dropped when the excerpt was
// truncated, and kept when it was not.
func TestRedactTokenDropsEveryCutLength(t *testing.T) {
	const token = "super-secret-token"
	for n := 1; n < len(token); n++ {
		in := "body: " + token[:n]
		if got := string(redactToken([]byte(in), token, true)); got != "body: " {
			t.Errorf("truncated, cut at %d: got %q, want %q", n, got, "body: ")
		}
		if got := string(redactToken([]byte(in), token, false)); got != in {
			t.Errorf("not truncated, cut at %d: got %q, want it unchanged", n, got)
		}
	}
	if got := string(redactToken([]byte("a "+token+" b"), "", true)); got != "a "+token+" b" {
		t.Errorf("empty token must leave the excerpt alone, got %q", got)
	}
}

func TestClientStream_RequiresSuccessfulTerminalEvent(t *testing.T) {
	tests := []struct {
		name      string
		stream    string
		wantErr   string
		cancelled bool
	}{
		{
			name:    "server error",
			stream:  "event: turn.error\ndata: {\"message\":\"history commit failed\"}\n\n",
			wantErr: "history commit failed",
		},
		{
			name:    "model required",
			stream:  "event: turn.model_required\ndata: {\"message\":\"pick a larger model\"}\n\n",
			wantErr: "pick a larger model",
		},
		{
			name:      "server cancellation",
			stream:    "event: turn.cancelled\ndata: {}\n\n",
			cancelled: true,
		},
		{
			name:    "premature eof",
			stream:  "event: text.delta\ndata: {\"text\":\"partial\"}\n\n",
			wantErr: "before a terminal turn event",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, tt.stream)
			}))
			defer srv.Close()

			client := NewClient(Config{ServerURL: srv.URL, Email: "u@x.co", Token: "token"})
			_, err := client.Stream(context.Background(), "hi", "", func(Event) {})
			if err == nil {
				t.Fatal("Stream returned nil error")
			}
			if tt.cancelled {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("Stream error = %v, want context cancellation", err)
				}
			} else if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Stream error = %q, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestClientPingRejectsNonSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not fleet", http.StatusNotFound)
	}))
	defer srv.Close()

	client := NewClient(Config{ServerURL: srv.URL})
	err := client.Ping(context.Background())
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("Ping error = %v, want health status", err)
	}
}

func TestClientStream_PropagatesInFlightCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: turn.started\ndata: {}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	client := NewClient(Config{ServerURL: srv.URL, Email: "u@x.co", Token: "token"})
	_, err := client.Stream(ctx, "hi", "", func(ev Event) {
		if ev.Name == "turn.started" {
			cancel()
		}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Stream error = %v, want context cancellation", err)
	}
}
