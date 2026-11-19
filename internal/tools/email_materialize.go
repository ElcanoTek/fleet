package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Email argument materialization, used by the governed staging surface (the web
// approval stager). An agent's send_email / preview_email tool call may reference
// a workspace content_file or relative attachment paths, but run_python writes
// into workspace/<convID>/ while the Go server and the email MCP subprocess each
// have their own cwd — so a bare filename never resolves downstream. The fix is
// applied at stage time (the one place that holds the convID and where the files
// are still on disk): inline content_file and rewrite relative attachment paths
// to absolute. Living here keeps the staged args and the args replayed
// post-approval byte-identical.

// MaxInlinedContentBytes caps what we pull off disk into a staged approval.
// SendGrid accepts ~30 MiB total including attachments; a ten-megabyte body is
// already far beyond any reasonable email and would bloat the approvals row, the
// SSE event, and the UI preview state. If a legitimate use case ever needs more,
// lift the cap — don't quietly truncate.
const MaxInlinedContentBytes = 10 << 20

// IsEmailToolName reports whether toolName is an email tool whose args need
// content_file / attachment-path materialization. Matches the generic
// suffix the policy layer uses (send_email, preview_email, mcp_<server>_send_email).
func IsEmailToolName(toolName string) bool {
	return toolName == "preview_email" || toolName == "send_email" || strings.HasSuffix(toolName, "_send_email")
}

// MaterializeContentFile reads content_file (relative paths resolved against the
// conversation workspace that run_python chdirs into) and rewrites the JSON args
// so content holds the inline bytes and content_file is removed. Returns the
// unchanged rawInput if the args don't parse or don't name a file. When
// content_file is set, it always takes precedence over any inline content —
// matching the tool descriptions and the MCP sendgrid server's behavior.
//
// The path is contained to the conversation workspace (#573): no $VAR or ~
// expansion, absolute paths outside the workspace are rejected, the
// conversation directory is opened without following a symbolic link, and the
// file is resolved and opened through it ("..", and links that leave it, are
// refused).
func MaterializeContentFile(convID, rawInput string) (string, error) {
	var args map[string]any
	if err := json.Unmarshal([]byte(rawInput), &args); err != nil {
		return rawInput, nil //nolint:nilerr // non-JSON args pass through unchanged
	}
	if _, ok := args["content_file"].(string); !ok {
		return rawInput, nil
	}
	if strings.TrimSpace(args["content_file"].(string)) == "" {
		return rawInput, nil
	}
	if err := MaterializeContentFileMap(convID, args); err != nil {
		return "", err
	}
	out, err := json.Marshal(args)
	if err != nil {
		return rawInput, err
	}
	return string(out), nil
}

// MaterializeContentFileMap resolves args["content_file"] inside the
// conversation workspace and replaces it in place with inline args["content"].
// No-op when content_file is absent or blank.
func MaterializeContentFileMap(convID string, args map[string]any) error {
	if args == nil {
		return nil
	}
	file, _ := args["content_file"].(string)
	file = strings.TrimSpace(file)
	if file == "" {
		return nil
	}

	// content_file takes precedence over inline content: the tool descriptions
	// for both preview_email and send_email document this contract, and the MCP
	// sendgrid server enforces the same rule. Always read the file when
	// content_file is set, replacing any inline content the agent may have provided.
	//
	// content_file is model-controlled (steerable via prompt injection), and its
	// bytes become an OUTBOUND email body, so containment here stops an agent
	// from inlining host secrets (/proc/self/environ, ~/.aws/credentials) into
	// mail (#573). Four rules:
	//   1. No $VAR / ~ expansion: a model-authored path resolves literally,
	//      never through the fleet process environment.
	//   2. The conversation directory is opened without following a symbolic
	//      link (OpenConversationDir): the sandbox can replace it with one.
	//   3. An absolute path must already live under the conversation workspace;
	//      anything else is rejected outright.
	//   4. The (now relative) path is resolved and opened through that
	//      directory (OpenResolved): ".." components and links that leave it are
	//      refused, and nothing that changes after the resolution is followed.
	dir, err := OpenConversationDir(context.Background(), convID)
	if err != nil {
		return fmt.Errorf("read content_file %q: %w", file, err)
	}
	defer dir.Close()
	rel := file
	if filepath.IsAbs(file) {
		r, ok := dir.RelOf(file)
		if !ok {
			return fmt.Errorf("content_file %q is outside the conversation workspace %s", file, dir.Path())
		}
		rel = r
	}
	f, info, _, err := dir.OpenResolved(rel)
	if err != nil {
		return fmt.Errorf("read content_file %q: %w", file, err)
	}
	defer f.Close()
	if info.Size() > MaxInlinedContentBytes {
		return fmt.Errorf("content_file %q is %d bytes, exceeds %d-byte inline cap", file, info.Size(), MaxInlinedContentBytes)
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxInlinedContentBytes+1))
	if err != nil {
		return fmt.Errorf("read content_file %q: %w", file, err)
	}
	if len(data) > MaxInlinedContentBytes {
		return fmt.Errorf("content_file %q is over the %d-byte inline cap", file, MaxInlinedContentBytes)
	}

	args["content"] = string(data)
	delete(args, "content_file")
	return nil
}

// MaterializeAttachmentPaths rewrites every relative `path` inside the
// `attachments` and `inline_attachments` arrays to an absolute path rooted at
// the conversation workspace dir. The sendgrid MCP resolves paths against ITS
// cwd, which is not the per-conversation workspace — so bare filenames the agent
// passes (e.g. "chart.png" written by run_python into workspace/<convID>/) never
// resolve at send time, and the post-approval send errors out with "Inline
// attachment file not found." Doing this at staging time means the staged args
// row carries absolute paths, and the replay after approval works.
//
// No `~/` or `$VAR` expansion: the path is model-authored, and os.ExpandEnv
// used to substitute host env values into persisted approval args.
//
// Every path, absolute ones included, must lie inside one of the two places an
// agent's files legitimately live (attachmentBasesFor): the conversation's own
// workspace directory, or the staged shared-file library. When the path already
// exists, it is resolved through its symlinks and must still land inside one of
// them as a regular file. The file need not exist at staging time (some flows
// stage before the file is on disk), so this is a staging-time check only: the
// authoritative gate is SnapshotEmailAttachments, which re-resolves each path
// immediately before the send and hands the connector a private copy. Skips
// unparseable args, missing arrays, and non-string path fields.
func MaterializeAttachmentPaths(convID, rawInput string) (string, error) {
	var args map[string]any
	if err := json.Unmarshal([]byte(rawInput), &args); err != nil {
		return rawInput, nil //nolint:nilerr // non-JSON args pass through unchanged
	}
	// Short-circuit: skipping the marshal round-trip when neither array exists
	// keeps rawInput byte-identical for the common no-attachment case (and avoids
	// alphabetizing keys in the args row).
	_, hasA := args["attachments"].([]any)
	_, hasI := args["inline_attachments"].([]any)
	if !hasA && !hasI {
		return rawInput, nil
	}
	bases, err := attachmentBasesFor(convID)
	if err != nil {
		return "", err
	}
	changed, err := forEachAttachmentPath(args, func(file string) (string, error) {
		path := bases.lexical(file)
		if err := bases.checkStaged(path); err != nil {
			return "", err
		}
		return path, nil
	})
	if err != nil {
		return "", err
	}
	if !changed {
		return rawInput, nil
	}
	out, err := json.Marshal(args)
	if err != nil {
		return rawInput, err
	}
	return string(out), nil
}

// forEachAttachmentPath calls fn with every non-empty attachment path
// (trimmed) in the `attachments` and `inline_attachments` arrays of args,
// writes the value fn returns back in its place, and reports whether any value
// changed. An item is either an object carrying a string `path`
// ({"path": "chart.png", ...}) or a bare path string ("report.xlsx"), and each
// keeps its shape: an object gets its `path` replaced, a string is replaced by
// the new string. Bare strings used to be skipped, which let them reach the
// connector unchecked, un-snapshotted and still relative.
func forEachAttachmentPath(args map[string]any, fn func(path string) (string, error)) (bool, error) {
	changed := false
	for _, key := range []string{"attachments", "inline_attachments"} {
		raw, ok := args[key].([]any)
		if !ok {
			continue
		}
		for i, item := range raw {
			var current string
			switch v := item.(type) {
			case string:
				current = v
			case map[string]any:
				p, ok := v["path"].(string)
				if !ok {
					continue
				}
				current = p
			default:
				continue
			}
			file := strings.TrimSpace(current)
			if file == "" {
				continue
			}
			next, err := fn(file)
			if err != nil {
				return false, err
			}
			if next == current {
				continue
			}
			if obj, ok := item.(map[string]any); ok {
				obj["path"] = next
			} else {
				raw[i] = next
			}
			changed = true
		}
		args[key] = raw
	}
	return changed, nil
}

// attachmentBases are the two directories an attachment may come from, both
// absolute: the run's own working directory (a conversation's workspace, or a
// scheduled job's) and the staged shared-file library (reachable through the
// `shared` link and mounted into both kinds of sandbox). The library is a
// legitimate source because the shared-files prompt block tells agents to use
// `shared/<name>`. root is the workspace root they live under, which the
// attachment copies must stay out of. openConv opens the working directory
// without following a symbolic link (the sandbox can replace it with one); its
// path is only ever used lexically.
type attachmentBases struct {
	conv     string
	shared   string
	root     string
	openConv func() (*WorkspaceDir, error)
}

func attachmentBasesFor(convID string) (attachmentBases, error) {
	if !filepath.IsLocal(convID) || strings.ContainsAny(convID, `/\`) {
		return attachmentBases{}, fmt.Errorf("attachments need a conversation workspace (conversation id %q is not usable)", convID)
	}
	conv, err := filepath.Abs(WorkspaceDirForConversation(convID))
	if err != nil {
		return attachmentBases{}, fmt.Errorf("resolve conversation workspace for attachments: %w", err)
	}
	shared := SharedFilesDir(workspaceRootForContainment())
	conv = filepath.Clean(conv)
	return attachmentBases{
		conv: conv, shared: filepath.Clean(shared), root: filepath.Dir(conv),
		openConv: func() (*WorkspaceDir, error) { return OpenConversationDir(context.Background(), convID) },
	}, nil
}

// attachmentBasesForDir is attachmentBases for a run whose working directory
// is workDir under workspaceRoot (a scheduled job's directory, a worktree, or
// the root itself when the operator chose the shared workspace).
func attachmentBasesForDir(workspaceRoot, workDir string) (attachmentBases, error) {
	if strings.TrimSpace(workDir) == "" || strings.TrimSpace(workspaceRoot) == "" {
		return attachmentBases{}, errors.New("attachments need the run's workspace directory")
	}
	dir, err := filepath.Abs(workDir)
	if err != nil {
		return attachmentBases{}, fmt.Errorf("resolve run workspace for attachments: %w", err)
	}
	root, err := filepath.Abs(workspaceRoot)
	if err != nil {
		return attachmentBases{}, fmt.Errorf("resolve workspace root for attachments: %w", err)
	}
	dir, root = filepath.Clean(dir), filepath.Clean(root)
	return attachmentBases{
		conv: dir, shared: filepath.Clean(SharedFilesDir(root)), root: root,
		openConv: func() (*WorkspaceDir, error) { return OpenRunDir(context.Background(), root, dir) },
	}, nil
}

// lexical anchors a model-authored path: relative paths are joined onto the
// conversation workspace; absolute ones are only cleaned.
func (b attachmentBases) lexical(file string) string {
	path := filepath.Clean(file)
	if !filepath.IsAbs(path) {
		path = filepath.Join(b.conv, path)
	}
	return path
}

// checkStagingDir refuses an attachment staging directory (dir, which exists)
// that lies inside the workspace root, the run's working directory or the
// shared library. Each is compared both as written and with symlinks
// resolved on both sides, so a staging parent that is (or sits under) a link
// into a workspace is refused too. A path that cannot be resolved is refused,
// except a base that does not exist, which can hold nothing.
func (b attachmentBases) checkStagingDir(dir string) error {
	refuse := func(cause error) error {
		if cause != nil {
			return fmt.Errorf("attachment staging directory must be outside the workspace root: %w", cause)
		}
		return errors.New("attachment staging directory must be outside the workspace root")
	}
	dirAbs, err := filepath.Abs(dir)
	if err != nil {
		return refuse(err)
	}
	dirReal, err := filepath.EvalSymlinks(dirAbs)
	if err != nil {
		return refuse(err)
	}
	if dirReal, err = filepath.Abs(dirReal); err != nil {
		return refuse(err)
	}
	for _, base := range []string{b.root, b.conv, b.shared} {
		if within(base, dirAbs) || within(base, dirReal) {
			return refuse(nil)
		}
		baseReal, err := filepath.EvalSymlinks(base)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return refuse(err)
		}
		if baseReal, err = filepath.Abs(baseReal); err != nil {
			return refuse(err)
		}
		if within(baseReal, dirReal) {
			return refuse(nil)
		}
	}
	return nil
}

// within reports whether path is dir or lies under it (both cleaned, absolute).
func within(dir, path string) bool {
	return path == dir || strings.HasPrefix(path, dir+string(filepath.Separator))
}

func outsideErr(path string) error {
	return fmt.Errorf("attachment path %q is outside this run's workspace — attach files the agent wrote to its workspace (or shared files) instead", path)
}

// checkStaged is the staging-time check: lexical containment always, and for
// an existing entry, resolution through the opened directory plus a
// regular-file check.
func (b attachmentBases) checkStaged(path string) error {
	if !within(b.conv, path) && !within(b.shared, path) {
		return outsideErr(path)
	}
	dir, rel, err := b.source(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil // need not exist yet; SnapshotEmailAttachments checks again before sending
		}
		return err
	}
	defer dir.Close()
	info, err := dir.Stat(rel)
	if err != nil {
		return fmt.Errorf("attachment %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("attachment %q is not a regular file", path)
	}
	return nil
}

// source resolves path to the directory it lives in — the run's working
// directory, opened without following a symbolic link, or the shared library —
// and its canonical name there. A path under the working directory that
// leads, through a symbolic link, into the shared library (the seeded
// `shared` link) resolves there; anything that resolves anywhere else is
// refused. The caller closes the returned directory.
func (b attachmentBases) source(path string) (*WorkspaceDir, string, error) {
	if within(b.conv, path) {
		rel, err := filepath.Rel(b.conv, path)
		if err != nil {
			return nil, "", outsideErr(path)
		}
		dir, err := b.openConv()
		if err != nil {
			return nil, "", fmt.Errorf("attachment %q: %w", path, err)
		}
		canon, err := dir.Resolve(rel)
		if err == nil && canon != "." {
			return dir, canon, nil
		}
		_ = dir.Close()
		if err == nil {
			return nil, "", outsideErr(path)
		}
		if target := OutsideTarget(err); target != "" && within(b.shared, target) {
			return b.sharedSource(target, path)
		}
		if errors.Is(err, ErrPathEscapesWorkspace) {
			return nil, "", outsideErr(path)
		}
		return nil, "", fmt.Errorf("attachment %q: %w", path, err)
	}
	if within(b.shared, path) {
		return b.sharedSource(path, path)
	}
	return nil, "", outsideErr(path)
}

// sharedSource resolves target inside the shared library. The library is
// mounted read-only into every sandbox (a mount point no sandbox can rename),
// so it is opened as a host directory.
func (b attachmentBases) sharedSource(target, path string) (*WorkspaceDir, string, error) {
	dir, err := OpenHostDir(b.shared)
	if err != nil {
		return nil, "", fmt.Errorf("attachment %q: %w", path, err)
	}
	rel, ok := dir.RelOf(target)
	if !ok || rel == "." {
		_ = dir.Close()
		return nil, "", outsideErr(path)
	}
	canon, err := dir.Resolve(rel)
	if err != nil || canon == "." {
		_ = dir.Close()
		if err == nil || errors.Is(err, ErrPathEscapesWorkspace) {
			return nil, "", outsideErr(path)
		}
		return nil, "", fmt.Errorf("attachment %q: %w", path, err)
	}
	return dir, canon, nil
}

// OutboundAttachmentStagingDir is where SnapshotEmailAttachments puts the
// copies an email is sent with: <emailAttachmentDir>/outbound, a host-side
// directory no sandbox mounts. "" when emailAttachmentDir is unset (the
// snapshot then uses the OS temp directory).
//
// The result is always absolute (resolved against the process cwd). The
// copies' paths are written into the tool arguments and read by the email
// connector, an MCP subprocess running in its own cwd (a per-run
// mcp-runs/task-<id> directory), so a relative staging path, which the
// default "./data/attachments" used to produce, never resolved there and the
// send failed with "attachment file not found".
func OutboundAttachmentStagingDir(emailAttachmentDir string) string {
	dir := strings.TrimSpace(emailAttachmentDir)
	if dir == "" {
		return ""
	}
	dir = filepath.Join(dir, "outbound")
	if abs, err := filepath.Abs(dir); err == nil {
		return abs
	}
	return dir
}

// MaxEmailAttachmentBytes bounds the attachments one email may carry in total,
// matching the ~30 MiB message ceiling of the email providers fleet targets.
const MaxEmailAttachmentBytes = 30 << 20

// SnapshotEmailAttachments is the send-time half of attachment handling. For
// each attachment path in args it re-resolves the path against the
// conversation's workspace — opened without following a symbolic link — or
// the shared library, opens it through that directory with no link followed
// at any component (OpenNoFollow), requires a regular file on the opened
// descriptor, and copies at most MaxEmailAttachmentBytes in total into
// a fresh private directory under stagingParent. args is rewritten in place to
// name those copies (keeping each file's base name, which the connector uses as
// the attachment name), so the connector reads files nothing else can change
// between this check and the send. The returned cleanup removes the copies; it
// is non-nil whenever err is nil.
func SnapshotEmailAttachments(convID string, args map[string]any, stagingParent string) (cleanup func(), err error) {
	if !hasAttachmentLists(args) {
		return func() {}, nil
	}
	bases, err := attachmentBasesFor(convID)
	if err != nil {
		return nil, err
	}
	return snapshotAttachments(bases, args, stagingParent)
}

// SnapshotEmailAttachmentsInDir is SnapshotEmailAttachments for a run that
// has no conversation: a scheduled task run, whose working directory workDir
// (its job directory, a worktree, or the root under the shared-workspace
// setting) is the directory its sandbox mounts. Attachments must resolve
// inside workDir or the shared-file library under workspaceRoot.
func SnapshotEmailAttachmentsInDir(workspaceRoot, workDir string, args map[string]any, stagingParent string) (cleanup func(), err error) {
	if !hasAttachmentLists(args) {
		return func() {}, nil
	}
	bases, err := attachmentBasesForDir(workspaceRoot, workDir)
	if err != nil {
		return nil, err
	}
	return snapshotAttachments(bases, args, stagingParent)
}

func hasAttachmentLists(args map[string]any) bool {
	_, hasA := args["attachments"].([]any)
	_, hasI := args["inline_attachments"].([]any)
	return hasA || hasI
}

func snapshotAttachments(bases attachmentBases, args map[string]any, stagingParent string) (cleanup func(), err error) {
	if stagingParent == "" {
		stagingParent = os.TempDir()
	}
	// The copies' paths are handed to a connector running in another cwd, so
	// they must be absolute whatever the caller passed.
	if abs, err := filepath.Abs(stagingParent); err == nil {
		stagingParent = abs
	}
	if err := os.MkdirAll(stagingParent, 0o700); err != nil {
		return nil, fmt.Errorf("prepare attachment staging: %w", err)
	}
	dir, err := os.MkdirTemp(stagingParent, "outbound-")
	if err != nil {
		return nil, fmt.Errorf("prepare attachment staging: %w", err)
	}
	cleanup = func() { _ = os.RemoveAll(dir) }
	// The staging directory must not be somewhere a sandbox can see.
	if err := bases.checkStagingDir(dir); err != nil {
		cleanup()
		return nil, err
	}
	var total int64
	n := 0
	_, err = forEachAttachmentPath(args, func(file string) (string, error) {
		path := bases.lexical(file)
		if !within(bases.conv, path) && !within(bases.shared, path) {
			return "", outsideErr(path)
		}
		src, rel, err := bases.source(path)
		if err != nil {
			return "", err
		}
		n++
		dst := filepath.Join(dir, strconv.Itoa(n), filepath.Base(rel))
		copied, err := copyFromDir(src, rel, dst, MaxEmailAttachmentBytes-total)
		_ = src.Close()
		if err != nil {
			return "", fmt.Errorf("attachment %q: %w", path, err)
		}
		total += copied
		return dst, nil
	})
	if err != nil {
		cleanup()
		return nil, err
	}
	return cleanup, nil
}

// copyFromDir copies rel inside src to dst, opening the source with no
// symbolic link followed at any component (rel is the canonical name source
// returned) and requiring a regular file on the opened descriptor. It refuses
// a source larger than budget bytes and never copies more than that.
func copyFromDir(src *WorkspaceDir, rel, dst string, budget int64) (int64, error) {
	in, info, err := OpenNoFollow(src.Root, rel)
	if err != nil {
		return 0, err
	}
	defer in.Close()
	if info.Size() > budget {
		return 0, fmt.Errorf("attachments exceed the %d-byte limit for one email", MaxEmailAttachmentBytes)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return 0, err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // dst is under a directory this function's caller just created with MkdirTemp
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(out, io.LimitReader(in, budget+1))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return 0, err
	}
	if n > budget {
		return 0, fmt.Errorf("attachments exceed the %d-byte limit for one email", MaxEmailAttachmentBytes)
	}
	return n, nil
}

// workspaceRootForContainment is the workspace root: $FLEET_WORKSPACE_ROOT,
// else ./workspace — the same resolution
// WorkspaceDirForConversation applies before appending the id. The shared-file
// library is staged directly under it.
func workspaceRootForContainment() string {
	if root := fleetEnv("WORKSPACE_ROOT"); root != "" {
		return root
	}
	return "workspace"
}
