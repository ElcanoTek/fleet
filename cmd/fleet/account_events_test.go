package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
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

// TestAccountEventsCreateUserPublishesAChatAccountsOpsGrant: the orchestrator
// admin API's POST /users writes an Ops identity directly. Named like a Chat
// account it changes that account's ops_role and is published (source cli);
// under any other name it publishes nothing. The wrapped handler still reads
// the whole body.
func TestAccountEventsCreateUserPublishesAChatAccountsOpsGrant(t *testing.T) {
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
	if _, err := sched.DB().Conn().ExecContext(ctx, `DELETE FROM users WHERE username LIKE '%@apiuser.test'`); err != nil {
		t.Fatal(err)
	}
	if _, err := chat.CreateUser(ctx, "chat@apiuser.test", "api-password-1"); err != nil {
		t.Fatal(err)
	}
	// Stands in for handlers.CreateUser: decodes the body it was handed and
	// writes the Ops identity.
	createOps := func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Password == "" {
			http.Error(w, "body not passed through", http.StatusBadRequest)
			return
		}
		if err := sched.EnsureAdminUser(r.Context(), body.Username); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusCreated)
	}
	h := accountEventsCreateUser(createOps, accountevents.NewRecorder(chat, sched, chat))
	for _, name := range []string{"chat@apiuser.test", "bot@apiuser.test"} {
		req := httptest.NewRequest(http.MethodPost, "/users",
			strings.NewReader(`{"username":"`+name+`","password":"api-password-1","role":"admin"}`))
		w := httptest.NewRecorder()
		h(w, req)
		if w.Code != http.StatusCreated {
			t.Fatalf("%s: %d %s", name, w.Code, w.Body.String())
		}
	}
	evs, err := chat.ClaimDueAccountEvents(ctx, time.Now().Unix()+1, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Email != "chat@apiuser.test" || evs[0].Source != "cli" || evs[0].OpsRole != "admin" {
		t.Fatalf("events = %+v, want one cli event for the Chat account only", evs)
	}
}

// TestAccountEventsDeliveryStopsAndSignalsDone: the worker's done channel
// closes after ctx ends, which is what shutdown waits on.
func TestAccountEventsDeliveryStopsAndSignalsDone(t *testing.T) {
	chatDsn := os.Getenv("FLEET_TEST_DATABASE_URL")
	if chatDsn == "" {
		t.Skip("FLEET_TEST_DATABASE_URL is required; skipping Postgres-backed test")
	}
	chat, err := store.Open(chatDsn, store.DefaultPoolConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = chat.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	done := startAccountEventsDelivery(ctx, &config.Config{}, chat)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("deliverer did not stop after cancel")
	}
}
