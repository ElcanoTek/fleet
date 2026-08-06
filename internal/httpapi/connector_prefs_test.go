package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/store"
)

// prefsFakeStore is fakeChatStore plus an in-memory user_connector_prefs so
// the /connector-prefs handler and the availability helpers can be exercised
// without a database.
type prefsFakeStore struct {
	*fakeChatStore
	prefs map[string]store.ConnectorPref
}

func (s *prefsFakeStore) SetConnectorPref(_ context.Context, _ string, p store.ConnectorPref) error {
	if p.Kind != store.ConnectorKindBundled && p.Kind != store.ConnectorKindRemote {
		return store.ErrConnectorPrefInvalid
	}
	if s.prefs == nil {
		s.prefs = map[string]store.ConnectorPref{}
	}
	s.prefs[store.ConnectorPrefKey(p.Kind, p.ConnectorID)] = p
	return nil
}

func (s *prefsFakeStore) DeleteConnectorPref(_ context.Context, _, kind, id string) error {
	delete(s.prefs, store.ConnectorPrefKey(kind, id))
	return nil
}

func (s *prefsFakeStore) ListConnectorPrefs(_ context.Context, _ string) (map[string]store.ConnectorPref, error) {
	return s.prefs, nil
}

func prefsRequest(t *testing.T, s *Server, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	r = r.WithContext(context.WithValue(r.Context(), ctxKeyUser, "u@x.com"))
	w := httptest.NewRecorder()
	s.connectorPrefs(w, r)
	return w
}

// The /connector-prefs endpoint: upsert, list, revert; malformed prefs are
// rejected; a seat is validated against the live catalog.
func TestConnectorPrefsEndpoint(t *testing.T) {
	fs := &prefsFakeStore{fakeChatStore: newFakeChatStore()}
	s := &Server{store: fs}

	if w := prefsRequest(t, s, http.MethodPut, "/connector-prefs",
		`{"kind":"bundled","connector_id":"gamma","enabled":false}`); w.Code != http.StatusNoContent {
		t.Fatalf("put: %d %s", w.Code, w.Body.String())
	}
	if w := prefsRequest(t, s, http.MethodPut, "/connector-prefs",
		`{"kind":"weird","connector_id":"x","enabled":true}`); w.Code != http.StatusBadRequest {
		t.Errorf("bad kind accepted: %d", w.Code)
	}

	w := prefsRequest(t, s, http.MethodGet, "/connector-prefs", "")
	if w.Code != http.StatusOK {
		t.Fatalf("get: %d", w.Code)
	}
	var resp struct {
		Prefs []store.ConnectorPref `json:"prefs"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if len(resp.Prefs) != 1 || resp.Prefs[0].ConnectorID != "gamma" || resp.Prefs[0].Enabled {
		t.Errorf("prefs = %+v", resp.Prefs)
	}

	if w := prefsRequest(t, s, http.MethodDelete, "/connector-prefs?kind=bundled&id=gamma", ""); w.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", w.Code)
	}
	w = prefsRequest(t, s, http.MethodGet, "/connector-prefs", "")
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Prefs) != 0 {
		t.Errorf("revert left prefs: %+v", resp.Prefs)
	}
}

// The availability helpers implement the layer contract: no explicit pref =
// available with the operator default; explicit disable hides; a stale seat
// degrades to the default seat.
func TestBundledPrefResolution(t *testing.T) {
	info := agent.OptionalServerInfo{Name: "gamma", EnabledByDefault: true, Accounts: []string{"client-a"}}
	none := map[string]store.ConnectorPref{}

	if avail, on, seat := bundledPrefFor(none, info); !avail || !on || seat != "" {
		t.Errorf("no pref: avail=%v on=%v seat=%q", avail, on, seat)
	}
	off := map[string]store.ConnectorPref{
		store.ConnectorPrefKey(store.ConnectorKindBundled, "gamma"): {Kind: store.ConnectorKindBundled, ConnectorID: "gamma", Enabled: false},
	}
	if avail, _, _ := bundledPrefFor(off, info); avail {
		t.Error("explicit disable should hide the connector")
	}
	staleSeat := map[string]store.ConnectorPref{
		store.ConnectorPrefKey(store.ConnectorKindBundled, "gamma"): {Kind: store.ConnectorKindBundled, ConnectorID: "gamma", Enabled: true, DefaultAccount: "gone"},
	}
	if _, _, seat := bundledPrefFor(staleSeat, info); seat != "" {
		t.Errorf("stale seat should degrade to default, got %q", seat)
	}
	goodSeat := map[string]store.ConnectorPref{
		store.ConnectorPrefKey(store.ConnectorKindBundled, "gamma"): {Kind: store.ConnectorKindBundled, ConnectorID: "gamma", Enabled: true, DefaultAccount: "client-a"},
	}
	if _, _, seat := bundledPrefFor(goodSeat, info); seat != "client-a" {
		t.Errorf("seat = %q, want client-a", seat)
	}

	// Two-toggle semantics: an explicit row carries the user's complete
	// choice — available without auto_enable means new chats start OFF even
	// when the operator default is on; auto_enable turns seeding back on;
	// and a disabled row never seeds regardless of auto_enable.
	availOnly := map[string]store.ConnectorPref{
		store.ConnectorPrefKey(store.ConnectorKindBundled, "gamma"): {Kind: store.ConnectorKindBundled, ConnectorID: "gamma", Enabled: true},
	}
	if avail, on, _ := bundledPrefFor(availOnly, info); !avail || on {
		t.Errorf("available-only pref: avail=%v on=%v, want true,false", avail, on)
	}
	alwaysOn := map[string]store.ConnectorPref{
		store.ConnectorPrefKey(store.ConnectorKindBundled, "gamma"): {Kind: store.ConnectorKindBundled, ConnectorID: "gamma", Enabled: true, AutoEnable: true},
	}
	if avail, on, _ := bundledPrefFor(alwaysOn, info); !avail || !on {
		t.Errorf("always-on pref: avail=%v on=%v, want true,true", avail, on)
	}
	disabledAuto := map[string]store.ConnectorPref{
		store.ConnectorPrefKey(store.ConnectorKindBundled, "gamma"): {Kind: store.ConnectorKindBundled, ConnectorID: "gamma", Enabled: false, AutoEnable: true},
	}
	if avail, on, _ := bundledPrefFor(disabledAuto, info); avail || on {
		t.Errorf("disabled pref: avail=%v on=%v, want false,false", avail, on)
	}

	if !remoteEnabledFor(none, "srv-1") {
		t.Error("remote defaults to enabled")
	}
	roff := map[string]store.ConnectorPref{
		store.ConnectorPrefKey(store.ConnectorKindRemote, "srv-1"): {Kind: store.ConnectorKindRemote, ConnectorID: "srv-1", Enabled: false},
	}
	if remoteEnabledFor(roff, "srv-1") {
		t.Error("explicit remote disable ignored")
	}
}
