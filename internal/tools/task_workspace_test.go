// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package tools

import (
	"path/filepath"
	"testing"
)

func TestTaskWorkspaceDir(t *testing.T) {
	root := filepath.Join("var", "lib", "fleet", "workspace")
	if got := TaskWorkspaceDir(root, "6bd0c212-0000-4000-8000-000000000000"); got != filepath.Join(root, "tasks", "6bd0c212-0000-4000-8000-000000000000") {
		t.Fatalf("per-job dir = %q", got)
	}
	for _, bad := range []string{"", "..", "../x", "/abs", "a/b"} {
		if got := TaskWorkspaceDir(root, bad); got != root {
			t.Errorf("non-local lineage %q must fall back to the root, got %q", bad, got)
		}
	}
}
