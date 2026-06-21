package tools

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/sandbox"
)

//go:embed python_bridge.py
var pythonBridgeScript []byte

// PythonBridgeScript returns the embedded bridge.py contents. Wired
// through to sandbox construction so the bridge ships as part of the
// Go binary (no separate distribution artifact).
func PythonBridgeScript() []byte { return pythonBridgeScript }

// RunPythonParams are the typed parameters for the run_python tool.
type RunPythonParams struct {
	Code           string   `json:"code" description:"The Python code to execute."`
	ReturnVars     []string `json:"return_vars,omitempty" description:"List of variable names to extract from the kernel after execution."`
	TimeoutSeconds int      `json:"timeout_seconds,omitempty" description:"Maximum time in seconds to wait for Python execution. Defaults to 300."`
}

const runPythonDescription = "Executes Python code in a per-turn IPython kernel inside a sandbox container. " +
	"Use for: pandas DataFrames, numpy/scipy calculations, charts (matplotlib/seaborn), Excel I/O, PDF read/generate, " +
	"image manipulation (Pillow), HTML/XML parsing (BeautifulSoup, lxml), ML (scikit-learn). " +
	"DO NOT use to: import MCP tools (they are separate callable tools), import from mcp/* or internal/* directories. " +
	"Other tools (search_emails, send_email, etc.) are called directly, NOT via Python imports.\n\n" +
	"PRE-INSTALLED — these are guaranteed available, no pip install needed (and pip won't work — see NETWORK):\n" +
	"  Data:      pandas, numpy, scipy, pyarrow\n" +
	"  Plotting:  matplotlib, seaborn\n" +
	"  ML:        scikit-learn\n" +
	"  Documents: openpyxl, xlsxwriter (Excel), pypdf, reportlab (PDF)\n" +
	"  Images:    pillow (PIL)\n" +
	"  Parsing:   beautifulsoup4, lxml, pyyaml, requests, tabulate\n\n" +
	"KERNEL LIFETIME — the IPython kernel is FRESH at the start of every turn and DESTROYED at turn end. " +
	"Variables, imports, and objects you define DO NOT survive into the next turn. To carry state across turns, " +
	"write to disk — any file in your workspace persists. Within ONE turn, multiple run_python calls share the same " +
	"kernel: `df = pd.read_csv(...)` in one call is still in scope on the next.\n\n" +
	"ALWAYS LEAVE A TRACE — end EVERY call with a verifiable checkpoint: print() row counts / key values / len(html), " +
	"or write the artifact to a workspace file and print its path and size. NEVER end a call with only assignments or " +
	"comments: a silent success gives you nothing to verify, and re-running identical code is blocked by a loop guard.\n\n" +
	"NETWORK — normal chats have outbound HTTP via rootless slirp4netns: `requests`, `urllib`, `socket`, " +
	"and `pip install` (in a child process) all reach the public internet. Lockdown chats run with the network " +
	"namespace sealed; those calls fail at the kernel layer. The webfetch MCP works in both modes and stages " +
	"the download into your workspace, so prefer it for one-off fetches when you don't know which mode you're in.\n\n" +
	"FILESYSTEM — your cwd is a private per-conversation scratch directory. Bare writes like `open('foo.html', 'w')` " +
	"land in THIS chat's scratch and are invisible to other chats. Files from supporting docs — `protocols/`, `personas/`, " +
	"`system_prompts/` — are exposed via symlinks inside your scratch so relative reads still work. For attachments the " +
	"user uploaded or the email MCP downloaded, use the absolute path from the tool that produced them.\n\n" +
	"DISPLAYING IMAGES — to show a chart or image to the user: save it to your workspace with `plt.savefig('chart.png')` " +
	"(or any other PNG/JPG/SVG writer) and reference it in your reply with markdown image syntax: `![Chart caption](chart.png)`. " +
	"The chat UI rewrites that relative filename to a per-conversation workspace URL and renders the image inline. " +
	"DO NOT base64-encode the image into an HTML <img src=\"data:...\"> block — that wastes tokens (a typical chart is ~20 KB " +
	"of base64 = thousands of completion tokens), thrashes the streaming renderer, and isn't even necessary. Just save and reference.\n\n" +
	"GIVING THE USER A DOWNLOAD LINK — to hand the user any workspace file (CSV, xlsx, PDF, .md, .html, .zip, ...), write a " +
	"normal markdown link whose target is the BARE relative filename exactly as it sits on disk: `[Report.xlsx](Report.xlsx)`. " +
	"The chat UI rewrites that to an authenticated per-conversation download URL and renders it as a click-to-download link — " +
	"the SAME rewrite that makes `![](chart.png)` work for images. Rules: (1) use the plain filename only — NO `sandbox:` " +
	"scheme, NO absolute path like `/opt/chat/workspace/<id>/file`, NO `file://`. Those are not download links and confuse the " +
	"rewrite. (2) Do NOT hand-percent-encode the filename: write `[My Report.csv](My Report.csv)`, not `My%20Report.csv` — the " +
	"UI encodes it for you. (3) The link text can be anything; only the target must be the on-disk name. (4) If the file is in a " +
	"subdir of the workspace, use the relative path (`out/report.csv`). This is the ONLY supported in-chat download mechanism — " +
	"do not tell the user a raw filesystem path, and do not detour through fast.io unless the user explicitly wants durable cross-chat storage.\n\n" +
	"PASSING DATA TO OTHER TOOLS — read this before chaining run_python into an MCP/native call:\n" +
	"  ⚠️  There is NO `${tool:...}`, `${var.name}`, or any other server-side placeholder substitution. " +
	"If you write `\"content_base64\": \"${tool:abc123.vars.payload}\"` into a downstream argument, that literal string is what the next tool receives — fast.io and friends will reject it as malformed input. " +
	"This is the single most common chaining mistake; do not make it.\n" +
	"  Correct pattern: set `return_vars=[\"payload\"]` on run_python. The full untruncated value comes back in the response's `vars` field — e.g. `\"vars\": {\"payload\": \"UEsDBBQ…the actual bytes…\"}`. " +
	"In your NEXT tool call you must take that exact string and inline it verbatim as the JSON value of the downstream parameter. `vars` values are NEVER truncated, so the copy is safe even for large payloads.\n" +
	"  When the payload is large (>10 KB) and the destination tool offers a path/URL/blob-id alternative (`fastio_upload_file path=...` for fast.io uploads, image tools that accept a workspace path, etc.), prefer that over inline base64 — it skips a round-trip through this model's context and is much harder to corrupt. The chat-server actively rejects oversized inline base64 uploads to fast.io; do not try to drive the upload yourself via `mcp_fast_io_upload action=stream-upload`."

const defaultPythonTimeoutSeconds = 300

// pythonOutputTruncateThreshold matches the bash tool threshold.
const pythonOutputTruncateThreshold = 32768 // ~8K tokens

// NewRunPythonTool returns a run_python tool bound to the given
// per-turn sandbox. The caller MUST pass a non-nil sandbox from the
// pool; the lifetime of the python kernel is the lifetime of that
// sandbox, which is the lifetime of the turn. A nil sandbox surfaces
// as a tool-call failure at runtime — there is no host-mode fallback.
func NewRunPythonTool(sb *sandbox.Sandbox) fantasy.AgentTool {
	return fantasy.NewAgentTool("run_python", runPythonDescription,
		func(ctx context.Context, params RunPythonParams, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			result, err := runPythonWithSandbox(ctx, sb, params)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			return fantasy.NewTextResponse(result), nil
		})
}

// pythonResponse is the wire shape the run_python tool returns to the
// LLM (matches the shape the legacy in-process implementation produced
// so cached tool-result fingerprints don't change).
type pythonResponse struct {
	Status           string                       `json:"status"`
	Output           string                       `json:"output"`
	Stdout           string                       `json:"stdout"`
	Stderr           string                       `json:"stderr"`
	Vars             map[string]any               `json:"vars"`
	Error            string                       `json:"error"`
	Hint             string                       `json:"hint,omitempty"`
	BridgeTruncation map[string]bridgeCaptureInfo `json:"bridge_truncation,omitempty"`
	ExecutionTimeMs  int64                        `json:"execution_time_ms,omitempty"`
	TruncationInfo   *truncationInfo              `json:"truncation_info,omitempty"`
}

// emptyOutputHint is appended to a successful run_python result that produced
// no output at all. An all-empty success response is information-free: it
// gives the model nothing to verify and nothing to anchor its next step, and
// at low temperature an unchanged context makes the model resample the exact
// same call — OMC conv 95697a52 replayed one such call 34 times. The hint both
// injects novel tokens (breaking the deterministic fixed point) and steers the
// model toward leaving a verifiable trace next time.
const emptyOutputHint = "Code ran successfully but produced NO output — no stdout, no stderr, no vars. " +
	"Do not re-run the same code; an identical call will return this same empty result. " +
	"End every run_python call with a verifiable trace: print() a checkpoint (row counts, key values, len(html)), " +
	"or write your artifact to a workspace file and print its path and size."

// maybeAddEmptyOutputHint sets resp.Hint when a successful execution produced
// nothing the model can act on.
func maybeAddEmptyOutputHint(resp *pythonResponse) {
	if resp.Error != "" || strings.TrimSpace(resp.Stderr) != "" {
		return // failures carry their own signal
	}
	if strings.TrimSpace(resp.Output) != "" || strings.TrimSpace(resp.Stdout) != "" || len(resp.Vars) > 0 {
		return
	}
	resp.Hint = emptyOutputHint
}

type bridgeCaptureInfo struct {
	Truncated     bool `json:"truncated"`
	CapturedBytes int  `json:"captured_bytes"`
	TotalBytes    int  `json:"total_bytes"`
}

func runPythonWithSandbox(ctx context.Context, sb *sandbox.Sandbox, params RunPythonParams) (string, error) {
	if sb == nil {
		return "", fmt.Errorf("run_python requires a sandbox; pool.Take returned nil or was bypassed")
	}
	if params.Code == "" {
		return "", fmt.Errorf("code is required")
	}
	timeoutSeconds := params.TimeoutSeconds
	if timeoutSeconds <= 0 {
		timeoutSeconds = defaultPythonTimeoutSeconds
	}

	workspaceDir := ""
	if convID := ConversationIDFromContext(ctx); convID != "" {
		if dir, err := EnsureWorkspaceDir(convID); err == nil {
			if abs, absErr := filepath.Abs(dir); absErr == nil {
				workspaceDir = abs
			} else {
				workspaceDir = dir
			}
		}
	}

	start := time.Now()
	out, runErr := sb.RunPython(ctx, sandbox.PythonRequest{
		Code:         params.Code,
		ReturnVars:   params.ReturnVars,
		Timeout:      time.Duration(timeoutSeconds) * time.Second,
		WorkspaceDir: workspaceDir,
	})
	if runErr != nil {
		return "", runErr
	}
	elapsed := time.Since(start)

	resp := pythonResponse{
		Status:          out.Status,
		Output:          out.Output,
		Stdout:          out.Stdout,
		Stderr:          out.Stderr,
		Vars:            out.Vars,
		Error:           out.Error,
		ExecutionTimeMs: elapsed.Milliseconds(),
	}
	if len(out.BridgeTruncation) > 0 {
		resp.BridgeTruncation = make(map[string]bridgeCaptureInfo, len(out.BridgeTruncation))
		for k, v := range out.BridgeTruncation {
			resp.BridgeTruncation[k] = bridgeCaptureInfo{
				Truncated:     v.Truncated,
				CapturedBytes: v.CapturedBytes,
				TotalBytes:    v.TotalBytes,
			}
		}
	}

	truncatePythonResponse(&resp)
	maybeAddEmptyOutputHint(&resp)

	jsonBytes, marshalErr := json.Marshal(resp)
	if marshalErr != nil {
		// JSON marshal failures shouldn't poison the turn — fall back to
		// the raw output. The marshal error is opaque to the model.
		return resp.Output, nil //nolint:nilerr
	}
	return string(jsonBytes), nil
}

// truncatePythonResponse handles large stdout/stderr by saving full
// content to temp files and replacing inline content with head+tail
// excerpts. Same shape the legacy implementation produced.
func truncatePythonResponse(resp *pythonResponse) {
	stdoutBytes := []byte(resp.Stdout)
	stderrBytes := []byte(resp.Stderr)
	if len(stdoutBytes) <= pythonOutputTruncateThreshold && len(stderrBytes) <= pythonOutputTruncateThreshold {
		return
	}
	ti := &truncationInfo{}
	if len(stdoutBytes) > pythonOutputTruncateThreshold {
		truncated, path := truncateWithFile(stdoutBytes, "python-stdout")
		ti.StdoutTruncated = true
		ti.StdoutFullPath = path
		ti.StdoutFullBytes = len(stdoutBytes)
		resp.Stdout = truncated
	}
	if len(stderrBytes) > pythonOutputTruncateThreshold {
		truncated, path := truncateWithFile(stderrBytes, "python-stderr")
		ti.StderrTruncated = true
		ti.StderrFullPath = path
		ti.StderrFullBytes = len(stderrBytes)
		resp.Stderr = truncated
	}
	resp.TruncationInfo = ti
}
