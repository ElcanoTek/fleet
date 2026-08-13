package store

import (
	"context"
	"errors"
	"testing"
)

// Connector prefs are explicit per-user choices: upsert wins, absence means
// operator default (delete reverts), remote rows may not carry a seat, and
// rows die with the user.
func TestConnectorPrefs(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	const email = "Brad@Elcano.com"

	if err := s.SetConnectorPref(ctx, email, ConnectorPref{Kind: ConnectorKindBundled, ConnectorID: "xandr", Enabled: true, DefaultAccount: "client-a"}); err != nil {
		t.Fatalf("set: %v", err)
	}
	// Upsert flips the seat + enabled state in place.
	if err := s.SetConnectorPref(ctx, email, ConnectorPref{Kind: ConnectorKindBundled, ConnectorID: "xandr", Enabled: false, DefaultAccount: "client-b"}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := s.SetConnectorPref(ctx, email, ConnectorPref{Kind: ConnectorKindRemote, ConnectorID: "srv-1", Enabled: false}); err != nil {
		t.Fatalf("set remote: %v", err)
	}

	prefs, err := s.ListConnectorPrefs(ctx, "brad@elcano.com")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(prefs) != 2 {
		t.Fatalf("want 2 prefs, got %d: %+v", len(prefs), prefs)
	}
	x := prefs[ConnectorPrefKey(ConnectorKindBundled, "xandr")]
	if x.Enabled || x.DefaultAccount != "client-b" {
		t.Errorf("upsert did not win: %+v", x)
	}

	// auto_enable round-trips and upserts like the other fields.
	if err := s.SetConnectorPref(ctx, email, ConnectorPref{Kind: ConnectorKindBundled, ConnectorID: "feeds", Enabled: true, AutoEnable: true}); err != nil {
		t.Fatalf("set auto_enable: %v", err)
	}
	prefs, err = s.ListConnectorPrefs(ctx, email)
	if err != nil {
		t.Fatalf("list after auto_enable: %v", err)
	}
	if f := prefs[ConnectorPrefKey(ConnectorKindBundled, "feeds")]; !f.Enabled || !f.AutoEnable {
		t.Errorf("auto_enable did not round-trip: %+v", f)
	}
	// auto_enable is a bundled-catalog concept; a remote row must reject it.
	if err := s.SetConnectorPref(ctx, email, ConnectorPref{Kind: ConnectorKindRemote, ConnectorID: "srv-2", Enabled: true, AutoEnable: true}); !errors.Is(err, ErrConnectorPrefInvalid) {
		t.Errorf("remote auto_enable: err = %v, want ErrConnectorPrefInvalid", err)
	}

	// Validation: unknown kind, blank id, seat on a remote row.
	for _, bad := range []ConnectorPref{
		{Kind: "weird", ConnectorID: "x", Enabled: true},
		{Kind: ConnectorKindBundled, ConnectorID: " ", Enabled: true},
		{Kind: ConnectorKindRemote, ConnectorID: "srv-1", Enabled: true, DefaultAccount: "seat"},
	} {
		if err := s.SetConnectorPref(ctx, email, bad); !errors.Is(err, ErrConnectorPrefInvalid) {
			t.Errorf("bad pref %+v accepted (err=%v)", bad, err)
		}
	}

	// Delete reverts to operator default; deleting again stays a no-op.
	if err := s.DeleteConnectorPref(ctx, email, ConnectorKindRemote, "srv-1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := s.DeleteConnectorPref(ctx, email, ConnectorKindRemote, "srv-1"); err != nil {
		t.Fatalf("re-delete: %v", err)
	}
	prefs, _ = s.ListConnectorPrefs(ctx, email)
	// xandr (bundled) + feeds (bundled, the auto_enable row) remain.
	if len(prefs) != 2 {
		t.Errorf("want 2 prefs after delete, got %+v", prefs)
	}
	if _, gone := prefs[ConnectorPrefKey(ConnectorKindRemote, "srv-1")]; gone {
		t.Errorf("deleted remote pref still present: %+v", prefs)
	}
}
