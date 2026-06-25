package sandbox

// bridge_protocol.go holds the python-bridge wire types + parser shared by BOTH
// backends (container.go and the tagged host.go). It is intentionally untagged —
// container mode needs it in a release build, so it must not live behind the
// fleet_host_executor tag with the host executor (#159).

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
)

// bridgeRequest mirrors the JSON shape the embedded bridge.py expects on
// stdin. Field names must match — change in lockstep.
type bridgeRequest struct {
	Code           string   `json:"code"`
	ReturnVars     []string `json:"return_vars,omitempty"`
	TimeoutSeconds int      `json:"timeout_seconds,omitempty"`
	WorkspaceDir   string   `json:"workspace_dir,omitempty"`
}

// bridgeResponse mirrors the JSON shape the bridge writes back. Fields
// align with sandbox.PythonResult; we re-pack rather than alias so the
// public type stays the bridge's wire detail.
type bridgeResponse struct {
	Status           string                       `json:"status"`
	Output           string                       `json:"output"`
	Stdout           string                       `json:"stdout"`
	Stderr           string                       `json:"stderr"`
	Result           string                       `json:"result"`
	Vars             map[string]any               `json:"vars"`
	Error            string                       `json:"error"`
	BridgeTruncation map[string]BridgeCaptureInfo `json:"bridge_truncation,omitempty"`
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
