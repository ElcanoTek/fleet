package admincli

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/identity"
	"github.com/ElcanoTek/fleet/internal/sched/apikeys"
	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/sched/storage"
)

// `fleet sched budget create` runs the same principal check as POST
// /admin/budgets (#2105 QA): a mistyped account email or an unknown key_id
// used to save a budget that listed as configured and bounded nothing. Now an
// unknown principal exits 2 (not found, like the other sched verbs), a key
// that cannot create tasks exits 1, nothing is saved, and an email in any case
// is stored as the account's own (lowercase) email so the budget binds.
func TestSchedBudgetCreateRequiresExistingPrincipal(t *testing.T) {
	d := fleetEnv(t)
	dataDir := t.TempDir()
	t.Setenv("FLEET_DATA_DIR", dataDir) // the key store the CLI checks key budgets against

	ctx := context.Background()
	sched := storage.New()
	sched.SetDatabase(d.SchedDB(t))
	if _, err := sched.Identity().CreateAccount(ctx, identity.NewAccount{
		ID: uuid.New(), Email: "alice@budget.test", Role: identity.RoleViewer, OpsScope: identity.OpsNone,
	}); err != nil {
		t.Fatal(err)
	}
	mgr, err := apikeys.NewManager(filepath.Join(dataDir, "api_keys.json"), filepath.Join(dataDir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	key, _, err := mgr.CreateKey(ctx, "ci", []models.Permission{models.PermissionCreateTask}, nil, 0, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	revoked, _, err := mgr.CreateKey(ctx, "old", []models.Permission{models.PermissionCreateTask}, nil, 0, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.RevokeKey(ctx, revoked.KeyID); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		args []string
		want int
	}{
		{[]string{"--scope", "user", "--principal", "alcie@budget.test", "--window", "day", "--hard-usd", "1"}, 2},
		{[]string{"--scope", "key", "--principal", "key_doesnotexist", "--window", "day", "--hard-usd", "1"}, 2},
		{[]string{"--scope", "key", "--principal", revoked.KeyID, "--window", "day", "--hard-usd", "1"}, 1},
	} {
		if code := schedBudgetCreate(tc.args); code != tc.want {
			t.Errorf("%v: exit %d, want %d", tc.args, code, tc.want)
		}
	}
	if got, err := sched.ListBudgets(ctx); err != nil || len(got) != 0 {
		t.Fatalf("refused budgets must not be saved: %d rows (err %v)", len(got), err)
	}

	for _, args := range [][]string{
		{"--scope", "user", "--principal", "Alice@Budget.TEST", "--window", "day", "--hard-usd", "1"},
		{"--scope", "key", "--principal", key.KeyID, "--window", "day", "--hard-usd", "1"},
	} {
		if code := schedBudgetCreate(args); code != 0 {
			t.Errorf("%v: exit %d, want 0", args, code)
		}
	}
	got, err := sched.ListBudgets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	principals := map[string]bool{}
	for _, b := range got {
		principals[b.PrincipalID] = true
	}
	if len(got) != 2 || !principals["alice@budget.test"] || !principals[key.KeyID] {
		t.Fatalf("saved principals = %v, want alice@budget.test and %s", principals, key.KeyID)
	}
}

// Off the box (or from a directory with no unit and no FLEET_DATA_DIR) the CLI
// opens a key store that does not exist, which reads as empty: every real key
// would then be "unknown". It says the store is empty instead, and saves
// nothing.
func TestSchedBudgetCreateEmptyKeyStore(t *testing.T) {
	d := fleetEnv(t)
	t.Setenv("FLEET_DATA_DIR", t.TempDir()) // no api_keys.json here

	sched := storage.New()
	sched.SetDatabase(d.SchedDB(t))
	var code int
	out := captureStderr(t, func() {
		code = schedBudgetCreate([]string{"--scope", "key", "--principal", "key_real_elsewhere", "--window", "day", "--hard-usd", "1"})
	})
	if code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	if !strings.Contains(out, "holds no API keys") || !strings.Contains(out, "FLEET_DATA_DIR") {
		t.Errorf("stderr should explain the empty key store, got:\n%s", out)
	}
	if got, err := sched.ListBudgets(context.Background()); err != nil || len(got) != 0 {
		t.Fatalf("nothing should be saved: %d rows (err %v)", len(got), err)
	}
}
