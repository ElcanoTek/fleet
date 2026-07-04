package settings

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/store"
)

// fakeStore is an in-memory Store seam so the service logic (precedence,
// validation ordering, apply hooks) is tested without Postgres; the real
// store methods have their own DB-gated test.
type fakeStore struct {
	rows    map[string]store.WorkspaceSetting
	failGet error
	failSet error
}

func newFakeStore() *fakeStore { return &fakeStore{rows: map[string]store.WorkspaceSetting{}} }

func (f *fakeStore) WorkspaceSettings(context.Context) (map[string]store.WorkspaceSetting, error) {
	if f.failGet != nil {
		return nil, f.failGet
	}
	out := map[string]store.WorkspaceSetting{}
	for k, v := range f.rows {
		out[k] = v
	}
	return out, nil
}

func (f *fakeStore) SetWorkspaceSetting(_ context.Context, key, value, updatedBy string) (store.WorkspaceSetting, error) {
	if f.failSet != nil {
		return store.WorkspaceSetting{}, f.failSet
	}
	row := store.WorkspaceSetting{Key: key, Value: value, UpdatedAt: 1234, UpdatedBy: updatedBy}
	f.rows[key] = row
	return row, nil
}

func (f *fakeStore) DeleteWorkspaceSetting(_ context.Context, key string) error {
	delete(f.rows, key)
	return nil
}

// testDefaults returns a full env-default map (what cmd/fleet derives from a
// default Config).
func testDefaults() map[string]string {
	return map[string]string{
		"pii_redaction_mode":                "off",
		"tool_disclosure_threshold":         "128",
		"max_tool_output_bytes":             "65536",
		"phone_a_friend_enabled":            "false",
		"subagents_enabled":                 "false",
		"memory_autoindex_enabled":          "false",
		"error_analysis_enabled":            "true",
		"auto_title_enabled":                "true",
		"connector_recommendations_enabled": "false",
		"context_handles_enabled":           "false",
	}
}

// testHooks records every apply (value + override flag) so tests can assert
// what reached the runtime.
func testHooks(applied map[string]string) map[string]ApplyFunc {
	hooks := map[string]ApplyFunc{}
	for _, spec := range Registry() {
		key := spec.Key
		hooks[key] = func(v string, override bool) error {
			applied[key] = v
			applied[key+"/override"] = fmt.Sprintf("%v", override)
			return nil
		}
	}
	return hooks
}

func newTestService(t *testing.T, st Store, applied map[string]string) *Service {
	t.Helper()
	svc, err := NewService(st, testDefaults(), testHooks(applied))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc
}

// TestNewServiceRequiresFullCoverage: a registry key without a default or hook
// is a wiring bug reported at construction, not a silent runtime gap.
func TestNewServiceRequiresFullCoverage(t *testing.T) {
	applied := map[string]string{}
	defaults := testDefaults()
	delete(defaults, "pii_redaction_mode")
	if _, err := NewService(newFakeStore(), defaults, testHooks(applied)); err == nil {
		t.Fatal("missing default should fail construction")
	}
	hooks := testHooks(applied)
	delete(hooks, "subagents_enabled")
	if _, err := NewService(newFakeStore(), testDefaults(), hooks); err == nil {
		t.Fatal("missing hook should fail construction")
	}
	// An OUT-OF-BOUNDS env default must NOT fail construction: the env accepts
	// a wider range than the admin bounds, and one legacy env value disabling
	// the whole panel would be a regression (the value is kept verbatim).
	defaults = testDefaults()
	defaults["max_tool_output_bytes"] = "512" // legal env ceiling, below admin Min
	svc, err := NewService(newFakeStore(), defaults, testHooks(applied))
	if err != nil {
		t.Fatalf("out-of-bounds env default must not fail construction: %v", err)
	}
	snap, err := svc.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	for _, r := range snap {
		if r.Key == "max_tool_output_bytes" && (r.Default != "512" || r.Value != "512") {
			t.Errorf("verbatim env default should serve: %+v", r)
		}
	}
	if _, err := NewService(newFakeStore(), map[string]string{}, map[string]ApplyFunc{}); err == nil {
		t.Fatal("empty wiring should fail construction")
	}
}

// TestValidate: table-driven over the three kinds, including the MinZeroOK
// escape hatch and normalization.
func TestValidate(t *testing.T) {
	spec := func(key string) Spec {
		for _, s := range Registry() {
			if s.Key == key {
				return s
			}
		}
		t.Fatalf("no spec %q", key)
		return Spec{}
	}
	cases := []struct {
		key, in, want string
		wantErr       bool
	}{
		{"pii_redaction_mode", "redact", "redact", false},
		{"pii_redaction_mode", " Block ", "block", false},
		{"pii_redaction_mode", "shred", "", true},
		{"phone_a_friend_enabled", "TRUE", "true", false},
		{"phone_a_friend_enabled", "yes", "", true},
		{"tool_disclosure_threshold", "42", "42", false},
		{"tool_disclosure_threshold", "0", "", true}, // no MinZeroOK: 0 is out of range
		{"tool_disclosure_threshold", "100001", "", true},
		{"tool_disclosure_threshold", "abc", "", true},
		{"max_tool_output_bytes", "0", "0", false}, // MinZeroOK: 0 = no ceiling
		{"max_tool_output_bytes", "512", "", true}, // below Min and not 0
		{"max_tool_output_bytes", "65536", "65536", false},
	}
	for _, c := range cases {
		got, err := Validate(spec(c.key), c.in)
		if c.wantErr != (err != nil) {
			t.Errorf("Validate(%s, %q) err = %v, wantErr %v", c.key, c.in, err, c.wantErr)
			continue
		}
		if !c.wantErr && got != c.want {
			t.Errorf("Validate(%s, %q) = %q, want %q", c.key, c.in, got, c.want)
		}
	}
}

// TestSnapshotPrecedence: an override row wins; everything else reports the
// env default; an override that no longer validates degrades to the default
// instead of surfacing an illegal value.
func TestSnapshotPrecedence(t *testing.T) {
	st := newFakeStore()
	st.rows["pii_redaction_mode"] = store.WorkspaceSetting{Key: "pii_redaction_mode", Value: "block", UpdatedAt: 99, UpdatedBy: "admin@x"}
	st.rows["tool_disclosure_threshold"] = store.WorkspaceSetting{Key: "tool_disclosure_threshold", Value: "999999999"} // out of bounds now
	applied := map[string]string{}
	svc := newTestService(t, st, applied)

	snap, err := svc.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(snap) != len(Registry()) {
		t.Fatalf("snapshot has %d entries, want %d", len(snap), len(Registry()))
	}
	byKey := map[string]Resolved{}
	for _, r := range snap {
		byKey[r.Key] = r
	}
	pii := byKey["pii_redaction_mode"]
	if pii.Value != "block" || pii.Source != SourceAdmin || pii.Default != "off" || pii.UpdatedBy != "admin@x" {
		t.Errorf("pii resolved = %+v, want admin override block over default off", pii)
	}
	thr := byKey["tool_disclosure_threshold"]
	if thr.Value != "128" || thr.Source != SourceDefault {
		t.Errorf("invalid override should degrade to default: %+v", thr)
	}
	if !thr.Stale {
		t.Error("an ignored override row must be surfaced as Stale so the panel can offer Reset")
	}
	if byKey["error_analysis_enabled"].Value != "true" {
		t.Errorf("untouched setting should report its default")
	}
}

// TestSetValidatesBeforePersistAndApplies: a bad value is rejected as
// ErrInvalidValue with nothing persisted or applied; a good one persists and
// reaches the hook.
func TestSetValidatesBeforePersistAndApplies(t *testing.T) {
	st := newFakeStore()
	applied := map[string]string{}
	svc := newTestService(t, st, applied)
	ctx := context.Background()

	if _, err := svc.Set(ctx, "pii_redaction_mode", "shred", "admin@x"); !errors.Is(err, ErrInvalidValue) {
		t.Fatalf("want ErrInvalidValue, got %v", err)
	}
	if len(st.rows) != 0 || len(applied) != 0 {
		t.Fatal("a rejected value must not persist or apply")
	}

	if _, err := svc.Set(ctx, "nonsense", "true", "admin@x"); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("want ErrUnknownKey, got %v", err)
	}

	r, err := svc.Set(ctx, "pii_redaction_mode", "Observe", "admin@x")
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	if r.Value != "observe" || r.Source != SourceAdmin || r.UpdatedBy != "admin@x" {
		t.Errorf("resolved after set = %+v", r)
	}
	if applied["pii_redaction_mode"] != "observe" {
		t.Errorf("hook saw %q, want observe", applied["pii_redaction_mode"])
	}
	if applied["pii_redaction_mode/override"] != "true" {
		t.Error("an admin Set must apply with override=true")
	}
	if st.rows["pii_redaction_mode"].Value != "observe" {
		t.Errorf("persisted %q, want observe", st.rows["pii_redaction_mode"].Value)
	}
}

// TestResetRevertsToDefault: reset deletes the row and re-applies the env
// default.
func TestResetRevertsToDefault(t *testing.T) {
	st := newFakeStore()
	applied := map[string]string{}
	svc := newTestService(t, st, applied)
	ctx := context.Background()

	if _, err := svc.Set(ctx, "memory_autoindex_enabled", "true", "admin@x"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	r, err := svc.Reset(ctx, "memory_autoindex_enabled", "admin@x")
	if err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if r.Value != "false" || r.Source != SourceDefault {
		t.Errorf("after reset = %+v, want default false", r)
	}
	if applied["memory_autoindex_enabled"] != "false" {
		t.Errorf("hook saw %q after reset, want false", applied["memory_autoindex_enabled"])
	}
	if applied["memory_autoindex_enabled/override"] != "false" {
		t.Error("reset must apply as a DEFAULT (override=false) so env-shadowing hooks clear")
	}
	if _, ok := st.rows["memory_autoindex_enabled"]; ok {
		t.Error("override row should be deleted on reset")
	}
	if _, err := svc.Reset(ctx, "nonsense", "admin@x"); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("want ErrUnknownKey, got %v", err)
	}
}

// TestApplyAllPushesEffectiveValues: boot apply pushes override-or-default for
// every key, and one failing hook doesn't stop the rest.
func TestApplyAllPushesEffectiveValues(t *testing.T) {
	st := newFakeStore()
	st.rows["subagents_enabled"] = store.WorkspaceSetting{Key: "subagents_enabled", Value: "true"}
	applied := map[string]string{}
	hooks := testHooks(applied)
	hooks["auto_title_enabled"] = func(string, bool) error { return fmt.Errorf("boom") }
	svc, err := NewService(st, testDefaults(), hooks)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	err = svc.ApplyAll(context.Background())
	if err == nil || !strings.Contains(err.Error(), "auto_title_enabled") {
		t.Fatalf("ApplyAll should report the failing key, got %v", err)
	}
	if applied["subagents_enabled"] != "true" || applied["subagents_enabled/override"] != "true" {
		t.Errorf("override should apply as override=true, got %q/%q", applied["subagents_enabled"], applied["subagents_enabled/override"])
	}
	if applied["pii_redaction_mode/override"] != "false" {
		t.Error("a default must apply as override=false at boot")
	}
	if applied["error_analysis_enabled"] != "true" || applied["pii_redaction_mode"] != "off" {
		t.Errorf("defaults should apply for un-overridden keys: %v", applied)
	}
}

// TestPersistFailureSurfaces: a store write error surfaces (and the hook does
// not run — nothing was persisted).
func TestPersistFailureSurfaces(t *testing.T) {
	st := newFakeStore()
	st.failSet = fmt.Errorf("db down")
	applied := map[string]string{}
	svc := newTestService(t, st, applied)
	if _, err := svc.Set(context.Background(), "subagents_enabled", "true", "admin@x"); err == nil {
		t.Fatal("want persist error")
	}
	if len(applied) != 0 {
		t.Error("hook must not run when persist fails")
	}
}

// TestRegistryShape: every spec is self-consistent — the UI and validators
// rely on kind-specific fields being present.
func TestRegistryShape(t *testing.T) {
	seen := map[string]bool{}
	for _, s := range Registry() {
		if s.Key == "" || s.EnvVar == "" {
			t.Errorf("spec %+v missing key or env var provenance", s)
		}
		if seen[s.Key] {
			t.Errorf("duplicate key %q", s.Key)
		}
		seen[s.Key] = true
		switch s.Kind {
		case KindEnum:
			if len(s.Enum) < 2 {
				t.Errorf("%s: enum needs options", s.Key)
			}
		case KindInt:
			if s.Min <= 0 || s.Max <= s.Min {
				t.Errorf("%s: int bounds unset or inverted", s.Key)
			}
		case KindBool:
		default:
			t.Errorf("%s: unknown kind %q", s.Key, s.Kind)
		}
	}
}

// TestSetCompensatesOnApplyFailure: a persisted override whose apply hook
// fails is deleted again, so a value that never took effect can't lie in wait
// for the next boot's ApplyAll.
func TestSetCompensatesOnApplyFailure(t *testing.T) {
	st := newFakeStore()
	applied := map[string]string{}
	hooks := testHooks(applied)
	hooks["subagents_enabled"] = func(string, bool) error { return fmt.Errorf("hook down") }
	svc, err := NewService(st, testDefaults(), hooks)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if _, err := svc.Set(context.Background(), "subagents_enabled", "true", "admin@x"); err == nil {
		t.Fatal("want apply error")
	}
	if _, ok := st.rows["subagents_enabled"]; ok {
		t.Error("a failed apply must roll back the persisted row")
	}
}
