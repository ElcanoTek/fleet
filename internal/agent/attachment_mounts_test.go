package agent

// The uploads tree is the one host directory a sandbox must never see
// (ADR-0058, docs/ATTACHMENT-SCOPING.md): it is a single flat tree shared by
// every user and conversation, so mounting it made a path — which travels into
// copied messages, exports and branched transcripts — the only thing between
// one chat's turn and another user's file.
//
// sandboxReadOnlyMounts is the single place that decides the read-only mount
// set, and it does not merely omit the tree, it refuses it. These tests pin
// both halves, because a re-added mount is invisible until someone reads
// another user's upload.

import (
	"path/filepath"
	"testing"

	"github.com/ElcanoTek/fleet/internal/config"
)

func TestSandboxReadOnlyMountsExcludeTheUploadsTree(t *testing.T) {
	cfg := &config.Config{EmailAttachmentDir: "/var/lib/fleet/data/attachments"}
	workspace := "/var/lib/fleet/workspace"
	mounts := sandboxReadOnlyMounts(cfg,
		"/opt/fleet/client/personas",
		"/opt/fleet/client/protocols",
		"/opt/fleet/client/system_prompts",
		"/opt/fleet/client/skills",
		filepath.Join(workspace, "shared"),
	)

	if len(mounts) != 5 {
		t.Fatalf("mounts = %v; want the four bundle doc roots plus the shared library tree", mounts)
	}
	uploads := filepath.Join(cfg.EmailAttachmentDir, "uploads")
	for _, m := range mounts {
		if pathIsWithin(uploads, m) {
			t.Errorf("mount %q is inside the uploads tree %q — no sandbox may see chat uploads", m, uploads)
		}
	}
}

// A doc dir configured to sit INSIDE the uploads tree is dropped, so the
// invariant survives a bundle path (or a future edit) that would smuggle the
// tree back in.
func TestSandboxReadOnlyMountsRefuseAnUploadsSubtree(t *testing.T) {
	cfg := &config.Config{EmailAttachmentDir: "/var/lib/fleet/data/attachments"}
	uploads := filepath.Join(cfg.EmailAttachmentDir, "uploads")
	mounts := sandboxReadOnlyMounts(cfg,
		"/opt/fleet/client/personas",
		uploads,                           // the tree itself
		filepath.Join(uploads, "sneaky"),  // something under it
		"",                                // empty entries still drop out
		"/var/lib/fleet/workspace/shared", //
	)

	want := []string{"/opt/fleet/client/personas", "/var/lib/fleet/workspace/shared"}
	if len(mounts) != len(want) {
		t.Fatalf("mounts = %v; want %v", mounts, want)
	}
	for i, m := range mounts {
		if m != want[i] {
			t.Errorf("mounts[%d] = %q, want %q", i, m, want[i])
		}
	}
}

func TestPathIsWithin(t *testing.T) {
	root := "/var/lib/fleet/data/attachments/uploads"
	for _, in := range []string{root, root + "/tok", root + "/tok/data.csv", root + "/./tok"} {
		if !pathIsWithin(root, in) {
			t.Errorf("pathIsWithin(%q, %q) = false, want true", root, in)
		}
	}
	// Neighbours that merely share a prefix, and parents, are outside.
	for _, in := range []string{
		"/var/lib/fleet/data/attachments/uploads-other",
		"/var/lib/fleet/data/attachments",
		"/var/lib/fleet/workspace",
		root + "/../secrets",
	} {
		if pathIsWithin(root, in) {
			t.Errorf("pathIsWithin(%q, %q) = true, want false", root, in)
		}
	}
}
