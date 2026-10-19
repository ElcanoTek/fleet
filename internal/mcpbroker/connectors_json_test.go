package mcpbroker

import (
	"encoding/json"
	"testing"
)

// An empty connector list crosses the broker boundary as [] (replace the
// parent's snapshot with nothing), distinct from an absent field (an older
// child: keep the snapshot).
func TestReloadResultConnectorsEmptyVersusAbsent(t *testing.T) {
	raw, err := json.Marshal(ReloadResult{Connectors: []ConnectorDescriptor{}})
	if err != nil {
		t.Fatal(err)
	}
	var got ReloadResult
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Connectors == nil || len(got.Connectors) != 0 {
		t.Fatalf("empty list decoded as %#v from %s, want []", got.Connectors, raw)
	}
	var old ReloadResult
	if err := json.Unmarshal([]byte(`{"summary":{}}`), &old); err != nil {
		t.Fatal(err)
	}
	if old.Connectors != nil {
		t.Fatalf("absent field decoded as %#v, want nil", old.Connectors)
	}
}
