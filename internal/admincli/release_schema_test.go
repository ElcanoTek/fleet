// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package admincli

import (
	"context"
	"crypto/tls"
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/migrations"
	"github.com/ElcanoTek/fleet/internal/release"
	"github.com/ElcanoTek/fleet/internal/testdb"
)

// fakeReleaseSchema is a database whose ledger holds 210..db.
type fakeReleaseSchema struct {
	db    int
	downs []int
}

func (f *fakeReleaseSchema) Applied(context.Context) ([]int, error) {
	var out []int
	for v := 210; v <= f.db; v++ {
		out = append(out, v)
	}
	return out, nil
}

func (f *fakeReleaseSchema) DownTo(_ context.Context, v int) ([]int, error) {
	f.downs = append(f.downs, v)
	var undone []int
	for x := f.db; x > v; x-- {
		undone = append(undone, x)
	}
	f.db = v
	return undone, nil
}

// stopStartOK is a service that stops, starts and is always ready.
type stopStartOK struct{ calls []string }

func (s *stopStartOK) Restart(context.Context, string) error {
	s.calls = append(s.calls, "restart")
	return nil
}
func (s *stopStartOK) Stop(context.Context, string) error {
	s.calls = append(s.calls, "stop")
	return nil
}
func (s *stopStartOK) Start(context.Context, string) error {
	s.calls = append(s.calls, "start")
	return nil
}
func (s *stopStartOK) WaitReady(context.Context, string, time.Duration) error { return nil }

// TestReleaseRollbackMigrateDown: `fleet release rollback` refuses (exit 9,
// nothing changed) to roll back past a migration fleet.prev does not ship
// unless --migrate-down, which undoes those migrations (with this binary's
// down migrations) as part of the rollback.
func TestReleaseRollbackMigrateDown(t *testing.T) {
	t.Setenv("FLEET_UPDATE_DRAIN_FILE", filepath.Join(t.TempDir(), "fleet.env.update.drain")) // never the real one
	installDir := t.TempDir()
	for name, body := range map[string]string{"fleet": "NEW", "fleet.prev": "OLD", "fleet.prev.schema": "210 211 212 213 214\n"} {
		if err := os.WriteFile(filepath.Join(installDir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	schema := &fakeReleaseSchema{db: 215}
	svc := &stopStartOK{}
	oldR, oldP, oldS := installRestarter, newInstallProber, installSchema
	installRestarter = svc
	newInstallProber = func(*tls.Config, io.Writer) release.Prober { return svc }
	installSchema = func() release.Schema { return schema }
	t.Cleanup(func() { installRestarter, newInstallProber, installSchema = oldR, oldP, oldS })
	args := func(extra ...string) []string {
		return append([]string{"--install-dir", installDir, "--health-url", "http://127.0.0.1:1/readyz", "--health-timeout", "1s"}, extra...)
	}

	if rc := releaseRollback(context.Background(), args()); rc != releaseExitMigrateDown {
		t.Fatalf("rollback without --migrate-down rc=%d, want %d", rc, releaseExitMigrateDown)
	}
	if b, _ := os.ReadFile(filepath.Join(installDir, "fleet")); string(b) != "NEW" || schema.db != 215 || len(svc.calls) != 0 {
		t.Fatalf("live %s db %d calls %v: the refusal changed something", b, schema.db, svc.calls)
	}

	if rc := releaseRollback(context.Background(), args("--migrate-down")); rc != 0 {
		t.Fatalf("rollback --migrate-down rc=%d", rc)
	}
	if b, _ := os.ReadFile(filepath.Join(installDir, "fleet")); string(b) != "OLD" || schema.db != 214 || !reflect.DeepEqual(schema.downs, []int{214}) {
		t.Fatalf("live %s db %d downs %v", b, schema.db, schema.downs)
	}
	if b, _ := os.ReadFile(filepath.Join(installDir, "fleet.prev.schema")); string(b) != "210 211 212 213 214 215\n" {
		t.Errorf("record of the new fleet.prev = %q", b)
	}
}

// TestReleaseSchemaDSNFollowsTheServer: a box that sets no DATABASE_URL and
// no DB_* (the server then connects to localhost:5432 as fleet/fleet, and
// the update's idle gate does the same) must not have every update refused
// for "DSN unset": the release schema resolves the database exactly as the
// server does, defaults included.
func TestReleaseSchemaDSNFollowsTheServer(t *testing.T) {
	envFile := filepath.Join(t.TempDir(), "fleet.env")
	if err := os.WriteFile(envFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FLEET_ENV_FILE", envFile)
	for _, k := range []string{"DATABASE_URL", "DB_HOST", "DB_PORT", "DB_USER", "DB_PASSWORD", "DB_NAME", "DB_SSLMODE"} {
		t.Setenv(k, "")
	}
	resetEnvFileCache()
	t.Cleanup(resetEnvFileCache)

	if _, err := chatDSN(""); err == nil {
		t.Fatal("chatDSN accepted the bare defaults; this test no longer shows the difference")
	}
	got, err := releaseSchemaDSN()
	if err != nil {
		t.Fatal(err)
	}
	want, err := config.ResolveDSNs(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if got != want.Chat || got == "" {
		t.Fatalf("releaseSchemaDSN = %q, want the server's default %q", got, want.Chat)
	}
}

// TestReleaseSchemaOnADatabase drives the adapter fleet release install uses
// against a real database: Applied lists the ledger, DownTo undoes the
// versions above its target (bounded) and reports them.
func TestReleaseSchemaOnADatabase(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := testdb.Empty(t)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := migrations.Up(ctx, db); err != nil {
		t.Fatal(err)
	}
	s := releaseSchema{dsn: config.WithSchema(dsn, config.ChatSchema)}
	vs, err := s.Applied(ctx)
	if err != nil || len(vs) == 0 || vs[len(vs)-1] < migrations.FirstNew {
		t.Fatalf("Applied = %v, %v", vs, err)
	}
	top := vs[len(vs)-1]
	undone, err := s.DownTo(ctx, top-1)
	if err != nil || !reflect.DeepEqual(undone, []int{top}) {
		t.Fatalf("DownTo(%d) = %v, %v", top-1, undone, err)
	}
	after, err := s.Applied(ctx)
	if err != nil || !reflect.DeepEqual(after, vs[:len(vs)-1]) {
		t.Fatalf("Applied after DownTo = %v, %v", after, err)
	}
	if err := migrations.Up(ctx, db); err != nil {
		t.Fatalf("up again: %v", err)
	}
}
