package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/sched/storage"
)

// GET /api/config carries the server's own clock so the dashboard can show
// server time honestly. Two properties matter and neither is implied by the
// timezone name alone: the timestamp must be formatted in the SERVER's location
// (so the offset travels with it, which is what lets a client render the time
// even when its ICU data has never heard of the zone), and it must be a real
// instant (so a client can measure its own clock's skew against it rather than
// rendering "now" from a browser clock that may simply be wrong).
func TestGetDashboardConfigCarriesServerClock(t *testing.T) {
	store := storage.New()
	store.SetTimezone("America/New_York")
	h := &Handlers{
		config:  Config{Version: "test", Timezone: "America/New_York", DefaultTaskTimezone: "Europe/London"},
		storage: store,
	}

	before := time.Now()
	rr := httptest.NewRecorder()
	h.GetDashboardConfig(rr, httptest.NewRequest(http.MethodGet, "/api/config", nil))
	after := time.Now()

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var body struct {
		Version             string `json:"version"`
		Timezone            string `json:"timezone"`
		ServerTime          string `json:"server_time"`
		DefaultTaskTimezone string `json:"default_task_timezone"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if body.Timezone != "America/New_York" {
		t.Errorf("timezone = %q, want the server clock's zone", body.Timezone)
	}
	// The task-scheduling default is reported separately: it is the zone a cron
	// recurrence actually fires in, and a deployment may set it differently from
	// the server clock.
	if body.DefaultTaskTimezone != "Europe/London" {
		t.Errorf("default_task_timezone = %q, want Europe/London", body.DefaultTaskTimezone)
	}

	parsed, err := time.Parse(time.RFC3339, body.ServerTime)
	if err != nil {
		t.Fatalf("server_time %q is not RFC3339: %v", body.ServerTime, err)
	}
	if parsed.Before(before.Add(-time.Second)) || parsed.After(after.Add(time.Second)) {
		t.Errorf("server_time %v is not the handler's own now (%v..%v)", parsed, before, after)
	}

	// The offset must be the SERVER's, not UTC's — otherwise a client whose ICU
	// data lacks the zone would render the wrong wall clock.
	_, offset := parsed.Zone()
	_, wantOffset := time.Now().In(store.Location()).Zone()
	if offset != wantOffset {
		t.Errorf("server_time offset = %ds, want the server location's %ds", offset, wantOffset)
	}
}

// A deployment that never called SetTimezone (or set an unloadable one) still
// has a location — storage.New defaults it to UTC — so the endpoint must not
// panic or emit a zero time.
func TestGetDashboardConfigDefaultsToUTC(t *testing.T) {
	h := &Handlers{config: Config{Version: "test"}, storage: storage.New()}

	rr := httptest.NewRecorder()
	h.GetDashboardConfig(rr, httptest.NewRequest(http.MethodGet, "/api/config", nil))

	var body struct {
		ServerTime          string `json:"server_time"`
		DefaultTaskTimezone string `json:"default_task_timezone"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	parsed, err := time.Parse(time.RFC3339, body.ServerTime)
	if err != nil {
		t.Fatalf("server_time %q is not RFC3339: %v", body.ServerTime, err)
	}
	if _, offset := parsed.Zone(); offset != 0 {
		t.Errorf("offset = %ds, want 0 (UTC) for an unconfigured server", offset)
	}
	if body.DefaultTaskTimezone != "UTC" {
		t.Errorf("default_task_timezone = %q, want UTC", body.DefaultTaskTimezone)
	}
}
