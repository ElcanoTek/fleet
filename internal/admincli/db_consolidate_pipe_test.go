// LEGACY-DB-LAYOUT: remove after consolidation — this whole file.

package admincli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeClientTools puts a pg_dump and a pg_restore built from the given shell
// bodies first on PATH.
func fakeClientTools(t *testing.T, dump, restore string) {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{"pg_dump": dump, "pg_restore": restore} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestDumpRestoreDoesNotHangWhenRestoreExitsEarly (#2055 review): a
// pg_restore that dies at once must not leave pg_dump blocked on a full
// pipe; the call returns promptly with the restore's error as the diagnostic.
func TestDumpRestoreDoesNotHangWhenRestoreExitsEarly(t *testing.T) {
	// 64 MiB is far more than any pipe buffer holds.
	fakeClientTools(t,
		`head -c 67108864 /dev/zero`,
		`echo "pg_restore: error: could not execute query: boom" >&2; exit 3`)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	start := time.Now()
	err := (&execPG{}).DumpRestore(ctx, "src", "chat", "dst", "owner")
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Fatalf("DumpRestore took %s: pg_dump blocked on the pipe after pg_restore exited", elapsed)
	}
	if err == nil || !strings.HasPrefix(err.Error(), "pg_restore into dst") || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v, want the restore's failure as the diagnostic", err)
	}
	if ctx.Err() != nil {
		t.Fatal("returned only because the context expired")
	}
}

// TestDumpRestoreStreamsTheWholeDump: the happy path still hands pg_restore
// every byte and reports success; a failing pg_dump with a reading restore
// reports pg_dump.
func TestDumpRestoreStreamsTheWholeDump(t *testing.T) {
	fakeClientTools(t,
		`head -c 8388608 /dev/zero`,
		`n=$(wc -c); [ "$n" -eq 8388608 ] || { echo "got $n bytes" >&2; exit 1; }`)
	if err := (&execPG{}).DumpRestore(t.Context(), "src", "chat", "dst", "owner"); err != nil {
		t.Fatalf("DumpRestore = %v", err)
	}
	fakeClientTools(t, `echo "pg_dump: error: no such schema" >&2; exit 1`, `cat >/dev/null`)
	if err := (&execPG{}).DumpRestore(t.Context(), "src", "chat", "dst", "owner"); err == nil || !strings.Contains(err.Error(), "no such schema") {
		t.Fatalf("DumpRestore = %v, want pg_dump's failure", err)
	}
}
