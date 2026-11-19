package admincli

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/identity"
	"github.com/ElcanoTek/fleet/internal/sched/storage"
	"github.com/ElcanoTek/fleet/internal/store"
	"github.com/ElcanoTek/fleet/internal/testdb/fleetdb"
)

// A legacy bundle's scheduler user whose username holds a look-alike of an
// ASCII letter (U+212A KELVIN SIGN for k) is refused, not merged into the
// ASCII account its lowercase names: that account keeps its access and gets
// none of the bundle user's tasks.
func TestImportSchedUsersRefusesLookalikeUsername(t *testing.T) {
	d := fleetdb.New(t)
	st := storage.New()
	st.SetDatabase(d.SchedDB(t))
	ctx := context.Background()
	kate, err := st.Identity().CreateAccount(ctx, identity.NewAccount{Email: "kate@x.com"})
	if err != nil {
		t.Fatal(err)
	}
	bundleID := uuid.New()
	stats := newImportStats()
	remap := importSchedUsers(ctx, st, []bundleSchedUser{{
		ID: bundleID, Username: "\u212aate@x.com", PasswordHash: "$2a$10$abcdefghijklmnopqrstuvABCDEFGHIJKLMNOPQRSTUV1234567890",
		Role: "admin", CreatedAt: time.Now().UTC(),
	}}, false, stats)
	if got, ok := remap[bundleID]; ok {
		t.Fatalf("the look-alike user was remapped onto %s", got)
	}
	if stats.errors == 0 {
		t.Fatal("the look-alike user was not reported")
	}
	got, err := st.Identity().GetByEmail(ctx, "kate@x.com")
	if err != nil || got.ID != kate.ID || got.OpsScope != identity.OpsNone {
		t.Fatalf("kate after the import = %+v, %v; want her account untouched", got, err)
	}
}

// fleet sched user add refuses a look-alike username before it opens the
// database, instead of creating, or promoting, the ASCII account.
func TestSchedUserAddRefusesLookalikeUsername(t *testing.T) {
	if code := schedUserAdd([]string{"\u212aate@x.com", "--password", "password-long-enough"}); code == 0 {
		t.Fatal("sched user add accepted a look-alike username")
	}
}

// The chat import's dry run predicts the real run's refusals: a user, a
// conversation and a memory owned by an email the real run refuses (here a
// KELVIN SIGN look-alike) are reported as errors, not counted as created.
func TestChatImportDryRunPredictsOwnerRefusals(t *testing.T) {
	d := fleetdb.New(t)
	chatStore := d.Chat(t)
	const lookalike = "\u212aate@x.com"
	sec := &chatSection{
		Users:         []store.ImportedUser{{Email: lookalike, PasswordHash: "$2a$10$abcdefghijklmnopqrstuvABCDEFGHIJKLMNOPQRSTUV1234567890"}},
		Conversations: []store.ImportedConversation{{ID: uuid.NewString(), UserEmail: lookalike, Title: "t"}},
		Memories:      []store.ImportedMemory{{ID: uuid.NewString(), UserEmail: lookalike, Content: "c", Source: "manual"}},
	}
	for _, dryRun := range []bool{true, false} {
		stats := newImportStats()
		if err := importChatSection(context.Background(), chatStore, sec, dryRun, stats); err != nil {
			t.Fatalf("dry run %v: %v", dryRun, err)
		}
		if stats.errors != 3 || stats.created["chat users"]+stats.created["conversations"]+stats.created["memories"] != 0 {
			t.Errorf("dry run %v: errors %d, created %v; want the three rows refused and none created", dryRun, stats.errors, stats.created)
		}
	}
}
