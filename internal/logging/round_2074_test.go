package logging

import (
	"bytes"
	"encoding/json"
	"log"
	"log/slog"
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

// leakyStringer is a composite whose String() prints its credential.
type leakyStringer struct {
	Name       string
	Credential string
}

func (l leakyStringer) String() string { return l.Name + ":" + l.Credential }

// fixedID stands in for a UUID: a byte array whose String() is its meaning.
type fixedID [4]byte

func (f fixedID) String() string { return "id-01020304" }

// A Stringer composite whose type carries a secret-named field is walked by
// structure — the key rule redacts the Credential — not flattened by its own
// String(); value types whose String() is their meaning (time.Time, a UUID,
// net.IP) still print by it (#2074 review).
func TestStringerCompositeWithSecretFieldIsWalked(t *testing.T) {
	const cred = "stringerCredPlain-Zq81" // matches no pattern, not registered
	var buf bytes.Buffer
	when := time.Date(2026, 11, 8, 1, 2, 3, 0, time.UTC)
	slog.New(NewHandler("json", &buf, slog.LevelDebug)).Info("x",
		"conn", leakyStringer{Name: "acme", Credential: cred},
		"connp", &leakyStringer{Name: "acme", Credential: cred},
		"nested", []any{leakyStringer{Name: "n", Credential: cred}},
		"at", slog.AnyValue(when),
		"id", fixedID{1, 2, 3, 4},
		"ip", net.IPv4(10, 0, 0, 1),
	)
	out := buf.String()
	if strings.Contains(out, cred) {
		t.Fatalf("the credential leaked through String(): %s", out)
	}
	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	conn, _ := m["conn"].(map[string]any)
	if conn["Name"] != "acme" || conn["Credential"] != redactedValue {
		t.Fatalf("conn = %v, want walked with the Credential redacted by key", m["conn"])
	}
	if m["id"] != "id-01020304" || m["ip"] != "10.0.0.1" || m["at"] != "2026-11-08T01:02:03Z" {
		t.Fatalf("value-type Stringers changed: id=%v ip=%v at=%v", m["id"], m["ip"], m["at"])
	}
}

// A child cannot supply the attributes the parent adds to its records: a
// child field named child/line/truncated is suffixed, so the record's child=
// is the parent's (#2074 review).
func TestChildCannotSpoofTheParentsKeys(t *testing.T) {
	out := captureOutput(t)
	w := ChildStderr("mcp-broker")
	_, _ = w.Write([]byte(`{"level":"INFO","msg":"hi","child":"spoof","line":"forged","truncated":true}` + "\n"))
	recs := out.records(t)
	if len(recs) != 1 {
		t.Fatalf("records = %v", recs)
	}
	m := recs[0]
	if m["child"] != "mcp-broker" || m["child#2"] != "spoof" || m["line#2"] != "forged" || m["truncated#2"] != true {
		t.Fatalf("record = %v, want the parent's child and the child's fields suffixed", m)
	}
	if _, ok := m["line"]; ok {
		t.Fatalf("a child field took the parent's line key: %v", m)
	}
}

// JSON with a msg but no valid slog level is not a child record. It takes
// the raw "child stderr" path, which is labelled warn and is not gated by
// FLEET_LOG_LEVEL (its true severity is unknown — here it says fatal), so it
// survives FLEET_LOG_LEVEL=error (#2074 review: it was re-logged at an
// assumed info and dropped).
func TestJSONWithoutAValidLevelSurvivesTheLevelGate(t *testing.T) {
	prevSlog := slog.Default()
	prevBridge := bridgeLogger
	prevOut := currentOutput()
	t.Cleanup(func() {
		slog.SetDefault(prevSlog)
		bridgeLogger = prevBridge
		setOutput(prevOut)
		log.SetOutput(os.Stderr)
		log.SetFlags(log.LstdFlags)
	})
	path := t.TempDir() + "/fleet.log"
	closer, err := Configure(Config{File: path, MaxSizeMB: 10, Format: "json", Level: "error"})
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()

	w := ChildStderr("mcp-broker")
	_, _ = w.Write([]byte(`{"msg":"x","severity":"fatal"}` + "\n"))
	_, _ = w.Write([]byte(`{"level":"bogus","msg":"y"}` + "\n"))
	_, _ = w.Write([]byte(`{"level":"INFO","msg":"a real info record"}` + "\n"))

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var wrapped int
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) != nil {
			continue
		}
		if m["msg"] == "a real info record" {
			t.Fatalf("a real child info record passed the error level: %s", line)
		}
		if m["msg"] == ChildLineMessage && m["level"] == "WARN" && m["child"] == "mcp-broker" {
			wrapped++
		}
	}
	if wrapped != 2 {
		t.Fatalf("want both level-less lines as warn child-stderr records, got %d:\n%s", wrapped, data)
	}
}
