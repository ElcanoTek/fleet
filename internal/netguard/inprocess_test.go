package netguard

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestWrapRoundTripperInProcess(t *testing.T) {
	RegisterInProcessHandler("test-inproc", func(u *url.URL) bool {
		return strings.EqualFold(u.Host, "fleet.example.com") && u.Path == "/mcp"
	}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"echo":` + string(body) + `}`))
	}))
	defer RegisterInProcessHandler("test-inproc", nil, nil)

	calledBase := false
	base := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		calledBase = true
		return nil, io.EOF
	})

	client := &http.Client{Transport: WrapRoundTripper(base)}
	resp, err := client.Post("https://fleet.example.com/mcp", "application/json", strings.NewReader(`"hello"`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()

	if calledBase {
		t.Fatalf("expected in-process handler to intercept request without calling base transport")
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected status: %d", resp.StatusCode)
	}
	got, _ := io.ReadAll(resp.Body)
	if string(got) != `{"echo":"hello"}` {
		t.Fatalf("unexpected body: %s", string(got))
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
