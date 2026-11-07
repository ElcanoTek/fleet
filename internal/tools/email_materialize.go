package tools

import (
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
// expansion, absolute paths outside the workspace are rejected, and the final
// resolution goes through SafeWorkspaceJoin (".." + symlink-escape reject).
func MaterializeContentFile(convID, rawInput string) (string, error) {
	var args map[string]any
	if err := json.Unmarshal([]byte(rawInput), &args); err != nil {
		return rawInput, nil //nolint:nilerr // non-JSON args pass through unchanged
	}
	file, _ := args["content_file"].(string)
	file = strings.TrimSpace(file)
	if file == "" {
		return rawInput, nil
	}

	// content_file takes precedence over inline content — the tool descriptions
	// for both preview_email and send_email document this contract, and the MCP
	// sendgrid server enforces the same rule. Always read the file when
	// content_file is set, replacing any inline content the agent may have provided.
	//
	// content_file is model-controlled (steerable via prompt injection), and its
	// bytes become an OUTBOUND email body — so containment here is what stops an
	// agent inlining host secrets (/proc/self/environ, ~/.aws/credentials) into
	// mail (#573). Three rules, mirroring SafeWorkspaceJoin:
	//   1. No $VAR / ~ expansion — an attacker-supplied path must resolve
	//      literally, never through the fleet process environment.
	//   2. An absolute path must already live under the conversation workspace;
	//      anything else is rejected outright.
	//   3. The (now relative) path goes through SafeWorkspaceJoin, which rejects
	//      ".." components and symlink escapes.
	workspace, err := filepath.Abs(WorkspaceDirForConversation(convID))
	if err != nil {
		return "", fmt.Errorf("resolve workspace for content_file %q: %w", file, err)
	}
	rel := file
	if filepath.IsAbs(file) {
		r, err := filepath.Rel(workspace, filepath.Clean(file))
		if err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("content_file %q is outside the conversation workspace %s", file, workspace)
		}
		rel = r
	}
	path, err := SafeWorkspaceJoin(workspace, rel)
	if err != nil {
		return "", fmt.Errorf("read content_file %q: %w", file, err)
	}

	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("read content_file %q: %w", file, err)
	}
	if info.Size() > MaxInlinedContentBytes {
		return "", fmt.Errorf("content_file %q is %d bytes, exceeds %d-byte inline cap", file, info.Size(), MaxInlinedContentBytes)
	}
	data, err := os.ReadFile(path) //nolint:gosec // path validated by SafeWorkspaceJoin to live under the conversation workspace
	if err != nil {
		return "", fmt.Errorf("read content_file %q: %w", file, err)
	}

	args["content"] = string(data)
	delete(args, "content_file")
	out, err := json.Marshal(args)
	if err != nil {
		return rawInput, err
	}
	return string(out), nil
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
// attachment copies must stay out of.
type attachmentBases struct {
	conv   string
	shared string
	root   string
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
	return attachmentBases{conv: conv, shared: filepath.Clean(shared), root: filepath.Dir(conv)}, nil
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
	return attachmentBases{conv: filepath.Clean(dir), shared: filepath.Clean(SharedFilesDir(root)), root: filepath.Clean(root)}, nil
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
// an existing entry, symlink resolution plus a regular-file check.
func (b attachmentBases) checkStaged(path string) error {
	if !within(b.conv, path) && !within(b.shared, path) {
		return outsideErr(path)
	}
	if _, err := os.Lstat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil // need not exist yet; SnapshotEmailAttachments checks again before sending
		}
		return fmt.Errorf("attachment %q: %w", path, err)
	}
	base, rel, err := b.resolve(path)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(base)
	if err != nil {
		return fmt.Errorf("attachment %q: %w", path, err)
	}
	defer root.Close()
	info, err := root.Stat(rel)
	if err != nil {
		return fmt.Errorf("attachment %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("attachment %q is not a regular file", path)
	}
	return nil
}

// resolve follows path's symlinks and returns the base directory it resolves
// under (the conversation workspace or the shared library, as they resolve
// themselves) and the path relative to that base. A path that resolves
// anywhere else is refused.
func (b attachmentBases) resolve(path string) (base, rel string, err error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", "", fmt.Errorf("attachment %q: %w", path, err)
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return "", "", fmt.Errorf("attachment %q: %w", path, err)
	}
	for _, dir := range []string{b.conv, b.shared} {
		dirResolved, derr := filepath.EvalSymlinks(dir)
		if derr != nil {
			continue
		}
		if dirResolved, derr = filepath.Abs(dirResolved); derr != nil {
			continue
		}
		if !within(dirResolved, resolved) || resolved == dirResolved {
			continue
		}
		r, rerr := filepath.Rel(dirResolved, resolved)
		if rerr != nil || !filepath.IsLocal(r) {
			continue
		}
		return dirResolved, r, nil
	}
	return "", "", outsideErr(path)
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
// conversation's workspace (or the shared library), opens it through os.Root so
// the open itself cannot leave that directory, requires a regular file on the
// opened descriptor, and copies at most MaxEmailAttachmentBytes in total into
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
		base, rel, err := bases.resolve(path)
		if err != nil {
			return "", err
		}
		n++
		dst := filepath.Join(dir, strconv.Itoa(n), filepath.Base(rel))
		copied, err := copyFromRoot(base, rel, dst, MaxEmailAttachmentBytes-total)
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

// copyFromRoot copies base/rel to dst, opening the source through os.Root and
// requiring a regular file on the opened descriptor. It refuses a source larger
// than budget bytes and never copies more than that.
func copyFromRoot(base, rel, dst string, budget int64) (int64, error) {
	root, err := os.OpenRoot(base)
	if err != nil {
		return 0, err
	}
	defer root.Close()
	src, info, err := OpenRegularInRoot(root, rel)
	if err != nil {
		return 0, err
	}
	defer src.Close()
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
	n, err := io.Copy(out, io.LimitReader(src, budget+1))
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

// workspaceRootForContainment is the workspace root: $FLEET_WORKSPACE_ROOT /
// $CHAT_WORKSPACE_ROOT, else ./workspace — the same resolution
// WorkspaceDirForConversation applies before appending the id. The shared-file
// library is staged directly under it.
func workspaceRootForContainment() string {
	if root := fleetEnv("WORKSPACE_ROOT"); root != "" {
		return root
	}
	return "workspace"
}
