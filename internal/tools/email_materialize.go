package tools

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
// Near-symmetric with MaterializeContentFile: same convID, same workspace
// anchoring for relative paths (plus the historical `~/` and `$VAR` expansion) —
// but unlike content_file (whose bytes this process reads and inlines, so it is
// containment-gated per #573), attachment paths are only REWRITTEN here; the
// sendgrid MCP subprocess is what opens them at send time, and files need not
// exist at staging time. Skips entries that are already absolute,
// unparseable args, missing arrays, or non-string path fields. Files don't need
// to exist at staging time — preview_email stages before the file is necessarily
// on disk in some flows; the real MCP call is the one that needs the file.
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
	changed := false
	rewriteList := func(key string) error {
		raw, ok := args[key].([]any)
		if !ok {
			return nil
		}
		for i, item := range raw {
			obj, ok := item.(map[string]any)
			if !ok {
				continue
			}
			file, ok := obj["path"].(string)
			if !ok {
				continue
			}
			file = strings.TrimSpace(file)
			if file == "" {
				continue
			}
			// No $VAR / ~ expansion (the same rule content_file adopted in
			// #573): the path is model-authored, and os.ExpandEnv substituted
			// the VALUE of any fleet env var — every connector secret the
			// host holds — into a string that was then persisted verbatim in
			// approvals.args_json, shown on the approval card, and replayed
			// to the connector.
			path := filepath.Clean(file)
			if !filepath.IsAbs(path) {
				path = filepath.Join(WorkspaceDirForConversation(convID), path)
			}
			if cerr := attachmentPathContained(path); cerr != nil {
				return cerr
			}
			if path != obj["path"] {
				obj["path"] = path
				raw[i] = obj
				changed = true
			}
		}
		args[key] = raw
		return nil
	}
	for _, key := range []string{"attachments", "inline_attachments"} {
		if err := rewriteList(key); err != nil {
			return "", err
		}
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

// attachmentPathContained refuses an attachment path that resolves outside
// the workspace root. The connector opens the file host-side at send time, so
// an absolute path here is a request to email a HOST file; the only files an
// agent can legitimately attach are the ones it (or an upload / the shared
// library) put under the workspace root. A prompt-injected
// `/etc/fleet/fleet.env` used to sail through to the approval card, one
// inattentive click from exfiltration. Lexical check (Clean + Rel), since the
// file need not exist at staging time.
func attachmentPathContained(path string) error {
	root, err := filepath.Abs(workspaceRootForContainment())
	if err != nil {
		return fmt.Errorf("resolve workspace root for attachment %q: %w", path, err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve attachment path %q: %w", path, err)
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("attachment path %q is outside the workspace root %s — attach files the agent wrote to its workspace (or shared files) instead", path, root)
	}
	return nil
}

// workspaceRootForContainment is the root every attachment must live under:
// $FLEET_WORKSPACE_ROOT / $CHAT_WORKSPACE_ROOT, else ./workspace — the same
// resolution WorkspaceDirForConversation applies before appending the id, so
// shared-library files staged directly under the root qualify too.
func workspaceRootForContainment() string {
	if root := fleetEnv("WORKSPACE_ROOT"); root != "" {
		return root
	}
	return "workspace"
}
