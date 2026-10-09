package agentcore

import (
	"testing"

	"github.com/ElcanoTek/fleet/internal/genui"
)

// A show_ui card the model is waiting on must survive replay: a tool-call
// input over HardMaxToolOutputBytes is replaced by a short envelope in
// reduceHistoricalPayloadsToHardCap, and the model would lose the card's
// labels and actions while the browser keeps it live. The validator's size
// limit is the raw tool input, so it must not exceed the replay cap.
func TestShowUICardFitsReplayCap(t *testing.T) {
	if genui.MaxSpecBytes > HardMaxToolOutputBytes {
		t.Fatalf("genui.MaxSpecBytes %d exceeds HardMaxToolOutputBytes %d", genui.MaxSpecBytes, HardMaxToolOutputBytes)
	}
}
