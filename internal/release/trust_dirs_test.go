// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package release

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallRefusesAnInstallDirOthersCanWrite(t *testing.T) {
	dir, bin, svc, p := setup(t, "OLD")
	p.ready["NEW"] = true
	_ = os.Chmod(dir, 0o777)
	if _, err := Install(context.Background(), opts(dir, bin, svc, p)); err == nil || !strings.Contains(err.Error(), "no one else can write") {
		t.Fatalf("an install dir others can write to was accepted: %v", err)
	}
	_ = os.WriteFile(filepath.Join(dir, "fleet.prev"), []byte("PREV"), 0o755)
	if err := Rollback(context.Background(), opts(dir, "", svc, p)); err == nil {
		t.Fatal("rollback into an install dir others can write to was accepted")
	}
	_ = os.Chmod(dir, 0o755) // readable by the service user, writable by its owner only: fine
	if _, err := Install(context.Background(), opts(dir, bin, svc, p)); err != nil {
		t.Fatalf("a 0755 install dir: %v", err)
	}
}

func TestCosignIsRunOnlyFromAPrivateVerifiedCopy(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "cache")
	src := filepath.Join(t.TempDir(), "cosign")
	_ = os.WriteFile(src, []byte("#!/bin/sh\necho not the pinned cosign\n"), 0o755)
	if _, err := InstallCosignFrom(src, cache, "amd64"); err == nil {
		t.Fatal("a FLEET_COSIGN_BIN that is not the pinned build was accepted")
	}
	entries, _ := os.ReadDir(cache)
	if len(entries) != 0 {
		t.Errorf("a refused cosign left files in the cache: %v", entries)
	}

	shared := filepath.Join(t.TempDir(), "shared")
	_ = os.Mkdir(shared, 0o700)
	_ = os.Chmod(shared, 0o777)
	if _, err := EnsureCosign(context.Background(), NewDownloader(), filepath.Join(shared, "cache"), "amd64"); err == nil || !strings.Contains(err.Error(), "others can write") {
		t.Fatalf("a cosign cache others could swap files in was accepted: %v", err)
	}
}
