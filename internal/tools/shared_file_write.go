package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/sandbox"
)

// SharedFileWriteToolName is the native tool that lets a conversation (or a
// scheduled run) write a TEXT file into the server's shared file library, so
// every other conversation on the server can read it at shared/<path>
// (ADR-0114, docs/SHARED-FILES.md "Files chats write").
const SharedFileWriteToolName = "shared_file_write"

// SharedFileMaxBytes caps one file chats write. It is declared here (not in
// internal/sharedfiles, which imports this package) so the tool can refuse an
// oversized from_path before reading it.
const SharedFileMaxBytes int64 = 25 << 20

// SharedFileMaxInlineBytes caps the content argument itself. Text a model
// writes inline is small (a log row, a section); a whole file it built goes
// through from_path, which costs no output tokens.
const SharedFileMaxInlineBytes = 1 << 20

// Shared-file write modes.
const (
	SharedFileModeCreate  = "create"
	SharedFileModeAppend  = "append"
	SharedFileModeReplace = "replace"
	SharedFileModeUndo    = "undo"
)

// SharedFileWriteParams is the schema-generating input for shared_file_write.
type SharedFileWriteParams struct {
	Path           string `json:"path" description:"Where the file lives in the shared library: 'name.ext' or 'folder/name.ext' (one folder level; a leading 'shared/' is fine). Every conversation reads it at shared/<path>."`
	Mode           string `json:"mode" description:"'create' a new file (fails if it exists; description required), 'append' text to the end (creates the file if missing, then description required; a newline is added first if the file does not end with one), 'replace' the whole file (expected_sha256 required), or 'undo' the last replace (restores the version it displaced)."`
	Content        string `json:"content,omitempty" description:"The text to write or append (UTF-8, at most 1 MB). Give either content or from_path, not both; omit both for 'undo'."`
	FromPath       string `json:"from_path,omitempty" description:"Instead of content: a file in YOUR workspace whose text to write or append (e.g. a CSV you built with run_python). Relative paths resolve against your workspace, like view_file."`
	Description    string `json:"description,omitempty" description:"One short line saying what the file is and when to read it (e.g. 'Reklaim optimization log: every applied SSP change; read it for what was done on a deal or client'). Every conversation on this server is shown it. Required when the call creates the file; otherwise replaces the description when given."`
	ExpectedSHA256 string `json:"expected_sha256,omitempty" description:"The sha256 of the version you read (view_file prints it; every shared_file_write result returns it). Required for 'replace'; optional for 'append' and 'undo'. If the file changed since, the write is refused: read it again, merge, and retry."`
	FolderIndex    *bool  `json:"folder_index,omitempty" description:"true marks this file as its folder's entry point (an index or README): when the folder holds many files, conversations are shown just this file for it. false clears the mark."`
}

// SharedFileWriteRequest is a validated call, handed to the handler.
type SharedFileWriteRequest struct {
	Path           string
	Mode           string
	Content        string
	Description    string
	ExpectedSHA256 string
	FolderIndex    *bool
}

// SharedFileWriteResult is what the tool reports back to the model.
type SharedFileWriteResult struct {
	Status         string `json:"status"`
	Path           string `json:"path"`
	Mode           string `json:"mode"`
	Created        bool   `json:"created,omitempty"`
	SizeBytes      int64  `json:"size_bytes"`
	SHA256         string `json:"sha256"`
	PreviousSHA256 string `json:"previous_sha256,omitempty"`
	CanUndo        bool   `json:"can_undo,omitempty"`
	Description    string `json:"description,omitempty"`
	FolderIndex    bool   `json:"folder_index,omitempty"`
	Message        string `json:"message"`
}

// SharedFileWriteHandler performs a write against the library. Injected onto
// the turn context by httpapi (chat turns) and by the scheduled runner.
type SharedFileWriteHandler func(ctx context.Context, req SharedFileWriteRequest) (*SharedFileWriteResult, error)

type sharedFileWriteHandlerKey struct{}

// WithSharedFileWriteHandler attaches a per-turn SharedFileWriteHandler to ctx.
func WithSharedFileWriteHandler(ctx context.Context, h SharedFileWriteHandler) context.Context {
	if h == nil {
		return ctx
	}
	return context.WithValue(ctx, sharedFileWriteHandlerKey{}, h)
}

func sharedFileWriteHandlerFromContext(ctx context.Context) SharedFileWriteHandler {
	if ctx == nil {
		return nil
	}
	h, _ := ctx.Value(sharedFileWriteHandlerKey{}).(SharedFileWriteHandler)
	return h
}

const sharedFileWriteDescription = `Write a TEXT file into this server's shared file library, so EVERY conversation and scheduled task on this server can read it at shared/<path> — including brand-new chats, which are told the file exists (with your description) at the start of every turn.

Use it for a record the team relies on across chats: a running log (e.g. every optimization applied to a deal, with who, when and why), a register, an index. Do NOT use it for scratch work or one-off outputs — those belong in your own workspace. Never write under shared/ with bash, run_python, write_file or edit_file: that tree is read-only; this tool is the only way in.

Modes:
- create: a new file. Fails if it exists. description is required.
- append: add text to the end (the right mode for logs: one call per new row or entry). Creates the file if it does not exist yet (then description is required). Concurrent appends from different chats never overwrite each other.
- replace: write the whole file. Requires expected_sha256 — the sha256 of the version you read (view_file prints it at the end; this tool returns it). If someone changed the file since, the write is refused: read it again, merge your change into the new version, retry. The displaced version is kept for one undo.
- undo: restore the version the last replace displaced.

Give the text as content (small edits, rows) or from_path (a file in your workspace you built with run_python — no need to paste it). Text only (UTF-8: CSV, Markdown, JSON, YAML…), up to 25 MB per file; past that, start a new file (one per month or per campaign).

Files an administrator published are read-only. Everyone on this server can read what you write here, so never put credentials or anything one user should not see. When you read a shared file to answer a question, filter it (e.g. pandas in run_python) and show only the matching rows — never print a whole large file.`

// NewSharedFileWriteTool constructs shared_file_write, bound to the turn's
// sandbox for from_path reads (the same read path view_file uses). With a nil
// sandbox, content still works and from_path fails closed.
func NewSharedFileWriteTool(sb *sandbox.Sandbox) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		SharedFileWriteToolName,
		sharedFileWriteDescription,
		func(ctx context.Context, params SharedFileWriteParams, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			res, err := runSharedFileWrite(ctx, sb, params)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			if out, mErr := json.MarshalIndent(res, "", "  "); mErr == nil {
				return fantasy.NewTextResponse(string(out)), nil
			}
			return fantasy.NewTextResponse(res.Message), nil
		},
	)
}

// NormalizeSharedFileMode maps a mode name or alias to its canonical value, or
// "" when unrecognized.
func NormalizeSharedFileMode(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "create", "new":
		return SharedFileModeCreate
	case "append", "add":
		return SharedFileModeAppend
	case "replace", "overwrite", "write", "update":
		return SharedFileModeReplace
	case "undo", "restore", "revert":
		return SharedFileModeUndo
	default:
		return ""
	}
}

func runSharedFileWrite(ctx context.Context, sb *sandbox.Sandbox, params SharedFileWriteParams) (*SharedFileWriteResult, error) {
	h := sharedFileWriteHandlerFromContext(ctx)
	if h == nil {
		return nil, fmt.Errorf("%s is not available in this run", SharedFileWriteToolName)
	}
	mode := NormalizeSharedFileMode(params.Mode)
	if mode == "" {
		return nil, fmt.Errorf("invalid mode %q: must be 'create', 'append', 'replace', or 'undo'", params.Mode)
	}
	if strings.TrimSpace(params.Path) == "" {
		return nil, fmt.Errorf("path is required, e.g. 'optimizations/OptLog_Client.csv'")
	}
	req := SharedFileWriteRequest{
		Path:           params.Path,
		Mode:           mode,
		Description:    params.Description,
		ExpectedSHA256: strings.TrimSpace(params.ExpectedSHA256),
		FolderIndex:    params.FolderIndex,
	}
	hasContent, hasFrom := params.Content != "", strings.TrimSpace(params.FromPath) != ""
	switch {
	case mode == SharedFileModeUndo:
		if hasContent || hasFrom {
			return nil, fmt.Errorf("undo takes neither content nor from_path: it restores the version the last replace displaced")
		}
	case hasContent && hasFrom:
		return nil, fmt.Errorf("give either content or from_path, not both")
	case hasFrom:
		text, err := readWorkspaceText(ctx, sb, params.FromPath)
		if err != nil {
			return nil, err
		}
		req.Content = text
	case hasContent:
		if len(params.Content) > SharedFileMaxInlineBytes {
			return nil, fmt.Errorf("content is %d bytes, over the %d-byte limit for inline text: write the file in your workspace and pass from_path", len(params.Content), SharedFileMaxInlineBytes)
		}
		req.Content = params.Content
	case mode == SharedFileModeAppend:
		return nil, fmt.Errorf("append needs content or from_path")
	default:
		// create / replace with empty text is allowed (an empty file is a
		// file); nothing to read.
	}
	return h(ctx, req)
}

// readWorkspaceText reads a file from the turn's workspace through the
// sandbox file-op seam — the same resolution and no-follow policy view_file
// uses, so from_path names exactly the file the model sees.
func readWorkspaceText(ctx context.Context, sb *sandbox.Sandbox, path string) (string, error) {
	if sb == nil {
		return "", fmt.Errorf("from_path needs this turn's workspace, which is not available here: pass the text as content instead")
	}
	resolved, err := resolveWorkspacePath(ctx, path)
	if err != nil {
		return "", fmt.Errorf("from_path: %w", err)
	}
	validPath, err := validateFileOpReadPath(resolved)
	if err != nil {
		return "", fmt.Errorf("from_path: %w", err)
	}
	root, err := fileOpRoot(ctx, resolved, validPath, false)
	if err != nil {
		return "", fmt.Errorf("from_path: %w", err)
	}
	res, err := sb.RunFileOp(ctx, sandbox.FileOpRequest{
		Op:      sandbox.FileOpRead,
		Path:    validPath,
		Root:    root,
		MaxSize: SharedFileMaxBytes,
	})
	switch {
	case errors.Is(err, sandbox.ErrFileOpNotFound):
		return "", fmt.Errorf("from_path: %s is not in your workspace", path)
	case errors.Is(err, sandbox.ErrFileOpTooLarge):
		return "", fmt.Errorf("from_path: %s is over the %d-byte limit for a shared file: split it (for example one file per month or per campaign)", path, SharedFileMaxBytes)
	case err != nil:
		return "", fmt.Errorf("from_path: %w", fileOpError("read", err))
	}
	if res.Size > int64(len(res.Data)) {
		return "", fmt.Errorf("from_path: read %d of %d bytes; the file changed while it was read — try again", len(res.Data), res.Size)
	}
	return string(res.Data), nil
}
