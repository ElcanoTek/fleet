package agent

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// Each chat turn's request must be a strict prefix of the next turn's: an
// implicit-only provider cache (Gemini 4 Argon) carries over only on an
// append, so a per-turn runtime_today note that lived only in the request
// tail made every new user turn miss (docs/PROMPT-CACHE-CONTRACT.md). The
// note is persisted as the user entry's day and replayed in place, so this
// holds across turns AND across a UTC-midnight rollover.
func TestTurnRequestIsAPrefixOfTheNext(t *testing.T) {
	day1 := time.Date(2026, 8, 12, 23, 58, 0, 0, time.UTC)
	day2 := day1.Add(5 * time.Minute) // past midnight: 2026-08-13

	turn1, entry1, err := assembleTurnMessages(TurnInput{UserMessage: "pull yesterday's report"}, day1)
	if err != nil {
		t.Fatal(err)
	}
	var c TextContent
	if err := json.Unmarshal(entry1.Content, &c); err != nil {
		t.Fatal(err)
	}
	if c.RuntimeToday != "2026-08-12" {
		t.Fatalf("persisted runtime_today = %q, want the turn's UTC day 2026-08-12", c.RuntimeToday)
	}
	if got := messageText(turn1[len(turn1)-1]); !strings.Contains(got, "runtime_today: 2026-08-12") {
		t.Fatalf("turn 1 must end with its runtime_today note, got %q", got)
	}

	history := []HistoryEntry{entry1, mustEntry("assistant", "text", TextContent{Text: "Here it is."})}
	turn2, _, err := assembleTurnMessages(TurnInput{UserMessage: "check again", History: history}, day2)
	if err != nil {
		t.Fatal(err)
	}
	if len(turn2) <= len(turn1) {
		t.Fatalf("turn 2 has %d messages, want more than turn 1's %d", len(turn2), len(turn1))
	}
	for i := range turn1 {
		a, _ := json.Marshal(turn1[i])
		b, _ := json.Marshal(turn2[i])
		if string(a) != string(b) {
			t.Fatalf("turn 2 diverges from turn 1 at message %d:\n turn1: %s\n turn2: %s", i, a, b)
		}
	}
	if got := messageText(turn2[len(turn2)-1]); !strings.Contains(got, "runtime_today: 2026-08-13") {
		t.Fatalf("turn 2 must end with the NEW day's note, got %q", got)
	}
}

// A row written before the day was persisted replays exactly as it was sent:
// no note is invented for it.
func TestReplayLegacyUserRowHasNoRuntimeNote(t *testing.T) {
	legacy := mustEntry("user", "text", TextContent{Text: "hello"})
	out, err := replayHistory([]HistoryEntry{legacy, mustEntry("assistant", "text", TextContent{Text: "hi"})}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Fatalf("replayed %d messages, want 2 (user, assistant)", len(out))
	}
	for _, m := range out {
		if strings.Contains(messageText(m), "runtime_today") {
			t.Fatalf("legacy row grew a runtime_today note on replay: %q", messageText(m))
		}
	}
	if raw, _ := json.Marshal(TextContent{Text: "hello"}); strings.Contains(string(raw), "runtime_today") {
		t.Fatalf("an entry with no day must not serialize one: %s", raw)
	}
}
