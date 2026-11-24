// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package admincli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/accountdelete"
	"github.com/ElcanoTek/fleet/internal/sched/apikeys"
)

// `fleet admin rm` and `fleet chat user del` delete what Settings → Admin →
// Users deletes: the account's rows, the user API keys that act as it in the
// service's key store, and its upload and private workspace trees under the
// service's attachment directory. Another account's keys and files stay.
func TestCLIAccountDeletionRemovesKeysAndFiles(t *testing.T) {
	for name, verb := range map[string]func(email string) int{
		"fleet admin rm":      func(email string) int { return adminRm([]string{email}) },
		"fleet chat user del": func(email string) int { return chatUserDel([]string{email}) },
	} {
		t.Run(name, func(t *testing.T) {
			d := fleetEnv(t)
			data, att := t.TempDir(), t.TempDir()
			t.Setenv("FLEET_DATA_DIR", data)
			t.Setenv("EMAIL_ATTACHMENT_DIR", att)
			t.Setenv("FLEET_SERVICE_NAME", "fleet-account-delete-test-none")
			ctx := context.Background()
			const gone, stay = "gone@acctdel.test", "stay@acctdel.test"
			chat := d.Chat(t)
			for _, e := range []string{gone, stay} {
				if _, err := chat.CreateUser(ctx, e, "delete-password-1"); err != nil {
					t.Fatal(err)
				}
			}
			mgr, err := apikeys.NewManager(filepath.Join(data, "api_keys.json"), "")
			if err != nil {
				t.Fatal(err)
			}
			_, goneKey, err := mgr.CreateUserKey(ctx, "cli", gone, 0, nil, "")
			if err != nil {
				t.Fatal(err)
			}
			_, stayKey, err := mgr.CreateUserKey(ctx, "cli", stay, 0, nil, "")
			if err != nil {
				t.Fatal(err)
			}
			files := map[string]string{}
			for _, e := range []string{gone, stay} {
				for _, dir := range []string{
					filepath.Join(accountdelete.UploadsDir(att, e), "tok"),
					filepath.Join(accountdelete.PrivateWorkspaceRoot(att), "conv-1", "participants", accountdelete.Digest(e)),
				} {
					if err := os.MkdirAll(dir, 0o700); err != nil {
						t.Fatal(err)
					}
					f := filepath.Join(dir, "file.txt")
					if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
						t.Fatal(err)
					}
					files[f] = e
				}
			}

			out := captureStdout(t, func() {
				if code := verb(gone); code != 0 {
					t.Fatalf("exit %d", code)
				}
			})
			if !strings.Contains(out, "1 user API key") {
				t.Errorf("output %q does not say the user key was deleted", out)
			}
			if _, err := chat.GetUser(ctx, gone); err == nil {
				t.Error("the account survived")
			}
			if ok, _, _ := mgr.ValidateKey(ctx, goneKey, nil, nil, nil); ok {
				t.Error("the deleted account's user key still authenticates")
			}
			if ok, _, reason := mgr.ValidateKey(ctx, stayKey, nil, nil, nil); !ok {
				t.Errorf("another account's user key was removed: %s", reason)
			}
			for f, owner := range files {
				_, err := os.Stat(f)
				if owner == gone && !os.IsNotExist(err) {
					t.Errorf("%s of the deleted account survived (err %v)", f, err)
				}
				if owner == stay && err != nil {
					t.Errorf("%s of another account was removed: %v", f, err)
				}
			}
			if code := verb(gone); code == 0 {
				t.Error("deleting an account that does not exist succeeded")
			}
		})
	}
}

// An API-key store that cannot be read stops the deletion before anything is
// deleted.
func TestCLIAccountDeletionStopsOnAnUnreadableKeyStore(t *testing.T) {
	d := fleetEnv(t)
	data := t.TempDir()
	t.Setenv("FLEET_DATA_DIR", data)
	t.Setenv("EMAIL_ATTACHMENT_DIR", t.TempDir())
	t.Setenv("FLEET_SERVICE_NAME", "fleet-account-delete-test-none")
	if err := os.WriteFile(filepath.Join(data, "api_keys.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	chat := d.Chat(t)
	if _, err := chat.CreateUser(ctx, "kept@acctdel.test", "delete-password-1"); err != nil {
		t.Fatal(err)
	}
	if code := chatUserDel([]string{"kept@acctdel.test"}); code == 0 {
		t.Fatal("deleted with an unreadable key store")
	}
	if _, err := chat.GetUser(ctx, "kept@acctdel.test"); err != nil {
		t.Fatalf("the account was deleted although the key store could not be read: %v", err)
	}
}

func TestChooseAttachmentDir(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		in      attachmentDirInputs
		want    string
		wantSrc string
	}{
		{"explicit, relative to cwd", attachmentDirInputs{explicit: "att", unitWorkDir: "/var/lib/fleet", cwd: "/root"}, "/root/att", "your environment"},
		{"unit, env file silent", attachmentDirInputs{unitWorkDir: "/var/lib/fleet", cwd: "/root"}, "/var/lib/fleet/data/attachments", "fleet.service"},
		{"unit, env file relative", attachmentDirInputs{unitWorkDir: "/var/lib/fleet", envFileDir: "files", cwd: "/root"}, "/var/lib/fleet/files", "fleet.service"},
		{"unit, env file absolute", attachmentDirInputs{unitWorkDir: "/var/lib/fleet", envFileDir: "/srv/att", cwd: "/root"}, "/srv/att", "fleet.service"},
		{"no unit", attachmentDirInputs{cwd: "/home/dev/fleet"}, "/home/dev/fleet/data/attachments", "no fleet.service"},
	} {
		got, src := chooseAttachmentDir(tc.in)
		if got != tc.want || !strings.Contains(src, tc.wantSrc) {
			t.Errorf("%s: %q (%s), want %q (%s)", tc.name, got, src, tc.want, tc.wantSrc)
		}
	}
}

// A deletion that completes with a step left — here an attachment directory
// that does not exist — deletes the account and exits 6, distinct from every
// refusal.
func TestCLIAccountDeletionWithStepsLeftExits6(t *testing.T) {
	d := fleetEnv(t)
	t.Setenv("FLEET_DATA_DIR", t.TempDir())
	t.Setenv("EMAIL_ATTACHMENT_DIR", filepath.Join(t.TempDir(), "not-here"))
	t.Setenv("FLEET_SERVICE_NAME", "fleet-account-delete-test-none")
	ctx := context.Background()
	chat := d.Chat(t)
	if _, err := chat.CreateUser(ctx, "left@acctdel.test", "delete-password-1"); err != nil {
		t.Fatal(err)
	}
	if code := chatUserDel([]string{"left@acctdel.test"}); code != exitDeletedStepsLeft {
		t.Fatalf("exit %d, want %d", code, exitDeletedStepsLeft)
	}
	if _, err := chat.GetUser(ctx, "left@acctdel.test"); err == nil {
		t.Fatal("the account survived")
	}
}

// `fleet sched apikey create --type user` binds the key to the account's id,
// read from the database the CLI resolves, and mints nothing for an address
// with no account.
func TestCLIUserKeyIsBoundToTheAccountInTheDatabase(t *testing.T) {
	d := fleetEnv(t)
	data := t.TempDir()
	t.Setenv("FLEET_DATA_DIR", data)
	t.Setenv("FLEET_SERVICE_NAME", "fleet-account-delete-test-none")
	ctx := context.Background()
	chat := d.Chat(t)
	u, err := chat.CreateUser(ctx, "bot@acctdel.test", "delete-password-1")
	if err != nil {
		t.Fatal(err)
	}
	if code := schedAPIKeyCreate([]string{"nobody-key", "--type", "user", "--user", "nobody@acctdel.test"}); code == 0 {
		t.Fatal("a user key was minted for an address with no account")
	}
	captureStdout(t, func() {
		if code := schedAPIKeyCreate([]string{"bot-key", "--type", "user", "--user", "bot@acctdel.test"}); code != 0 {
			t.Fatalf("exit %d", code)
		}
	})
	mgr, err := apikeys.NewManager(filepath.Join(data, "api_keys.json"), "")
	if err != nil {
		t.Fatal(err)
	}
	keys := mgr.ListKeys()
	if len(keys) != 1 || keys[0].AccountID != u.ID || keys[0].UserEmail != "bot@acctdel.test" {
		t.Fatalf("keys = %+v, want one bound to %s", keys, u.ID)
	}
}

// `fleet sched apikey list` shows the account each user key acts as: its id,
// "missing" (the key acts as no one) or "unbound" (bound at the server's next
// boot or the key's first use), in the text listing and in --json.
func TestAPIKeyListShowsTheKeysAccount(t *testing.T) {
	data := t.TempDir()
	t.Setenv("FLEET_DATA_DIR", data)
	t.Setenv("FLEET_SERVICE_NAME", "fleet-account-delete-test-none")
	ctx := context.Background()
	mgr, err := apikeys.NewManager(filepath.Join(data, "api_keys.json"), "")
	if err != nil {
		t.Fatal(err)
	}
	bound, _, err := mgr.CreateBoundUserKey(ctx, "bound", "a@x.com", "acct-1", 0, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	missing, _, _ := mgr.CreateUserKey(ctx, "missing", "b@x.com", 0, nil, "")
	if err := mgr.RecordUserKeyAccount(ctx, missing.KeyID, ""); err != nil {
		t.Fatal(err)
	}
	unbound, _, _ := mgr.CreateUserKey(ctx, "unbound", "c@x.com", 0, nil, "")
	task, _, _ := mgr.CreateTypedKey(ctx, "task", apikeys.KeyTypeTask, nil, 0, nil, "")
	want := map[string]string{bound.KeyID: "acct-1", missing.KeyID: "missing", unbound.KeyID: "unbound", task.KeyID: ""}

	text := captureStdout(t, func() {
		if code := schedAPIKeyList(nil); code != 0 {
			t.Fatalf("exit %d", code)
		}
	})
	for id, account := range want {
		var line string
		for _, l := range strings.Split(text, "\n") {
			if strings.HasPrefix(l, id+"\t") {
				line = l
			}
		}
		if account == "" {
			if strings.Contains(line, "account=") {
				t.Errorf("a key that acts as no account lists one: %q", line)
			}
		} else if !strings.Contains(line, "\taccount="+account) {
			t.Errorf("key %s: line %q, want account=%s", id, line, account)
		}
	}
	raw := captureStdout(t, func() {
		if code := schedAPIKeyList([]string{"--json"}); code != 0 {
			t.Fatalf("exit %d", code)
		}
	})
	var rows []struct {
		KeyID   string `json:"key_id"`
		Account string `json:"account"`
	}
	if err := json.Unmarshal([]byte(raw), &rows); err != nil {
		t.Fatalf("--json: %v (%s)", err, raw)
	}
	for _, r := range rows {
		if r.Account != want[r.KeyID] {
			t.Errorf("--json key %s: account %q, want %q", r.KeyID, r.Account, want[r.KeyID])
		}
	}
}

// An account deleted after `fleet sched apikey create --type user` looked it
// up, but before its key is saved — the deletion's key cleanup finds no key
// yet — gets no key: the mint checks the account again with the key saved
// and the key store still locked, takes the key out, and says so.
func TestUserKeyForAnAccountDeletedWhileMintingIsNotMinted(t *testing.T) {
	d := fleetEnv(t)
	data := t.TempDir()
	t.Setenv("FLEET_DATA_DIR", data)
	t.Setenv("FLEET_SERVICE_NAME", "fleet-account-delete-test-none")
	ctx := context.Background()
	chat := d.Chat(t)
	if _, err := chat.CreateUser(ctx, "racing@acctdel.test", "delete-password-1"); err != nil {
		t.Fatal(err)
	}
	ks := resolveKeyStore()
	mgr, err := apikeys.NewManager(ks.Path(), "")
	if err != nil {
		t.Fatal(err)
	}
	lookups := 0
	lookup := func(ctx context.Context, email string) (string, bool, error) {
		id, found, err := chatAccountLookup(ctx, email)
		if lookups++; lookups == 1 && found {
			// Between the lookup and the key's write: the account is
			// deleted, keys and all (there are none yet).
			if _, derr := deleteAccount(ctx, chat, email); derr != nil {
				t.Errorf("delete the account: %v", derr)
			}
		}
		return id, found, err
	}
	var code int
	stderr := captureStderr(t, func() {
		captureStdout(t, func() { code = createUserKey(mgr, ks, "racing-key", "racing@acctdel.test", 0, lookup) })
	})
	if code == 0 {
		t.Fatal("a key was minted for an account deleted while it was being created")
	}
	if !strings.Contains(stderr, "nothing was minted") {
		t.Errorf("stderr %q, want it to say nothing was minted", stderr)
	}
	if lookups != 2 {
		t.Errorf("the account was looked up %d times; want a second look with the key saved", lookups)
	}
	reread, err := apikeys.NewManager(ks.Path(), "")
	if err != nil {
		t.Fatal(err)
	}
	if keys := reread.ListKeys(); len(keys) != 0 {
		t.Fatalf("the key store holds %+v, want no key", keys)
	}
}
