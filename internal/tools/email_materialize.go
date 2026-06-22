package tools

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Email argument materialization, shared by every governed staging surface
// (the web approval stager and the ACP ingress approver). An agent's
// send_email / preview_email tool call may reference a workspace content_file
// or relative attachment paths, but run_python writes into
// workspace/<convID>/ while the Go server and the email MCP subprocess each
// have their own cwd — so a bare filename never resolves downstream. The fix
// is the same on every transport: at stage time (the one place that holds the
// convID and where the files are still on disk) inline content_file and rewrite
// relative attachment paths to absolute. Living here keeps both call sites
// byte-identical (the staged args must equal the args replayed post-approval).

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
	path := os.ExpandEnv(file)
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, path[2:])
		}
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(WorkspaceDirForConversation(convID), path)
	}

	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("read content_file %q: %w", file, err)
	}
	if info.Size() > MaxInlinedContentBytes {
		return "", fmt.Errorf("content_file %q is %d bytes, exceeds %d-byte inline cap", file, info.Size(), MaxInlinedContentBytes)
	}
	data, err := os.ReadFile(path) //nolint:gosec // path resolved within per-conversation workspace by caller
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
// Symmetric with MaterializeContentFile: same convID, same workspace resolution,
// same `~/` and `$VAR` expansion. Skips entries that are already absolute,
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
	rewriteList := func(key string) {
		raw, ok := args[key].([]any)
		if !ok {
			return
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
			path := os.ExpandEnv(file)
			if strings.HasPrefix(path, "~/") {
				if home, err := os.UserHomeDir(); err == nil {
					path = filepath.Join(home, path[2:])
				}
			}
			if !filepath.IsAbs(path) {
				path = filepath.Join(WorkspaceDirForConversation(convID), path)
			}
			if path != obj["path"] {
				obj["path"] = path
				raw[i] = obj
				changed = true
			}
		}
		args[key] = raw
	}
	rewriteList("attachments")
	rewriteList("inline_attachments")
	if !changed {
		return rawInput, nil
	}
	out, err := json.Marshal(args)
	if err != nil {
		return rawInput, err
	}
	return string(out), nil
}
