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
		"pii_redaction_engine":              "pattern",
		"pii_rampart_url":                   "",
		"guardrail_url":                     "",
		"guardrail_mode":                    "off",
		"tool_disclosure_threshold":         "128",
		"max_tool_output_bytes":             "65536",
		"approval_timeout_seconds":          "3600",
		"phone_a_friend_enabled":            "false",
		"subagents_enabled":                 "true",
		"default_model":                     "google/gemini-3.7-flash",
		"advanced_model":                    "openai/gpt-5.6-sol",
		"memory_autoindex_enabled":          "false",
		"error_analysis_enabled":            "true",
		"auto_title_enabled":                "true",
		"connector_recommendations_enabled": "false",
		"context_handles_enabled":           "false",
		"shared_files_max_total_mb":         "10240",
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
	defaults["max_tool_output_bytes"] = "512" // generic service preserves an out-of-bounds env default
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
		{"max_tool_output_bytes", "0", "0", false}, // MinZeroOK: 0 = safe runtime default
		{"pii_redaction_engine", "Rampart", "rampart", false},
		{"pii_redaction_engine", "onnx", "", true},
		{"pii_rampart_url", "", "", false}, // empty = not configured
		{"pii_rampart_url", "http://127.0.0.1:8787/v1/redact", "http://127.0.0.1:8787/v1/redact", false},
		{"pii_rampart_url", "ftp://x", "", true},
		{"pii_rampart_url", "not a url", "", true},
		{"max_tool_output_bytes", "512", "", true}, // below Min and not 0
		{"max_tool_output_bytes", "65536", "65536", false},
		{"max_tool_output_bytes", "131072", "131072", false},
		{"max_tool_output_bytes", "131073", "", true},
		// KindModel: any provider/model slug, case preserved (slugs are
		// case-sensitive upstream), whitespace trimmed but never internal.
		{"default_model", "openai/gpt-5.6-sol", "openai/gpt-5.6-sol", false},
		{"default_model", "  myBedrock/anthropic.claude-opus-5  ", "myBedrock/anthropic.claude-opus-5", false},
		{"advanced_model", "google/gemini-3.7-flash", "google/gemini-3.7-flash", false},
		{"default_model", "", "", true},                                                         // a tier always has a value
		{"default_model", "gpt-5.6-sol", "", true},                                              // no provider half
		{"default_model", "/gpt-5.6-sol", "", true},                                             // empty provider half
		{"default_model", "openai/", "", true},                                                  // empty model half
		{"default_model", "openai/gpt 5", "", true},                                             // internal whitespace
		{"advanced_model", strings.Repeat("a", 100) + "/" + strings.Repeat("b", 101), "", true}, // over 200 chars
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
		case KindBool, KindURL, KindModel:
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

// TestResetCompensatesOnApplyFailure: resetting a key whose DEFAULT cannot
// apply (e.g. clearing a rampart URL the engine still needs) restores the
// deleted row, so DB and live state never diverge across a restart.
func TestResetCompensatesOnApplyFailure(t *testing.T) {
	st := newFakeStore()
	applied := map[string]string{}
	hooks := testHooks(applied)
	hooks["pii_rampart_url"] = func(v string, _ bool) error {
		if v == "" {
			return fmt.Errorf("engine still needs a URL")
		}
		return nil
	}
	svc, err := NewService(st, testDefaults(), hooks)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	ctx := context.Background()
	if _, err := svc.Set(ctx, "pii_rampart_url", "http://127.0.0.1:8787/v1/redact", "admin@x"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if _, err := svc.Reset(ctx, "pii_rampart_url", "admin@x"); err == nil {
		t.Fatal("reset should fail when the default cannot apply")
	}
	row, ok := st.rows["pii_rampart_url"]
	if !ok || row.Value != "http://127.0.0.1:8787/v1/redact" {
		t.Fatalf("failed reset must restore the previous row, got %+v (present=%v)", row, ok)
	}
}

// TestApplyAllRecordsPerKeyErrorsAndSnapshotSurfacesThem: a boot-apply
// failure marks the key (Resolved.ApplyError) instead of hiding it, and a
// later successful ApplyAll clears the mark.
func TestApplyAllRecordsPerKeyErrorsAndSnapshotSurfacesThem(t *testing.T) {
	st := newFakeStore()
	st.rows["subagents_enabled"] = store.WorkspaceSetting{Key: "subagents_enabled", Value: "true"}
	applied := map[string]string{}
	hooks := testHooks(applied)
	broken := true
	hooks["subagents_enabled"] = func(string, bool) error {
		if broken {
			return fmt.Errorf("boom")
		}
		return nil
	}
	svc, err := NewService(st, testDefaults(), hooks)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	ctx := context.Background()
	if err := svc.ApplyAll(ctx); err == nil || errors.Is(err, ErrLoadFailed) {
		t.Fatalf("per-key failure should be a plain error, not ErrLoadFailed: %v", err)
	}
	snap, err := svc.Snapshot(ctx)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	found := false
	for _, r := range snap {
		if r.Key == "subagents_enabled" {
			found = true
			if r.ApplyError == "" {
				t.Error("failed key must surface ApplyError")
			}
		}
	}
	if !found {
		t.Fatal("missing key")
	}

	broken = false
	if err := svc.ApplyAll(ctx); err != nil {
		t.Fatalf("second ApplyAll: %v", err)
	}
	snap, _ = svc.Snapshot(ctx)
	for _, r := range snap {
		if r.Key == "subagents_enabled" && r.ApplyError != "" {
			t.Error("recovered key must clear ApplyError")
		}
	}
}

// TestApplyAllLoadFailureIsTyped: a store read failure wraps ErrLoadFailed so
// the boot wiring can distinguish "cannot render truthfully" (501) from
// per-key degradation (panel stays up).
func TestApplyAllLoadFailureIsTyped(t *testing.T) {
	st := newFakeStore()
	st.failGet = fmt.Errorf("db down")
	applied := map[string]string{}
	svc := newTestService(t, st, applied)
	if err := svc.ApplyAll(context.Background()); !errors.Is(err, ErrLoadFailed) {
		t.Fatalf("want ErrLoadFailed, got %v", err)
	}
}

// TestDependentKeyHealsAfterFix: a key that failed to apply at boot (rampart
// engine with no URL) recovers automatically when the setting it depends on
// is fixed — no reboot, no redundant re-save of the failed key.
func TestDependentKeyHealsAfterFix(t *testing.T) {
	st := newFakeStore()
	st.rows["pii_redaction_engine"] = store.WorkspaceSetting{Key: "pii_redaction_engine", Value: "rampart"}
	applied := map[string]string{}
	hooks := testHooks(applied)
	var url string
	hooks["pii_rampart_url"] = func(v string, _ bool) error { url = v; return nil }
	hooks["pii_redaction_engine"] = func(v string, _ bool) error {
		if v == "rampart" && url == "" {
			return fmt.Errorf("needs a URL")
		}
		applied["pii_redaction_engine"] = v
		return nil
	}
	svc, err := NewService(st, testDefaults(), hooks)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	ctx := context.Background()

	// Boot: the engine row fails (no URL yet) and is marked.
	if err := svc.ApplyAll(ctx); err == nil {
		t.Fatal("boot apply should report the failing engine")
	}

	// The admin fixes the dependency — the engine heals without being touched.
	if _, err := svc.Set(ctx, "pii_rampart_url", "http://127.0.0.1:8787/v1/redact", "admin@x"); err != nil {
		t.Fatalf("Set url: %v", err)
	}
	if applied["pii_redaction_engine"] != "rampart" {
		t.Fatal("fixing the URL should re-apply the failed engine setting")
	}
	snap, _ := svc.Snapshot(ctx)
	for _, r := range snap {
		if r.Key == "pii_redaction_engine" && r.ApplyError != "" {
			t.Errorf("healed key must clear ApplyError: %+v", r)
		}
	}
}
