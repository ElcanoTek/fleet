// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package scripts

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReleaseLibOlder(t *testing.T) {
	for _, tc := range []struct {
		tag, cur string
		older    bool
	}{
		{"v2026.11.07.3", "2026.11.07.10", true},
		{"v2026.11.07.10", "2026.11.07.9", false},
		{"v2026.11.07.3", "2026.11.07.3", false},
		{"v2026.10.30.1", "2026.11.01.1+3.gabc", true},
		{"v2026.10.30.1", "dev+gabc", false},
		{"v2026.10.30.1", "", false},
	} {
		_, err := runReleaseLib(t, []string{"TAG=" + tc.tag, "CUR=" + tc.cur}, `fleet_release_older "$TAG" "$CUR"`)
		if (err == nil) != tc.older {
			t.Errorf("older(%s, %q) = %v, want %v", tc.tag, tc.cur, err == nil, tc.older)
		}
	}
}

func TestReleaseLibPrivateDir(t *testing.T) {
	base := t.TempDir()
	mk := func(rel string, mode os.FileMode) string {
		p := filepath.Join(base, rel)
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	check := func(dir string) error {
		_, err := runReleaseLib(t, []string{"D=" + dir}, `fleet_release_private_dir "$D"`)
		return err
	}
	if err := check(mk("ok", 0o700)); err != nil {
		t.Errorf("a private dir was refused: %v", err)
	}
	if check(mk("open", 0o755)) == nil {
		t.Error("a 0755 staging dir was accepted")
	}
	mk("shared", 0o777)
	if check(mk("shared/stage", 0o700)) == nil {
		t.Error("a staging dir under a world-writable, non-sticky parent was accepted")
	}
	mk("sticky", 0o777|os.ModeSticky)
	if err := check(mk("sticky/stage", 0o700)); err != nil {
		t.Errorf("a private dir under a sticky parent (like /tmp) was refused: %v", err)
	}
}

// TestReleaseLibManifestCheckRefusesWithoutASignature — the pre-checkout
// manifest check fails closed: no signed SHA256SUMS, no checkout move.
func TestReleaseLibManifestCheckRefusesWithoutASignature(t *testing.T) {
	out, err := runReleaseLib(t, []string{"FLEET_RELEASE_BASE_URL=http://127.0.0.1:1", "FLEET_COSIGN_URL=http://127.0.0.1:1", "C=" + t.TempDir()},
		`fleet_release_verify_manifest "$ROOT" v2026.11.07.3 `+strings.Repeat("a", 40)+` "$C"`)
	if err == nil {
		t.Fatalf("an unreachable/unsigned release verified\n%s", out)
	}
	if _, err := runReleaseLib(t, nil, `fleet_release_verify_manifest "$ROOT" v2026.11.07.3 'abc;id' /tmp`); err == nil {
		t.Error("a malformed commit was accepted")
	}
}
