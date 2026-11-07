// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package release

import (
	"context"
	"debug/elf"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckPrivateDir(t *testing.T) {
	base := t.TempDir()
	ok := filepath.Join(base, "ok")
	if err := EnsurePrivateDir(ok); err != nil {
		t.Fatalf("a fresh 0700 dir under a private parent: %v", err)
	}

	open := filepath.Join(base, "open")
	_ = os.Mkdir(open, 0o755)
	_ = os.Chmod(open, 0o755)
	if err := CheckPrivateDir(open); err == nil {
		t.Error("a 0755 staging dir was accepted")
	}
	if err := EnsurePrivateDir(open); err != nil {
		t.Errorf("EnsurePrivateDir must tighten a dir this user owns: %v", err)
	}

	shared := filepath.Join(base, "shared")
	_ = os.Mkdir(shared, 0o700)
	_ = os.Chmod(shared, 0o777) // world-writable, NOT sticky: anyone can rename the child away
	child := filepath.Join(shared, "stage")
	_ = os.Mkdir(child, 0o700)
	if err := CheckPrivateDir(child); err == nil || !strings.Contains(err.Error(), "others can write") {
		t.Errorf("a staging dir under a world-writable parent was accepted: %v", err)
	}

	sticky := filepath.Join(base, "sticky")
	_ = os.Mkdir(sticky, 0o700)
	_ = os.Chmod(sticky, 0o777|os.ModeSticky) // like /tmp
	safe := filepath.Join(sticky, "stage")
	_ = os.Mkdir(safe, 0o700)
	if err := CheckPrivateDir(safe); err != nil {
		t.Errorf("a private dir under a sticky parent (like /tmp) must be fine: %v", err)
	}

	link := filepath.Join(base, "link")
	_ = os.Symlink(child, link)
	if err := CheckPrivateDir(link); err == nil {
		t.Error("a symlink to an unsafe dir was accepted: the resolved path is what counts")
	}
}

// stagedRelease runs a real Fetch into a private staging dir and returns it.
func stagedRelease(t *testing.T) (dir string, v *fakeVerifier) {
	t.Helper()
	r := newRelease(t, testTag, SignerIdentity)
	got, v, err := fetch(t, serve(t, r), FetchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Dir(got.Binary), v
}

func TestInstallUsesTheVerifiedDescriptorNotTheName(t *testing.T) {
	dir, v := stagedRelease(t)
	got, err := LoadVerified(context.Background(), v, dir, mustTag(t, testTag), "amd64")
	if err != nil {
		t.Fatal(err)
	}
	defer got.BinaryFile.Close()
	verified, _ := os.ReadFile(got.Binary)
	// After verification, something replaces the NAME with other bytes.
	evil := filepath.Join(dir, "evil")
	_ = os.WriteFile(evil, fakeELF(elf.EM_X86_64, "swapped in after verification"), 0o700)
	if err := os.Rename(evil, got.Binary); err != nil {
		t.Fatal(err)
	}
	inst, _, svc, p := setup(t, "OLD")
	p.ready[string(verified)] = true
	o := opts(inst, got.Binary, svc, p)
	o.Source, o.SHA256 = got.BinaryFile, got.BinarySHA256
	if _, err := Install(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if read(t, filepath.Join(inst, "fleet")) != string(verified) {
		t.Fatal("the install used the replaced file, not the verified descriptor")
	}
}

func TestInstallRefusesBytesChangedAfterVerification(t *testing.T) {
	dir, v := stagedRelease(t)
	got, err := LoadVerified(context.Background(), v, dir, mustTag(t, testTag), "amd64")
	if err != nil {
		t.Fatal(err)
	}
	defer got.BinaryFile.Close()
	// The same inode rewritten in place after the hash.
	if err := os.WriteFile(got.Binary, fakeELF(elf.EM_X86_64, "rewritten in place"), 0o755); err != nil {
		t.Fatal(err)
	}
	inst, _, svc, p := setup(t, "OLD")
	o := opts(inst, got.Binary, svc, p)
	o.Source, o.SHA256 = got.BinaryFile, got.BinarySHA256
	if _, err := Install(context.Background(), o); err == nil || !strings.Contains(err.Error(), "changed after it was verified") {
		t.Fatalf("want a refusal, got %v", err)
	}
	if read(t, filepath.Join(inst, "fleet")) != "OLD" {
		t.Fatal("changed bytes were installed")
	}
}

func TestLoadVerifiedRefusesAnUnsafeStagingDir(t *testing.T) {
	dir, v := stagedRelease(t)
	_ = os.Chmod(dir, 0o755)
	if _, err := LoadVerified(context.Background(), v, dir, mustTag(t, testTag), "amd64"); err == nil {
		t.Fatal("a staging dir others can read and enter was accepted")
	}
	_ = os.Chmod(dir, 0o700)
	_ = os.Chmod(filepath.Join(dir, SumsFile), 0o666)
	if _, err := LoadVerified(context.Background(), v, dir, mustTag(t, testTag), "amd64"); err == nil || !strings.Contains(err.Error(), "private regular file") {
		t.Fatalf("a staging file others can write was accepted: %v", err)
	}
}

func TestFetchRequiresTheTagsCommit(t *testing.T) {
	r := newRelease(t, testTag, SignerIdentity)
	base := serve(t, r)
	if _, _, err := fetch(t, base, FetchOptions{Commit: strings.Repeat("c", 40)}); err != nil {
		t.Fatalf("matching commit: %v", err)
	}
	if _, _, err := fetch(t, base, FetchOptions{Commit: strings.Repeat("d", 40)}); err == nil || !strings.Contains(err.Error(), "tag was moved") {
		t.Fatalf("a release.json naming another commit was accepted: %v", err)
	}
}
