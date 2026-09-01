// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package sandbox

// Hermetic tests for the boot-time keep-id pre-warm (#1358): a stub podman
// script records every invocation, so the tests pin the marker contract (warm
// on change, skip on match, re-warm after the image ID moves) and the exact
// idmap flag without needing podman or the multi-GB image.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stubPodman writes an executable that answers `image inspect` with imageID
// (or fails when imageID is empty, like an unpulled image) and logs every
// argv line to callLog.
func stubPodman(t *testing.T, dir, imageID, callLog string) string {
	t.Helper()
	script := "#!/bin/sh\necho \"$@\" >> " + callLog + "\n" +
		"if [ \"$1\" = image ] && [ \"$2\" = inspect ]; then\n"
	if imageID == "" {
		script += "  echo 'Error: image not known' >&2; exit 125\n"
	} else {
		script += "  echo " + imageID + "\n"
	}
	script += "fi\nexit 0\n"
	path := filepath.Join(dir, "podman")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func stubCalls(t *testing.T, callLog string) []string {
	t.Helper()
	b, err := os.ReadFile(callLog)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func TestPrewarmKeepIDImage_WarmsOnceThenSkips(t *testing.T) {
	dir := t.TempDir()
	callLog := filepath.Join(dir, "calls")
	podman := stubPodman(t, dir, "sha256:aaa", callLog)
	marker := filepath.Join(dir, "state", "prewarm-image-id")

	// First boot: inspect + the throwaway keep-id run, marker written.
	PrewarmKeepIDImage(context.Background(), podman, "img:latest", marker)
	calls := stubCalls(t, callLog)
	if len(calls) != 2 {
		t.Fatalf("first warm: %d podman calls %v, want inspect + run", len(calls), calls)
	}
	run := calls[1]
	if !strings.Contains(run, "run --rm "+keepIDUserns+" img:latest true") {
		t.Errorf("throwaway run must use the exact sandbox idmap (%s), got %q", keepIDUserns, run)
	}
	if got := readPrewarmMarker(marker); got != "sha256:aaa" {
		t.Errorf("marker = %q, want the warmed image ID", got)
	}

	// Same image, next boot: inspect only — no throwaway container.
	PrewarmKeepIDImage(context.Background(), podman, "img:latest", marker)
	if calls := stubCalls(t, callLog); len(calls) != 3 || !strings.HasPrefix(calls[2], "image inspect") {
		t.Errorf("unchanged image should cost one inspect, got calls %v", calls)
	}
}

func TestPrewarmKeepIDImage_RewarmsWhenImageChanges(t *testing.T) {
	dir := t.TempDir()
	callLog := filepath.Join(dir, "calls")
	podman := stubPodman(t, dir, "sha256:bbb", callLog)
	marker := filepath.Join(dir, "prewarm-image-id")
	if err := writePrewarmMarker(marker, "sha256:aaa"); err != nil {
		t.Fatal(err)
	}

	PrewarmKeepIDImage(context.Background(), podman, "img:latest", marker)
	calls := stubCalls(t, callLog)
	if len(calls) != 2 || !strings.Contains(calls[1], "run --rm") {
		t.Fatalf("changed image ID must re-warm, got calls %v", calls)
	}
	if got := readPrewarmMarker(marker); got != "sha256:bbb" {
		t.Errorf("marker = %q, want the new image ID", got)
	}
}

func TestPrewarmKeepIDImage_UnpulledImageWarmsAndRecordsAfterPull(t *testing.T) {
	dir := t.TempDir()
	callLog := filepath.Join(dir, "calls")
	// Inspect fails (image not pulled yet) — the run must still happen, and it
	// is what pulls the image in production. The stub keeps failing inspect,
	// so no marker can be recorded; the next boot warms again, which is the
	// safe direction.
	podman := stubPodman(t, dir, "", callLog)
	marker := filepath.Join(dir, "prewarm-image-id")

	PrewarmKeepIDImage(context.Background(), podman, "img:latest", marker)
	calls := stubCalls(t, callLog)
	foundRun := false
	for _, c := range calls {
		if strings.Contains(c, "run --rm "+keepIDUserns) {
			foundRun = true
		}
	}
	if !foundRun {
		t.Fatalf("unpulled image must still be warmed (the run pulls it), got calls %v", calls)
	}
	if got := readPrewarmMarker(marker); got != "" {
		t.Errorf("marker = %q, want empty while the image ID stays unresolvable", got)
	}
}
