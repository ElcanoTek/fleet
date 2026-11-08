package httpmw_test

import (
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/ElcanoTek/fleet/internal/httpmw"
	"github.com/ElcanoTek/fleet/internal/logging"
	"github.com/ElcanoTek/fleet/internal/safe"
)

// A panic under /shared/<token> reports the route, never the token — the
// share token is a live capability — in the log line or the panic event a
// hook (Sentry) receives (#2074 review). Through net/http's ServeMux, chi,
// and with no router at all (the sanitised path).
func TestPanicOnASharePathKeepsTheTokenOut(t *testing.T) {
	const tok = "shareTok9Qx2Lm7Vb4Zp1Rt8" // matches no pattern, not registered
	prevSlog, prevHook := slog.Default(), safe.PanicEventHook
	t.Cleanup(func() {
		slog.SetDefault(prevSlog)
		safe.PanicEventHook = prevHook
		safe.SetPanicHandler(nil)
		log.SetOutput(os.Stderr)
		log.SetFlags(log.LstdFlags)
	})
	path := t.TempDir() + "/fleet.log"
	closer, err := logging.Configure(logging.Config{File: path, MaxSizeMB: 10, Format: "json", Level: "info"})
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	var locations []string
	safe.PanicEventHook = func(e safe.PanicEvent, _ any) { locations = append(locations, e.Location) }

	boom := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") })
	mux := http.NewServeMux()
	mux.Handle("/shared/", boom)
	cr := chi.NewRouter()
	cr.Use(httpmw.Recoverer)
	cr.Get("/shared/{token}", boom)
	for name, h := range map[string]http.Handler{
		"servemux":  httpmw.Recoverer(mux),
		"chi":       cr,
		"no router": httpmw.Recoverer(boom),
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/shared/"+tok+"?code=oauthCode77Hx", nil))
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("%s: status = %d", name, w.Code)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	all := string(data) + strings.Join(locations, "\n")
	if strings.Contains(all, tok) || strings.Contains(all, "oauthCode77Hx") {
		t.Fatalf("the share token or query reached the log or the event:\n%s", all)
	}
	want := []string{"GET /shared/", "GET /shared/{token}", "GET /shared/{…}"}
	if len(locations) != 3 {
		t.Fatalf("events = %q", locations)
	}
	for _, w := range want {
		if !strings.Contains(strings.Join(locations, "\n"), w) {
			t.Fatalf("no event labelled %q: %q", w, locations)
		}
	}
}

func TestSanitizePath(t *testing.T) {
	for in, want := range map[string]string{
		"/":                     "/",
		"/tasks":                "/tasks",
		"/shared/AbC123":        "/shared/{…}",
		"/invite/abcdefghijklm": "/invite/{…}",
		"/Tok3n/x":              "/{…}",
	} {
		if got := httpmw.SanitizePath(in); got != want {
			t.Errorf("SanitizePath(%q) = %q, want %q", in, got, want)
		}
	}
}
