package chattui

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The pieces `fleet acp` (internal/acp) relies on: the public web URL for
// approval deep links, the server-side Stop call, the client label, and a
// typed status error so an auth failure is distinguishable without parsing text.

func TestResolvePublicURL(t *testing.T) {
	base := map[string]string{"FLEET_SERVER_TOKEN": "tok", "FLEET_USER_EMAIL": "a@b.c"}
	t.Run("env, base URL preferred, trailing slash trimmed", func(t *testing.T) {
		env := map[string]string{"FLEET_PUBLIC_BASE_URL": "https://fleet.example.com/", "FLEET_PUBLIC_URL": "https://other"}
		for k, v := range base {
			env[k] = v
		}
		cfg, err := Resolve(Flags{}, envMap(env), noFile, noEnvFile)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.PublicURL != "https://fleet.example.com" {
			t.Errorf("PublicURL = %q", cfg.PublicURL)
		}
	})
	t.Run("from the server env file even when token and addr are already set", func(t *testing.T) {
		cfg, err := Resolve(Flags{EnvFile: "/etc/fleet/fleet.env"}, envMap(base), noFile,
			envFileFrom(map[string]map[string]string{"/etc/fleet/fleet.env": {"FLEET_PUBLIC_URL": "https://box.example.com"}}))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.PublicURL != "https://box.example.com" {
			t.Errorf("PublicURL = %q", cfg.PublicURL)
		}
	})
	t.Run("never from a different deployment's env file", func(t *testing.T) {
		// The token and address come from .env.local; /etc/fleet/fleet.env
		// belongs to another deployment and must not supply the deep link.
		cfg, err := Resolve(Flags{Email: "a@b.c"}, envMap(map[string]string{}), noFile, envFileFrom(map[string]map[string]string{
			".env.local":           {"FLEET_SERVER_TOKEN": "tok", "FLEET_SERVER_ADDR": "127.0.0.1:9000"},
			"/etc/fleet/fleet.env": {"FLEET_SERVER_TOKEN": "other", "FLEET_PUBLIC_URL": "https://other.example.com"},
		}))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Token != "tok" || cfg.PublicURL != "" {
			t.Errorf("token=%q PublicURL=%q, want tok and no public URL", cfg.Token, cfg.PublicURL)
		}
	})
	t.Run("from the file that supplied the server config", func(t *testing.T) {
		cfg, err := Resolve(Flags{Email: "a@b.c"}, envMap(map[string]string{}), noFile, envFileFrom(map[string]map[string]string{
			"/etc/fleet/fleet.env": {"FLEET_SERVER_TOKEN": "tok", "FLEET_PUBLIC_BASE_URL": "https://box.example.com"},
		}))
		if err != nil || cfg.PublicURL != "https://box.example.com" {
			t.Errorf("PublicURL = %q, err %v", cfg.PublicURL, err)
		}
	})
	t.Run("a file that supplied nothing is not the deployment", func(t *testing.T) {
		// The token came from the env; .env.local matched only on a token key
		// and has no address, so the client talks to the loopback default —
		// its (possibly stale) public URL must not be used.
		cfg, err := Resolve(Flags{}, envMap(base), noFile, envFileFrom(map[string]map[string]string{
			".env.local": {"FLEET_SERVER_TOKEN": "other", "FLEET_PUBLIC_URL": "https://stale.example.com"},
		}))
		if err != nil || cfg.PublicURL != "" || cfg.Token != "tok" {
			t.Errorf("token=%q PublicURL=%q err=%v", cfg.Token, cfg.PublicURL, err)
		}
	})
	t.Run("token from env and no pinned file: not probed", func(t *testing.T) {
		cfg, err := Resolve(Flags{}, envMap(base), noFile, envFileFrom(map[string]map[string]string{
			"/etc/fleet/fleet.env": {"FLEET_PUBLIC_URL": "https://maybe-other.example.com"},
		}))
		if err != nil || cfg.PublicURL != "" {
			t.Errorf("PublicURL = %q, err %v", cfg.PublicURL, err)
		}
	})
	t.Run("an explicit --server ignores ambient public URLs", func(t *testing.T) {
		env := map[string]string{"FLEET_PUBLIC_URL": "https://deployment-a.example.com"}
		for k, v := range base {
			env[k] = v
		}
		cfg, err := Resolve(Flags{Server: "https://deployment-b.example.com"}, envMap(env), noFile, noEnvFile)
		if err != nil || cfg.PublicURL != "" {
			t.Errorf("PublicURL = %q err=%v, want none (it belongs to another deployment)", cfg.PublicURL, err)
		}
		cfg, err = Resolve(Flags{Server: "https://deployment-b.example.com", PublicURL: "https://b.example.com/"}, envMap(env), noFile, noEnvFile)
		if err != nil || cfg.PublicURL != "https://b.example.com" {
			t.Errorf("explicit --public-url = %q err=%v", cfg.PublicURL, err)
		}
	})
	t.Run("an env file that picked the server outranks an ambient public URL", func(t *testing.T) {
		// --env-file supplies deployment B's token and address; the process
		// env still carries deployment A's public URL.
		env := map[string]string{"FLEET_USER_EMAIL": "a@b.c", "FLEET_PUBLIC_URL": "https://deployment-a.example.com"}
		files := map[string]map[string]string{"/srv/b.env": {"FLEET_SERVER_TOKEN": "tok-b", "FLEET_SERVER_ADDR": "b:8080"}}
		cfg, err := Resolve(Flags{EnvFile: "/srv/b.env"}, envMap(env), noFile, envFileFrom(files))
		if err != nil || cfg.PublicURL != "" {
			t.Errorf("PublicURL = %q err=%v, want none (the ambient one is deployment A's)", cfg.PublicURL, err)
		}
		files["/srv/b.env"]["FLEET_PUBLIC_URL"] = "https://deployment-b.example.com"
		cfg, err = Resolve(Flags{EnvFile: "/srv/b.env"}, envMap(env), noFile, envFileFrom(files))
		if err != nil || cfg.PublicURL != "https://deployment-b.example.com" {
			t.Errorf("PublicURL = %q err=%v, want the env file's own", cfg.PublicURL, err)
		}
	})
	t.Run("unset is empty", func(t *testing.T) {
		cfg, err := Resolve(Flags{}, envMap(base), noFile, noEnvFile)
		if err != nil || cfg.PublicURL != "" {
			t.Errorf("PublicURL = %q, err %v", cfg.PublicURL, err)
		}
	})
}

func TestCancelStopsTheTurnServerSide(t *testing.T) {
	var gotPath, gotBody, gotClient, gotToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotPath, gotBody = r.Method+" "+r.URL.Path, string(b)
		gotClient, gotToken = r.Header.Get("X-Fleet-Client"), r.Header.Get("X-Chat-Server-Token")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	c := NewClient(Config{ServerURL: srv.URL, Email: "a@b.c", Token: "tok", ClientName: "fleet-acp"})
	if err := c.Cancel("conv 1", ""); err != nil {
		t.Fatal(err)
	}
	if gotPath != "POST /conversations/conv 1/cancel" || gotBody != `{"scope":"turn"}` {
		t.Errorf("request = %s %s", gotPath, gotBody)
	}
	if gotClient != "fleet-acp" || gotToken != "tok" {
		t.Errorf("headers: client=%q token=%q", gotClient, gotToken)
	}
	if err := c.Cancel("", ""); err != nil {
		t.Errorf("no conversation yet must be a no-op, got %v", err)
	}
}

func TestStreamReturnsTypedStatusError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Fleet-Client"); got != "fleet-chat" {
			t.Errorf("default client label = %q", got)
		}
		http.Error(w, "forbidden", http.StatusForbidden) // the shared-token check's exact refusal
	}))
	defer srv.Close()
	_, err := NewClient(Config{ServerURL: srv.URL, Email: "a@b.c", Token: "tok"}).Stream(context.Background(), "hi", "", func(Event) {})
	var se *StatusError
	if !errors.As(err, &se) || se.Code != http.StatusForbidden {
		t.Fatalf("err = %#v, want *StatusError 403", err)
	}
	if se.Error() != "server rejected the request (403): check FLEET_SERVER_TOKEN matches the server" {
		t.Errorf("message changed: %q", se.Error())
	}
}

// POST /chat answers 403 for four reasons fleet names (token, membership,
// viewer role, IP filter); each gets its own fix-it text, and anything else is
// quoted as a short single-line excerpt. Every one stays a 403 StatusError so
// `fleet acp` still maps it to auth_required. Only the token check's own body
// may point at FLEET_SERVER_TOKEN. Bodies are the exact bytes the server writes.
func TestStream403NamesTheRefusalReason(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		body        string
		want        string
	}{
		{
			name:        "wrong token",
			contentType: "text/plain; charset=utf-8",
			body:        "forbidden\n",
			want:        "server rejected the request (403): check FLEET_SERVER_TOKEN matches the server",
		},
		{
			name:        "not a member",
			contentType: "application/json",
			body:        `{"error":"not_a_member"}`,
			want:        "server rejected the request (403): nobody@example.com is not a fleet user; an admin can add it with `fleet chat user add nobody@example.com --password -`, or use --email/FLEET_USER_EMAIL for a provisioned user",
		},
		{
			name:        "viewer",
			contentType: "application/json",
			body:        `{"error":"read_only"}`,
			want:        "server rejected the request (403): nobody@example.com has the read-only viewer role and cannot send messages; an admin can change it with `fleet chat user role nobody@example.com --role member`",
		},
		{
			name:        "ip filter",
			contentType: "text/plain; charset=utf-8",
			body:        "Access denied\n",
			want:        "server rejected the request (403): the server's IP access control (FLEET_IP_ALLOWLIST / FLEET_IP_DENYLIST) does not admit this client's address; connect from an admitted address, or ask an admin to admit this one",
		},
		{
			name:        "unknown json code",
			contentType: "application/json",
			body:        `{"error":"something_else"}`,
			want:        `server rejected the request (403): {"error":"something_else"}`,
		},
		{
			name:        "reverse proxy html",
			contentType: "text/html",
			body:        "<html>\r\n<head><title>403 Forbidden</title></head>\r\n<body>\r\n<center><h1>403 Forbidden</h1></center>\r\n<hr><center>nginx</center>\r\n</body>\r\n</html>\r\n",
			want:        "server rejected the request (403): <html> <head><title>403 Forbidden</title></head> <body> <center><h1>403 Forbidden</h1></center> <hr><center>nginx</center> </body> </html>",
		},
		{
			name:        "long body is capped",
			contentType: "text/plain",
			body:        strings.Repeat("a", 300),
			want:        "server rejected the request (403): " + strings.Repeat("a", 160) + "…",
		},
		{
			name:        "empty body",
			contentType: "text/plain",
			body:        "",
			want:        "server rejected the request (403)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", tt.contentType)
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()
			_, err := NewClient(Config{ServerURL: srv.URL, Email: "nobody@example.com", Token: "super-secret-token"}).Stream(context.Background(), "hi", "", func(Event) {})
			var se *StatusError
			if !errors.As(err, &se) || se.Code != http.StatusForbidden {
				t.Fatalf("err = %#v, want *StatusError 403", err)
			}
			if se.Error() != tt.want {
				t.Errorf("message = %q\nwant      %q", se.Error(), tt.want)
			}
			if strings.Contains(se.Error(), "super-secret-token") {
				t.Errorf("error must NOT leak the token: %v", se)
			}
		})
	}
}

// A 400 is fleet refusing the request, not the user: it is quoted as a
// rejection, never as "not authorized", while a 401 keeps naming the user.
// The non-empty 400 bodies are the exact bytes POST /chat writes (http.Error
// appends "\n"); the 401 body stands in for a proxy's, since fleet chat and
// fleet acp never send what fleet's own 401 answers.
func TestStream400IsARejectionNotAnAuthFailure(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{
			name:   "body over the 1 MB cap",
			status: http.StatusBadRequest,
			body:   "bad json: http: request body too large\n",
			want:   "server rejected the request (400): bad json: http: request body too large",
		},
		{
			name:   "empty message",
			status: http.StatusBadRequest,
			body:   "message is required\n",
			want:   "server rejected the request (400): message is required",
		},
		{
			name:   "lockdown model",
			status: http.StatusBadRequest,
			body:   "model not allowed in lockdown mode\n",
			want:   "server rejected the request (400): model not allowed in lockdown mode",
		},
		{
			name:   "empty body",
			status: http.StatusBadRequest,
			body:   "",
			want:   "server rejected the request (400)",
		},
		{
			name:   "401 still names the user",
			status: http.StatusUnauthorized,
			body:   "unauthorized\n",
			want:   "not authorized (401) for nobody@example.com: unauthorized",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/plain; charset=utf-8")
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()
			_, err := NewClient(Config{ServerURL: srv.URL, Email: "nobody@example.com", Token: "super-secret-token"}).Stream(context.Background(), "hi", "", func(Event) {})
			var se *StatusError
			if !errors.As(err, &se) || se.Code != tt.status {
				t.Fatalf("err = %#v, want *StatusError %d", err, tt.status)
			}
			if se.Error() != tt.want {
				t.Errorf("message = %q\nwant      %q", se.Error(), tt.want)
			}
		})
	}
}

// A stream that dies before its first frame still reports the conversation
// the server named on the response headers (#1591).
func TestStreamReportsTheHeaderConversationID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Fleet-Conversation-Id", "conv-h")
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	var seen []string
	id, err := NewClient(Config{ServerURL: srv.URL, Email: "a@b.c", Token: "tok"}).Stream(context.Background(), "hi", "", func(ev Event) {
		seen = append(seen, ev.Name+":"+ev.Str("id"))
	})
	if err == nil {
		t.Fatal("want the interrupted-stream error")
	}
	if id != "conv-h" || len(seen) != 1 || seen[0] != "conversation:conv-h" {
		t.Errorf("id=%q events=%v", id, seen)
	}
	// Resuming an existing conversation never lets the header override it.
	id, _ = NewClient(Config{ServerURL: srv.URL, Email: "a@b.c", Token: "tok"}).Stream(context.Background(), "hi", "conv-mine", func(Event) {})
	if id != "conv-mine" {
		t.Errorf("resumed id = %q", id)
	}
}

// A queue acknowledgement (202, or a 200 JSON replay of an accepted input) is
// reported as *QueuedError, not a stream failure.
func TestStreamReportsAQueuedSubmission(t *testing.T) {
	for _, status := range []int{http.StatusAccepted, http.StatusOK} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, `{"queued":true,"input":{"id":"in-1","position":3,"ahead":1},"conversation_id":"conv-q"}`)
		}))
		id, err := NewClient(Config{ServerURL: srv.URL, Email: "a@b.c", Token: "tok"}).StreamInput(context.Background(), "hi", "conv-q", "key-1", func(Event) {})
		srv.Close()
		var q *QueuedError
		if !errors.As(err, &q) || q.Position != 3 || q.Ahead == nil || *q.Ahead != 1 || q.InputID != "in-1" || id != "conv-q" {
			t.Errorf("status %d: id=%q err=%#v, want *QueuedError position 3, 1 ahead", status, id, err)
		}
	}
}

// An acknowledgement without "ahead" (an older server, a failed count, or an
// input no longer in line) decodes to a nil Ahead, and the message then names
// no place — never the position key in its stead.
func TestStreamQueuedAckWithoutAheadNamesNoPlace(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, `{"queued":true,"input":{"id":"in-1","position":40,"state":"queued"},"conversation_id":"conv-q"}`)
	}))
	defer srv.Close()
	_, err := NewClient(Config{ServerURL: srv.URL, Email: "a@b.c", Token: "tok"}).StreamInput(context.Background(), "hi", "conv-q", "key-1", func(Event) {})
	var q *QueuedError
	if !errors.As(err, &q) || q.Ahead != nil {
		t.Fatalf("err = %#v, want *QueuedError with a nil Ahead", err)
	}
	if want := "a turn is already running in this conversation, so the message was queued and will run after it"; q.Error() != want {
		t.Errorf("Error() = %q, want %q", q.Error(), want)
	}
}

// A queued submission's message names its place in line (the server's
// "ahead" count), never the position ordering key; with no count it names
// no place.
func TestQueuedErrorNamesThePlaceInLine(t *testing.T) {
	ahead := func(n int) *int { return &n }
	const base = "a turn is already running in this conversation, so the message was queued and will run after it"
	for _, tc := range []struct {
		q    QueuedError
		want string
	}{
		{QueuedError{Position: 2, Ahead: ahead(0)}, base + " (next in line)"},
		{QueuedError{Position: 0, Ahead: ahead(1)}, base + " and 1 other queued message"},
		{QueuedError{Position: 40, Ahead: ahead(3)}, base + " and 3 other queued messages"},
		{QueuedError{Position: 40}, base},
	} {
		if got := tc.q.Error(); got != tc.want {
			t.Errorf("Error() = %q, want %q", got, tc.want)
		}
	}
}

// A replay's message gives the earlier input's state. Its row settles after
// its turn's stream ends (possibly much later, if settlement fails), so
// "running" may be a turn that has finished, and the message allows for that.
func TestQueuedErrorForAReplay(t *testing.T) {
	for _, tc := range []struct {
		q    QueuedError
		want string
	}{
		{QueuedError{Mode: "direct", State: "running", Replay: true}, "this message is already running or has finished (it was accepted earlier)"},
		{QueuedError{Mode: "steer", State: "injected", Replay: true}, "this message is already running or has finished (it was accepted earlier)"},
		{QueuedError{Mode: "direct", State: "completed", Replay: true}, "this message already ran (it was accepted earlier)"},
		{QueuedError{Mode: "direct", State: "cancelled", Replay: true}, "this message was accepted earlier but did not run"},
	} {
		if got := tc.q.Error(); got != tc.want {
			t.Errorf("%s: Error() = %q, want %q", tc.q.State, got, tc.want)
		}
	}
}

func TestCancelNamesTheTurn(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	if err := NewClient(Config{ServerURL: srv.URL, Email: "a@b.c", Token: "tok"}).Cancel("c", "turn-9"); err != nil {
		t.Fatal(err)
	}
	if gotBody != `{"scope":"turn","turn_id":"turn-9"}` {
		t.Errorf("body = %s", gotBody)
	}
}

func TestStreamSurfacesTheHeaderTurnID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Fleet-Conversation-Id", "c")
		w.Header().Set("X-Fleet-Turn-Id", "t-1")
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	var turn string
	_, _ = NewClient(Config{ServerURL: srv.URL, Email: "a@b.c", Token: "tok"}).Stream(context.Background(), "hi", "", func(ev Event) {
		if ev.Name == "turn.identified" {
			turn = ev.Str("turn_id")
		}
	})
	if turn != "t-1" {
		t.Errorf("turn id = %q", turn)
	}
}

// A queue acknowledgement cut off mid-body is an unknown outcome (fleet may
// have queued the message), never a definite *StatusError.
func TestTruncatedQueueAckIsNotAStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, `{"queued":tr`)
	}))
	defer srv.Close()
	_, err := NewClient(Config{ServerURL: srv.URL, Email: "a@b.c", Token: "tok"}).StreamInput(context.Background(), "hi", "c", "k", func(Event) {})
	var se *StatusError
	var qe *QueuedError
	if err == nil || errors.As(err, &se) || errors.As(err, &qe) {
		t.Fatalf("err = %#v, want a plain unknown-outcome error", err)
	}
}

// With ModelNewConversationsOnly, the configured model is sent only on a turn
// that starts a conversation, never as an override on an existing one.
func TestModelNewConversationsOnly(t *testing.T) {
	var models []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		models = append(models, body.Model)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: conversation\ndata: {\"id\":\"c1\"}\n\nevent: turn.completed\ndata: {}\n\n")
	}))
	defer srv.Close()
	for _, newOnly := range []bool{true, false} {
		models = nil
		c := NewClient(Config{ServerURL: srv.URL, Email: "a@b.c", Token: "t", Model: "x/model", ModelNewConversationsOnly: newOnly})
		for _, conv := range []string{"", "c1"} {
			if _, err := c.StreamInput(context.Background(), "hi", conv, "", func(Event) {}); err != nil {
				t.Fatal(err)
			}
		}
		want := []string{"x/model", ""}
		if !newOnly {
			want = []string{"x/model", "x/model"}
		}
		if len(models) != 2 || models[0] != want[0] || models[1] != want[1] {
			t.Errorf("newOnly=%v: models sent = %q, want %q", newOnly, models, want)
		}
	}
}
