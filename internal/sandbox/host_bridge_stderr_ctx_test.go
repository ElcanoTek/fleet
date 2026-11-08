//go:build fleet_host_executor

package sandbox

import (
	"testing"
)

// host backend: the bridge script prints a traceback on stderr, then answers.
func TestHostBridgeStderrCarriesTheRequestContext(t *testing.T) {
	if !pythonAvailable() {
		t.Skip("python3 not available")
	}
	out := captureBridgeLog(t)
	script := "import sys\n" +
		"for line in sys.stdin:\n" +
		"    sys.stderr.write('Traceback: boom-host\\n'); sys.stderr.flush()\n" +
		"    sys.stdout.write('{\"status\":\"ok\",\"result\":\"x\"}\\n'); sys.stdout.flush()\n"
	h := &hostImpl{bridgeScript: []byte(script)}
	defer h.close()
	if _, err := h.runPython(bridgeRequestCtx(), PythonRequest{Code: "1/0"}); err != nil {
		t.Fatalf("runPython: %v", err)
	}
	requireAttributedTraceback(t, out, "boom-host")
}
