// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package handlers

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"
)

// writeUploadFile is the seam behind HandleUpload's file write: it must close
// (and check the close of) the file before reporting success, and a failure
// mid-copy must leave no partial file under the name a task would attach.
func TestWriteUploadFile(t *testing.T) {
	dir := t.TempDir()

	t.Run("success returns the size and checksum of the closed file", func(t *testing.T) {
		path := filepath.Join(dir, "ok.txt")
		size, sum, err := writeUploadFile(path, strings.NewReader("task brief"))
		if err != nil {
			t.Fatalf("writeUploadFile: %v", err)
		}
		want := sha256.Sum256([]byte("task brief"))
		if size != int64(len("task brief")) || sum != hex.EncodeToString(want[:]) {
			t.Fatalf("size=%d sum=%s, want %d/%s", size, sum, len("task brief"), hex.EncodeToString(want[:]))
		}
		if got, err := os.ReadFile(path); err != nil || string(got) != "task brief" {
			t.Fatalf("file on disk = %q (err %v)", got, err)
		}
	})

	t.Run("a failing source leaves no partial file", func(t *testing.T) {
		path := filepath.Join(dir, "partial.txt")
		src := io.MultiReader(strings.NewReader("half of the"), iotest.ErrReader(errors.New("client went away")))
		if _, _, err := writeUploadFile(path, src); err == nil {
			t.Fatal("expected the copy error to surface")
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("partial upload left on disk (stat err=%v)", err)
		}
	})
}
