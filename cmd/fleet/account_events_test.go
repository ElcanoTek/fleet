package main

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/accountevents"
	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/sched/storage"
	"github.com/ElcanoTek/fleet/internal/store"
)

func TestAccountEventsRecorderIsNilWhenFeedIsOff(t *testing.T) {
	if rec := accountEventsRecorder(&config.Config{}, nil, nil); rec != nil {
		t.Fatalf("recorder = %v with FLEET_ACCOUNT_EVENTS_URL unset, want nil", rec)
	}
}

// TestSeedBootstrapAdminsPublishesOnlyARealChange: the boot seed that first
// grants a Chat account Ops admin publishes one "system" event; the same seed
// on the next boot is not a change.
func TestSeedBootstrapAdminsPublishesOnlyARealChange(t *testing.T) {
	chatDsn := os.Getenv("FLEET_TEST_DATABASE_URL")
	schedDsn := os.Getenv("DATABASE_URL")
	if chatDsn == "" || schedDsn == "" {
		t.Skip("FLEET_TEST_DATABASE_URL and DATABASE_URL are required; skipping Postgres-backed test")
	}
	ctx := context.Background()
	chat, err := store.Open(chatDsn, store.DefaultPoolConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = chat.Close() })
	if err := chat.TruncateAllForTest(ctx); err != nil {
		t.Fatal(err)
	}
	sched := storage.New()
	if err := sched.Initialize(schedDsn, storage.DefaultPoolConfig()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sched.Close() })
	const email = "seed@acctev.test"
	if _, err := sched.DB().Conn().ExecContext(ctx, `DELETE FROM users WHERE username = $1`, email); err != nil {
		t.Fatal(err)
	}
	if _, err := chat.CreateUser(ctx, email, "seed-password-1"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FLEET_ORCHESTRATOR_BOOTSTRAP_ADMINS", email)
	rec := accountevents.NewRecorder(chat, sched, chat)
	for range 2 {
		if err := seedBootstrapAdmins(sched, rec); err != nil {
			t.Fatal(err)
		}
	}
	evs, err := chat.ClaimDueAccountEvents(ctx, time.Now().Unix()+1, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Source != "system" || evs[0].OpsRole != "admin" || evs[0].ChatRole != "member" {
		t.Fatalf("events = %+v, want one system event with ops admin", evs)
	}
}
