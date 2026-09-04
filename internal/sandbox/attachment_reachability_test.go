package sandbox

// Pins the reachability half of ADR-0058: a chat attachment is readable from a
// sandbox because it was STAGED into the owning conversation's workspace, not
// because the uploads tree is mounted. So the uploads tree must not resolve to
// any bind-mount anchor — knowing another user's upload path is not enough to
// read it — while the staged copy under the workspace root must.
//
// fileOpAnchorFor is the resolution seam BOTH backends use (container.go's
// RunFileOp and k8s_backend.go's), so one test covers podman and kubernetes.

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestFileOpAnchorRefusesTheChatUploadsTree(t *testing.T) {
	const (
		workspace = "/var/lib/fleet/workspace"
		dataDir   = "/var/lib/fleet/data"
	)
	// The production mount list after ADR-0058: bundle doc roots plus the
	// shared library's staged tree. No uploads root.
	mounts := []string{
		"/opt/fleet/client/protocols",
		"/opt/fleet/client/personas",
		filepath.Join(workspace, "shared"),
	}

	// An upload of ANOTHER user/conversation, named by absolute path exactly as
	// a copied user message or an export would spell it.
	foreignUpload := filepath.Join(dataDir, "attachments", "uploads",
		"9f2c1d5b7a3e4c8d9f2c1d5b7a3e4c8d", "fq6xyz", "fleet-team-share-test.csv")
	if _, _, err := fileOpAnchorFor(workspace, mounts, foreignUpload); !errors.Is(err, ErrFileOpUnsafePath) {
		t.Errorf("uploads path resolved to an anchor: fileOpAnchorFor(%q) err = %v; want ErrFileOpUnsafePath", foreignUpload, err)
	}
	// The uploads root itself, and the flat pre-ADR-0058 shape, are equally
	// unreachable — this is not about one path shape.
	for _, root := range []string{
		filepath.Join(dataDir, "attachments", "uploads"),
		filepath.Join(dataDir, "attachments", "uploads", "tok", "legacy.csv"),
	} {
		if _, _, err := fileOpAnchorFor(workspace, mounts, root); !errors.Is(err, ErrFileOpUnsafePath) {
			t.Errorf("fileOpAnchorFor(%q) err = %v; want ErrFileOpUnsafePath", root, err)
		}
	}

	// The staged copy IS reachable, writable, and anchored on the workspace
	// mount — that is how an attachment reaches the agent at all now.
	staged := filepath.Join(workspace, "conv-a", "attachments", "data.csv")
	anchor, readOnly, err := fileOpAnchorFor(workspace, mounts, staged)
	if err != nil || anchor != workspace || readOnly {
		t.Errorf("staged attachment anchor = %q, ro=%v, %v; want %q, writable, nil", anchor, readOnly, err, workspace)
	}
}
