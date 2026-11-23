package apikeys

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// A user key (ADR-0108) names the account it acts as on the chat API, and
// carries no Operations Center scope at all.
func TestUserKey(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	m, err := NewManager(filepath.Join(dir, "keys.json"), filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.CreateUserKey(ctx, "cli", "  ", 0, nil, ""); err == nil {
		t.Fatal("a user key without an account was minted")
	}
	if _, _, err := m.CreateTypedKey(ctx, "cli", KeyTypeUser, nil, 0, nil, ""); err == nil {
		t.Fatal("CreateTypedKey minted a user key with no account")
	}
	key, raw, err := m.CreateUserKey(ctx, "cli", " Me@Example.com ", 0, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if key.Type != KeyTypeUser || key.UserEmail != "me@example.com" || len(key.Permissions) != 0 {
		t.Fatalf("key = %+v", key)
	}
	if kt, _, err := ParseAPIKey(raw); err != nil || kt != KeyTypeUser {
		t.Fatalf("ParseAPIKey = %q, %v", kt, err)
	}
	ok, got, _ := m.ValidateKey(ctx, raw, nil, nil, nil)
	if !ok || got.UserEmail != "me@example.com" {
		t.Fatalf("ValidateKey = %v %+v", ok, got)
	}
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		if TypeAllowsMethod(KeyTypeUser, method) {
			t.Errorf("a user key may %s a scoped-key route", method)
		}
	}
	if resp := key.ToResponse(); resp.UserEmail != "me@example.com" || resp.Type != "user" {
		t.Errorf("response = %+v", resp)
	}
	// Rotation keeps the account.
	rotated, _, err := m.RotateKey(ctx, key.KeyID, 1)
	if err != nil || rotated.UserEmail != "me@example.com" || rotated.Type != KeyTypeUser {
		t.Fatalf("rotate = %+v, %v", rotated, err)
	}
}

// Deleting an account deletes the user keys that act as it — a rotated key's
// outgoing token too — and nothing else: another account's user key and keys
// that name no account stay, in this Manager and in one that shares the file.
func TestDeleteUserKeys(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "keys.json")
	m, err := NewManager(path, filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	other, err := NewManager(path, filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	gone, goneRaw, err := m.CreateUserKey(ctx, "cli", "gone@x.com", 0, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	_, rotatedIn, err := m.RotateKey(ctx, gone.KeyID, 24)
	if err != nil {
		t.Fatal(err)
	}
	_, secondRaw, err := m.CreateUserKey(ctx, "acp", "GONE@x.com", 0, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	_, stayRaw, err := m.CreateUserKey(ctx, "cli", "stay@x.com", 0, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	_, taskRaw, err := m.CreateTypedKey(ctx, "intake", KeyTypeTask, nil, 0, nil, "")
	if err != nil {
		t.Fatal(err)
	}

	noAccount := func(context.Context) (bool, error) { return false, nil }
	n, err := m.DeleteUserKeys(ctx, " Gone@X.com ", "gone-id", noAccount)
	if err != nil || n != 2 {
		t.Fatalf("DeleteUserKeys = %d, %v; want 2 keys", n, err)
	}
	for _, mgr := range []*Manager{m, other} {
		for name, raw := range map[string]string{"rotated-out token": goneRaw, "rotated-in token": rotatedIn, "second key": secondRaw} {
			if ok, _, _ := mgr.ValidateKey(ctx, raw, nil, nil, nil); ok {
				t.Errorf("the deleted account's %s still authenticates", name)
			}
		}
		for name, raw := range map[string]string{"another account's user key": stayRaw, "task key": taskRaw} {
			if ok, _, reason := mgr.ValidateKey(ctx, raw, nil, nil, nil); !ok {
				t.Errorf("%s was removed with the account: %s", name, reason)
			}
		}
	}
	if n, err := m.DeleteUserKeys(ctx, "gone@x.com", "gone-id", noAccount); err != nil || n != 0 {
		t.Errorf("second DeleteUserKeys = %d, %v; want 0, nil", n, err)
	}
}

// DeleteUserKeys is limited to the deleted account: a key bound to it goes; a
// key bound to a later account under the same address stays; and a key that
// names no account goes only while no account holds the address.
func TestDeleteUserKeysSparesALaterAccount(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	m, err := NewManager(filepath.Join(dir, "keys.json"), filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	old, _, err := m.CreateBoundUserKey(ctx, "old", "back@x.com", "old-id", 0, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	later, _, err := m.CreateBoundUserKey(ctx, "later", "back@x.com", "later-id", 0, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	unbound, _, err := m.CreateUserKey(ctx, "legacy", "back@x.com", 0, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	asked := 0
	held := func(context.Context) (bool, error) { asked++; return true, nil }
	if n, err := m.DeleteUserKeys(ctx, "back@x.com", "old-id", held); err != nil || n != 1 {
		t.Fatalf("DeleteUserKeys with the address held = %d, %v; want the old account's key only", n, err)
	}
	if asked != 1 {
		t.Errorf("the address was asked about %d times; want once", asked)
	}
	if m.GetKey(old.KeyID) != nil || m.GetKey(later.KeyID) == nil || m.GetKey(unbound.KeyID) == nil {
		t.Fatalf("old %v, later %v, unbound %v; want only the old account's key gone",
			m.GetKey(old.KeyID) != nil, m.GetKey(later.KeyID) != nil, m.GetKey(unbound.KeyID) != nil)
	}
	free := func(context.Context) (bool, error) { return false, nil }
	if n, err := m.DeleteUserKeys(ctx, "back@x.com", "old-id", free); err != nil || n != 1 || m.GetKey(unbound.KeyID) != nil {
		t.Fatalf("DeleteUserKeys with the address free = %d, %v; want the unbound key gone", n, err)
	}
	if m.GetKey(later.KeyID) == nil {
		t.Fatal("a key bound to a later account was deleted")
	}
}

// A bound user key records its account at mint; one for no account is not
// minted.
func TestCreateBoundUserKey(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	m, err := NewManager(filepath.Join(dir, "keys.json"), filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.CreateBoundUserKey(ctx, "cli", "u@x.com", " ", 0, nil, ""); !errors.Is(err, ErrNoAccount) {
		t.Fatalf("no account id: err = %v, want ErrNoAccount", err)
	}
	if n := len(m.ListKeys()); n != 0 {
		t.Fatalf("%d keys minted without an account", n)
	}
	key, raw, err := m.CreateBoundUserKey(ctx, "cli", "U@x.com", "acct-1", 0, nil, "")
	if err != nil || key.AccountID != "acct-1" || key.UserEmail != "u@x.com" || key.AccountMissing {
		t.Fatalf("CreateBoundUserKey = %+v, %v", key, err)
	}
	reloaded, err := NewManager(filepath.Join(dir, "keys.json"), filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if ok, got, _ := reloaded.ValidateKey(ctx, raw, nil, nil, nil); !ok || got.AccountID != "acct-1" {
		t.Fatalf("the account id did not persist: %+v", got)
	}
	if err := m.RecordUserKeyAccount(ctx, key.KeyID, "acct-1"); err != nil {
		t.Errorf("recording the same account again: %v", err)
	}
	if err := m.RecordUserKeyAccount(ctx, key.KeyID, "acct-2"); err == nil {
		t.Error("a bound key was bound to another account")
	}
}

// Keys minted before keys named their account are bound once, at boot: to the
// account their email has then, or marked as having none — and a key marked
// so is not bound later, when an account appears under its email.
func TestBindUserKeys(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	m, err := NewManager(filepath.Join(dir, "keys.json"), filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	has, _, _ := m.CreateUserKey(ctx, "a", "has@x.com", 0, nil, "")
	gone, _, _ := m.CreateUserKey(ctx, "b", "gone@x.com", 0, nil, "")
	flaky, _, _ := m.CreateUserKey(ctx, "c", "flaky@x.com", 0, nil, "")
	bound, _, _ := m.CreateBoundUserKey(ctx, "d", "has@x.com", "acct-old", 0, nil, "")
	accounts := map[string]string{"has@x.com": "acct-has"}
	lookup := func(_ context.Context, email string) (string, bool, error) {
		if email == "flaky@x.com" {
			return "", false, errors.New("db down")
		}
		id, ok := accounts[email]
		return id, ok, nil
	}
	b, missing, err := m.BindUserKeys(ctx, lookup)
	if err != nil || b != 1 || missing != 1 {
		t.Fatalf("BindUserKeys = %d bound, %d missing, %v; want 1, 1", b, missing, err)
	}
	if k := m.GetKey(has.KeyID); k.AccountID != "acct-has" || k.AccountMissing {
		t.Errorf("key for an account: %+v", k)
	}
	if k := m.GetKey(gone.KeyID); k.AccountID != "" || !k.AccountMissing {
		t.Errorf("key for no account: %+v", k)
	}
	if k := m.GetKey(flaky.KeyID); k.AccountID != "" || k.AccountMissing {
		t.Errorf("a key whose lookup failed was decided: %+v", k)
	}
	if k := m.GetKey(bound.KeyID); k.AccountID != "acct-old" {
		t.Errorf("an already bound key was rebound: %+v", k)
	}
	// The address gets an account later: the key marked as having none stays so.
	accounts["gone@x.com"] = "acct-new"
	if b, missing, err := m.BindUserKeys(ctx, lookup); err != nil || b != 0 || missing != 0 {
		t.Fatalf("second BindUserKeys = %d, %d, %v; want nothing to do", b, missing, err)
	}
	if k := m.GetKey(gone.KeyID); k.AccountID != "" || !k.AccountMissing {
		t.Errorf("a key marked as having no account was bound later: %+v", k)
	}
}

// Two processes writing the key store at once (here two Managers on one file,
// as the server and a `fleet sched apikey` CLI are) lose no key: every
// read-modify-write holds the file's lock and re-reads the file under it.
func TestConcurrentWritersLoseNoKey(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "keys.json")
	a, err := NewManager(path, filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewManager(path, filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	const each = 25
	var wg sync.WaitGroup
	for _, m := range []*Manager{a, b} {
		wg.Add(1)
		go func(m *Manager) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				if _, _, err := m.CreateTypedKey(ctx, "k", KeyTypeTask, nil, 0, nil, ""); err != nil {
					t.Error(err)
					return
				}
			}
		}(m)
	}
	wg.Wait()
	fresh, err := NewManager(path, filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if n := len(fresh.ListKeys()); n != 2*each {
		t.Fatalf("%d keys in the store, want %d", n, 2*each)
	}
	if leftovers, _ := filepath.Glob(filepath.Join(dir, "*.tmp")); len(leftovers) != 0 {
		t.Errorf("temp files left behind: %v", leftovers)
	}
}

// A user key bound at mint is written once, already bound.
func TestCreateBoundUserKeyWritesOnce(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	audit := filepath.Join(dir, "audit.jsonl")
	m, err := NewManager(filepath.Join(dir, "keys.json"), audit)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.CreateBoundUserKey(ctx, "cli", "u@x.com", "acct-1", 0, nil, ""); err != nil {
		t.Fatal(err)
	}
	if entries := m.GetAuditLog(nil, nil, nil, 0); len(entries) != 1 || entries[0]["action"] != "create_key" {
		t.Fatalf("audit = %v, want one create_key (no separate binding write)", entries)
	}
}

// Another process holding the key file's lock (a stopped CLI, say) stalls a
// write only for the bounded wait, which then fails clearly, and never stalls
// authentication.
func TestHeldFileLockBoundsWritesAndNeverBlocksValidation(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "keys.json")
	m, err := NewManager(path, filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	m.lockWait = 200 * time.Millisecond
	_, raw, err := m.CreateTypedKey(ctx, "k", KeyTypeTask, nil, 0, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	holder, err := os.Open(path + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if err := unix.Flock(int(holder.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, _, err := m.CreateTypedKey(ctx, "k2", KeyTypeTask, nil, 0, nil, ""); !errors.Is(err, ErrStoreLocked) {
		t.Fatalf("a write with the lock held elsewhere: err = %v, want ErrStoreLocked", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("the write waited %v", took)
	}
	start = time.Now()
	if ok, _, reason := m.ValidateKey(ctx, raw, nil, nil, nil); !ok {
		t.Fatalf("validation with the lock held elsewhere: %s", reason)
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("validation waited %v for the file lock", took)
	}
}

// The lock file is readable by every user whatever the umask, so the service
// can lock one a root-run CLI created; one that cannot be opened at all lets
// writes go on without the cross-process lock.
func TestLockFileModeAndDegradedLocking(t *testing.T) {
	ctx := context.Background()
	old := unix.Umask(0o077)
	dir := t.TempDir()
	m, err := NewManager(filepath.Join(dir, "keys.json"), filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		unix.Umask(old)
		t.Fatal(err)
	}
	_, _, err = m.CreateTypedKey(ctx, "k", KeyTypeTask, nil, 0, nil, "")
	unix.Umask(old)
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(dir, "keys.json.lock"))
	if err != nil {
		t.Fatal(err)
	}
	if mode := st.Mode().Perm(); mode != 0o644 {
		t.Errorf("lock file mode %o, want 644", mode)
	}

	if os.Geteuid() == 0 {
		t.Skip("root opens any file: the unopenable lock file cannot be made")
	}
	dir2 := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir2, "keys.json.lock"), nil, 0o000); err != nil {
		t.Fatal(err)
	}
	m2, err := NewManager(filepath.Join(dir2, "keys.json"), filepath.Join(dir2, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := m2.CreateTypedKey(ctx, "k", KeyTypeTask, nil, 0, nil, ""); err != nil {
		t.Fatalf("a write with an unopenable lock file: %v", err)
	}
}

// Recording "no account" on a key already recorded so is a no-op, so two
// first uses of a key whose email has no account both answer the refusal.
func TestRecordNoAccountTwice(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	m, err := NewManager(filepath.Join(dir, "keys.json"), filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	key, _, _ := m.CreateUserKey(ctx, "k", "none@x.com", 0, nil, "")
	for i := 0; i < 2; i++ {
		if err := m.RecordUserKeyAccount(ctx, key.KeyID, ""); err != nil {
			t.Fatalf("record no account (%d): %v", i+1, err)
		}
	}
	if err := m.RecordUserKeyAccount(ctx, key.KeyID, "acct-1"); err == nil {
		t.Fatal("a key recorded as having no account was bound later")
	}
}

// A write waiting for another process's lock on the key file holds nothing a
// key check needs: validation answers at once meanwhile.
func TestValidationWhileAWriterWaits(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "keys.json")
	m, err := NewManager(path, filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	_, raw, err := m.CreateTypedKey(ctx, "k", KeyTypeTask, nil, 0, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	m.SetFileLockWait(10 * time.Second)
	holder, err := os.Open(path + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if err := unix.Flock(int(holder.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	wrote := make(chan error, 1)
	go func() {
		_, _, err := m.CreateTypedKey(ctx, "k2", KeyTypeTask, nil, 0, nil, "")
		wrote <- err
	}()
	time.Sleep(100 * time.Millisecond) // the writer is now waiting for the file lock
	for i := 0; i < 3; i++ {
		start := time.Now()
		if ok, _, reason := m.ValidateKey(ctx, raw, nil, nil, nil); !ok {
			t.Fatalf("validation while a writer waits: %s", reason)
		}
		if took := time.Since(start); took > 500*time.Millisecond {
			t.Fatalf("validation waited %v behind a waiting writer", took)
		}
		_ = m.ListKeys()
	}
	_ = unix.Flock(int(holder.Fd()), unix.LOCK_UN)
	if err := <-wrote; err != nil {
		t.Fatalf("the waiting write, once the lock was free: %v", err)
	}
}

// Using a rotated-out token past its grace retires it, and the retirement is
// saved to the file.
func TestRotatedTokenRetirementIsSaved(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "keys.json")
	m, err := NewManager(path, filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	key, old, err := m.CreateTypedKey(ctx, "k", KeyTypeTask, nil, 0, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.RotateKey(ctx, key.KeyID, 0); err != nil { // no grace: past it at once
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	if ok, _, reason := m.ValidateKey(ctx, old, nil, nil, nil); ok || reason != "API key has been rotated" {
		t.Fatalf("the rotated-out token: ok %v, %q", ok, reason)
	}
	fresh, err := NewManager(path, filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if k := fresh.GetKey(key.KeyID); k == nil || k.PreviousKeyHash != nil {
		t.Fatalf("the file still records the rotated-out token: %+v", k)
	}
}
