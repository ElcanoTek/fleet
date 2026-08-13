package sandbox

// bridge_protocol.go holds the python-bridge wire types + parser shared by BOTH
// backends (container.go and the tagged host.go). It is intentionally untagged —
// container mode needs it in a release build, so it must not live behind the
// fleet_host_executor tag with the host executor (#159).

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
)

// bridgeResponseCaptureCap bounds how many bytes of a single bridge response
// line are held in host memory. The bridge caps its own stdout/stderr/result
// captures (MAX_CAPTURE_BYTES in python_bridge.py), but `vars` extraction is
// not size-bounded, so a cell returning a huge variable produces an
// arbitrarily large response line — and an uncapped ReadBytes would buffer all
// of it on the host. Same ceiling as the bash path's BashOutputCaptureCap;
// bytes past it are drained and counted (see readCappedLine) rather than
// stored.
const bridgeResponseCaptureCap = BashOutputCaptureCap

// readCappedLine reads one newline-terminated line from r, keeping at most
// limit bytes and draining-but-counting the rest — the same
// store-then-discard semantics as cappedBuffer on the bash path, applied to
// the bridge's response stream. Draining to the delimiter keeps the stream
// framed for the next request even when a response overflows the cap. The
// returned data includes the newline when it fell within the cap; err mirrors
// bufio.Reader.ReadBytes (nil means the delimiter was found).
func readCappedLine(r *bufio.Reader, limit int) (data []byte, discarded int64, err error) {
	for {
		frag, ferr := r.ReadSlice('\n')
		keep := limit - len(data)
		if keep > len(frag) {
			keep = len(frag)
		}
		if keep > 0 {
			// ReadSlice's bytes are only valid until the next read — copy.
			data = append(data, frag[:keep]...)
		}
		discarded += int64(len(frag) - keep)
		if errors.Is(ferr, bufio.ErrBufferFull) {
			continue
		}
		return data, discarded, ferr
	}
}

// bridgeRequest mirrors the JSON shape the embedded bridge.py expects on
// stdin. Field names must match — change in lockstep.
type bridgeRequest struct {
	Code           string   `json:"code"`
	ReturnVars     []string `json:"return_vars,omitempty"`
	TimeoutSeconds int      `json:"timeout_seconds,omitempty"`
	WorkspaceDir   string   `json:"workspace_dir,omitempty"`
	// ResetKernel asks the bridge to discard the current IPython kernel
	// (clearing all variables/imports) and start a fresh one before running
	// Code. Only meaningful in persistent REPL mode (#213); in per-turn mode the
	// kernel is fresh each turn anyway.
	ResetKernel bool `json:"reset_kernel,omitempty"`
}

// bridgeResponse mirrors the JSON shape the bridge writes back. Fields
// align with sandbox.PythonResult; we re-pack rather than alias so the
// public type stays the bridge's wire detail.
//
// NOTE: parseBridgeResponse does a direct struct conversion PythonResult(resp),
// so the two types MUST keep identical field order and types (struct tags are
// ignored by the conversion). Add fields to BOTH in lockstep.
type bridgeResponse struct {
	Status           string                       `json:"status"`
	Output           string                       `json:"output"`
	Stdout           string                       `json:"stdout"`
	Stderr           string                       `json:"stderr"`
	Result           string                       `json:"result"`
	Vars             map[string]any               `json:"vars"`
	Error            string                       `json:"error"`
	BridgeTruncation map[string]BridgeCaptureInfo `json:"bridge_truncation,omitempty"`
	// ImageFiles are workspace-relative paths the bridge wrote for each
	// image/png the kernel emitted (matplotlib figures etc.), so the chat UI can
	// render them inline without the agent calling plt.savefig() (#213).
	ImageFiles []string `json:"image_files,omitempty"`
}

func parseBridgeResponse(data []byte) (PythonResult, error) {
	var resp bridgeResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		preview := string(bytes.TrimSpace(data))
		if len(preview) > 240 {
			preview = preview[:240] + "..."
		}
		log.Printf("sandbox: bridge response not JSON: %v (preview: %s)", err, preview)
		return PythonResult{}, fmt.Errorf("parse bridge response: %w", err)
	}
	return PythonResult(resp), nil
}
