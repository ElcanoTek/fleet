package chattui

import (
	"bytes"
	"context"
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
	code := runOneShot(Config{ServerURL: srv.URL, Email: "u@x.co", Token: "sekret"}, "", "what is 6*7?", strings.NewReader(""), &out, &errOut)
	if code != 0 {
		t.Fatalf("exit %d; stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "the answer is 42") {
		t.Errorf("stdout missing reply: %q", out.String())
	}
	if !strings.Contains(errOut.String(), "python") {
		t.Errorf("tool-call progress should go to stderr: %q", errOut.String())
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
