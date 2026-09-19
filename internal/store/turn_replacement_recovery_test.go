package store

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRecoveryRetractsFlushedAndUnflushedDrafts(t *testing.T) {
	events := []TurnEvent{
		{Name: "text.delta", Data: []byte(`{"text":"superseded before tool"}`)},
		{Name: "tool.call", Data: []byte(`{"id":"call","name":"bash","input":"{}"}`)},
		{Name: "text.delta", Data: []byte(`{"text":"superseded after tool"}`)},
		{Name: "text.replace", Data: []byte(`{"text":"authoritative answer"}`)},
	}
	entries, _ := buildRecoveredEntries(nil, events)
	encoded, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	if strings.Contains(text, "superseded") || !strings.Contains(text, "authoritative answer") || !strings.Contains(text, "tool_call") || !strings.Contains(text, "tool_result") {
		t.Fatalf("incorrect replacement recovery: %s", text)
	}
}
