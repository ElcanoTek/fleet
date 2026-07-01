package apiversion

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// echoPath is an inner handler that records the path it was served at, so a test
// can prove Router stripped the /v1 prefix before delegating.
func echoPath(seen *string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*seen = r.URL.Path
		w.WriteHeader(http.StatusOK)
	})
}

// TestRouter_VersionedStripsPrefixAndSetsHeader: /v1/<path> is served at <path>
// with the X-Fleet-API-Version header and no Deprecation.
func TestRouter_VersionedStripsPrefixAndSetsHeader(t *testing.T) {
	var seen string
	h := Router(echoPath(&seen))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/tasks", nil))

	if seen != "/tasks" {
		t.Errorf("inner saw path %q, want /tasks (prefix stripped)", seen)
	}
	if got := rec.Header().Get(VersionHeader); got != Version {
		t.Errorf("%s = %q, want %q", VersionHeader, got, Version)
	}
	if rec.Header().Get("Deprecation") != "" {
		t.Error("versioned response must NOT be deprecation-tagged")
	}
}

// TestRouter_LegacyTaggedDeprecated: a bare path is served unchanged but tagged
// Deprecation + Link(successor-version), and gets no version header.
func TestRouter_LegacyTaggedDeprecated(t *testing.T) {
	var seen string
	h := Router(echoPath(&seen))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/tasks", nil))

	if seen != "/tasks" {
		t.Errorf("inner saw path %q, want /tasks (unchanged)", seen)
	}
	if rec.Header().Get("Deprecation") != "true" {
		t.Error("legacy response must be tagged Deprecation: true")
	}
	if got := rec.Header().Get("Link"); got != `</v1/tasks>; rel="successor-version"` {
		t.Errorf("Link = %q, want the /v1 successor link", got)
	}
	if rec.Header().Get(VersionHeader) != "" {
		t.Error("legacy response must NOT carry the version header")
	}
}

// TestRouter_HealthAndInfoNotDeprecated: unversioned-forever paths (health
// probes + api-info) are served without a deprecation signal.
func TestRouter_HealthAndInfoNotDeprecated(t *testing.T) {
	for _, path := range []string{"/healthz", "/health", "/readyz", "/api-info"} {
		var seen string
		h := Router(echoPath(&seen))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Header().Get("Deprecation") != "" {
			t.Errorf("%s must not be deprecation-tagged (it is unversioned forever)", path)
		}
		if seen != path {
			t.Errorf("%s: inner saw %q, want unchanged", path, seen)
		}
	}
}

// TestInfoHandler returns the version metadata as JSON.
func TestInfoHandler(t *testing.T) {
	rec := httptest.NewRecorder()
	InfoHandler("1.2.3")(rec, httptest.NewRequest(http.MethodGet, "/api-info", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var body struct {
		APIVersion        string   `json:"api_version"`
		FleetVersion      string   `json:"fleet_version"`
		SupportedVersions []string `json:"supported_versions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.APIVersion != Version {
		t.Errorf("api_version = %q, want %q", body.APIVersion, Version)
	}
	if body.FleetVersion != "1.2.3" {
		t.Errorf("fleet_version = %q, want 1.2.3", body.FleetVersion)
	}
	if len(body.SupportedVersions) != 1 || body.SupportedVersions[0] != Version {
		t.Errorf("supported_versions = %v", body.SupportedVersions)
	}
}

// TestInfoHandler_MethodNotAllowed rejects non-GET.
func TestInfoHandler_MethodNotAllowed(t *testing.T) {
	rec := httptest.NewRecorder()
	InfoHandler("1.0.0")(rec, httptest.NewRequest(http.MethodPost, "/api-info", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /api-info = %d, want 405", rec.Code)
	}
}
