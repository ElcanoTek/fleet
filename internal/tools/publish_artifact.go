package tools

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"charm.land/fantasy"
)

// PublishArtifactToolName is the canonical name of the built-in publish_artifact tool.
const PublishArtifactToolName = "publish_artifact"

// ArtifactRecorder is the thin seam the publish_artifact tool calls to register
// one published output file (#204). It is satisfied per-run by the scheduled
// driver (an adapter over the run's artifact collector); the indirection keeps
// the tool unit-testable and free of any sched/runner dependency. The
// implementation owns the per-run cap and dedup-by-path policy and returns a
// clear error the tool surfaces to the model.
type ArtifactRecorder interface {
	// RecordArtifact registers a published artifact. name is the sanitized base
	// filename, relPath is the workspace-relative path (already validated by the
	// tool to exist inside the workspace), description is the agent's optional
	// note, and size is the file size in bytes.
	RecordArtifact(name, relPath, description string, size int64) error
}

type publishArtifactParams struct {
	Path        string `json:"path" description:"Workspace-relative path of a file THIS run produced to publish as a downloadable deliverable, e.g. \"report.csv\" or \"out/summary.pdf\". The file must already exist in the workspace."`
	Description string `json:"description,omitempty" description:"Optional short note describing what the file is (e.g. \"Q3 revenue report\")."`
}

const publishArtifactDescription = `Publish a file you created in the workspace as a NAMED OUTPUT ARTIFACT the operator can download after this task finishes.

Use this for your DELIVERABLES — the report, the processed dataset, the rendered document — so they show up in the task's curated artifact list, separate from scratch/intermediate files. Write the file first, THEN publish it (the path must already exist). Publishing the same path again updates its description. A small per-run cap applies.

Returns a confirmation. The published file is downloadable via the task's workspace file endpoint once the run reaches a terminal state.`

// NewPublishArtifactTool returns the scheduled-only publish_artifact tool wired
// to rec. The scheduled driver constructs it per run with a recorder backed by
// that run's artifact collector. The tool validates that path stays within the
// run's workspace (no traversal / symlink escape) and names an existing regular
// file, then records it; it never reads, moves, or copies the file (the bytes
// stay in the workspace the file-browser endpoints already serve).
//
// workspaceRoot is the run's effective workspace for a NON-worktree task (the
// same directory the #287 file browser serves for it). A worktree run's forced
// working dir takes precedence so published paths stay scoped to the worktree.
// Resolving ONLY via the forced working dir — which the scheduled driver sets
// exclusively for worktree isolation — left every non-worktree task with
// "no workspace is configured" on every publish attempt.
func NewPublishArtifactTool(rec ArtifactRecorder, workspaceRoot string) fantasy.AgentTool {
	return fantasy.NewAgentTool(PublishArtifactToolName, publishArtifactDescription,
		func(ctx context.Context, in publishArtifactParams, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if rec == nil {
				return fantasy.NewTextErrorResponse("publish_artifact is not available in this run."), nil
			}
			rel := strings.TrimSpace(in.Path)
			if rel == "" {
				return fantasy.NewTextErrorResponse("publish_artifact requires a non-empty path."), nil
			}
			workdir := ForcedWorkingDirFromContext(ctx)
			if workdir == "" {
				workdir = strings.TrimSpace(workspaceRoot)
			}
			if workdir == "" {
				return fantasy.NewTextErrorResponse("publish_artifact: no workspace is configured for this run."), nil
			}
			// The run's directory is opened without following a symbolic link
			// below the workspace root (a whole-root sandbox can replace a
			// worktree directory with one), and the path is resolved through it:
			// an absolute path, any ".." component and a symlink that leaves the
			// workspace are refused, and a missing file fails — exactly the
			// "publish an existing workspace file" contract we want.
			dir, err := OpenRunDir(ctx, workspaceRootForContainment(), workdir)
			if err != nil {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("publish_artifact: the run's workspace could not be opened (%v).", err)), nil
			}
			defer dir.Close()
			canon, err := dir.Resolve(rel)
			if err != nil {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("publish_artifact: %q is not a readable file inside the workspace (%v). Write the file first, then publish its workspace-relative path.", rel, err)), nil
			}
			info, err := dir.Stat(canon)
			if err != nil {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("publish_artifact: cannot stat %q: %v", rel, err)), nil
			}
			if info.IsDir() {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("publish_artifact: %q is a directory; publish individual files, not directories.", rel)), nil
			}
			name := filepath.Base(filepath.FromSlash(rel))
			if err := rec.RecordArtifact(name, filepath.ToSlash(rel), strings.TrimSpace(in.Description), info.Size()); err != nil {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("publish_artifact: %v", err)), nil
			}
			return fantasy.NewTextResponse(fmt.Sprintf("Published artifact %q (%d bytes). It will be downloadable from the task's artifacts once the run finishes.", name, info.Size())), nil
		})
}
