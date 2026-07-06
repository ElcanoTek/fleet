package admincli

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// testPubkey returns a freshly generated, valid standard-base64 Ed25519 public
// key — the exact shape `auth-admin keygen` emits.
func testPubkey(t *testing.T) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return base64.StdEncoding.EncodeToString(pub)
}

func TestParseAuthPubkey(t *testing.T) {
	key := testPubkey(t)

	t.Run("accepts bare base64", func(t *testing.T) {
		got, err := ParseAuthPubkey(key)
		if err != nil || got != key {
			t.Fatalf("got %q, %v; want %q, nil", got, err, key)
		}
	})
	t.Run("accepts the auth pubkey output line", func(t *testing.T) {
		got, err := ParseAuthPubkey("AUTH_SIGNING_PUBKEY=" + key + "\n")
		if err != nil || got != key {
			t.Fatalf("got %q, %v; want %q, nil", got, err, key)
		}
	})
	t.Run("accepts a quoted value", func(t *testing.T) {
		got, err := ParseAuthPubkey(`AUTH_SIGNING_PUBKEY="` + key + `"`)
		if err != nil || got != key {
			t.Fatalf("got %q, %v; want %q, nil", got, err, key)
		}
	})
	t.Run("rejects non-base64", func(t *testing.T) {
		if _, err := ParseAuthPubkey("not-a-key!!"); err == nil {
			t.Fatal("want error for non-base64 input")
		}
	})
	t.Run("rejects wrong-size key", func(t *testing.T) {
		short := base64.StdEncoding.EncodeToString([]byte("short"))
		if _, err := ParseAuthPubkey(short); err == nil {
			t.Fatal("want error for a non-32-byte key")
		}
	})
	t.Run("rejects empty", func(t *testing.T) {
		if _, err := ParseAuthPubkey("  \n"); err == nil {
			t.Fatal("want error for empty input")
		}
	})
	t.Run("rejects a different KEY= line", func(t *testing.T) {
		// Only the AUTH_SIGNING_PUBKEY= prefix is stripped; some other VAR= line
		// must not silently have its prefix treated as part of a key.
		if _, err := ParseAuthPubkey("OTHER_KEY=" + key); err == nil {
			t.Fatal("want error when a non-AUTH_SIGNING_PUBKEY line is pasted")
		}
	})
}

func TestSetAuthPubkeyWritesEnvFile(t *testing.T) {
	key := testPubkey(t)
	dir := t.TempDir()
	envPath := filepath.Join(dir, "web.env")

	if code := configSetAuthPubkey([]string{
		"AUTH_SIGNING_PUBKEY=" + key,
		"--env-file", envPath,
		"--cookie-domain", "example.com",
	}); code != 0 {
		t.Fatalf("configSetAuthPubkey exit = %d, want 0", code)
	}

	b, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatalf("read env file: %v", err)
	}
	content := string(b)
	if !strings.Contains(content, "AUTH_SIGNING_PUBKEY="+key) {
		t.Errorf("env file missing pubkey line; got:\n%s", content)
	}
	if !strings.Contains(content, "AUTH_COOKIE_DOMAIN=example.com") {
		t.Errorf("env file missing cookie domain; got:\n%s", content)
	}
	// 0600: the web env holds session/token secrets alongside the (public) key.
	if fi, err := os.Stat(envPath); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("env file mode = %v, %v; want 0600", fi.Mode().Perm(), err)
	}

	// Invalid key must not touch the file.
	if code := configSetAuthPubkey([]string{"garbage", "--env-file", envPath}); code == 0 {
		t.Fatal("want non-zero exit for an invalid key")
	}
	b2, _ := os.ReadFile(envPath)
	if string(b2) != content {
		t.Error("env file changed by a rejected key")
	}
}

func TestSplitPositionalValueFlags(t *testing.T) {
	cases := []struct {
		name     string
		argv     []string
		wantPos  string
		wantRest []string
	}{
		{
			// The bug this splitter fixes: a flag VALUE with no leading dash must
			// stay bound to its flag, not be lifted as the positional.
			name:     "flag value not mistaken for positional",
			argv:     []string{"--from", "key.txt", "--env-file", "web.env"},
			wantPos:  "",
			wantRest: []string{"--from", "key.txt", "--env-file", "web.env"},
		},
		{
			name:     "positional before flags",
			argv:     []string{"a@b.com", "--password", "-"},
			wantPos:  "a@b.com",
			wantRest: []string{"--password", "-"},
		},
		{
			name:     "positional after a flag pair",
			argv:     []string{"--env-file", "x.env", "a@b.com"},
			wantPos:  "a@b.com",
			wantRest: []string{"--env-file", "x.env"},
		},
		{
			name:     "self-contained --flag=value",
			argv:     []string{"--env-file=x.env", "a@b.com"},
			wantPos:  "a@b.com",
			wantRest: []string{"--env-file=x.env"},
		},
		{
			name:     "empty argv",
			argv:     nil,
			wantPos:  "",
			wantRest: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pos, rest := splitPositionalValueFlags(tc.argv)
			if pos != tc.wantPos || !reflect.DeepEqual(rest, tc.wantRest) {
				t.Errorf("got (%q, %v); want (%q, %v)", pos, rest, tc.wantPos, tc.wantRest)
			}
		})
	}
}

func TestEnvFileResolvers(t *testing.T) {
	// Explicit flag always wins for both resolvers.
	if got := serverEnvFile("/x/custom.env"); got != "/x/custom.env" {
		t.Errorf("serverEnvFile(flag) = %q", got)
	}
	if got := webEnvFile("/x/web.env"); got != "/x/web.env" {
		t.Errorf("webEnvFile(flag) = %q", got)
	}
	// FLEET_ENV_FILE beats the /etc probe for the server file.
	t.Setenv("FLEET_ENV_FILE", "/y/fleet.env")
	if got := serverEnvFile(""); got != "/y/fleet.env" {
		t.Errorf("serverEnvFile(env) = %q", got)
	}
}
