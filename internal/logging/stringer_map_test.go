package logging

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

// credKey is a map KEY type carrying a secret-named field.
type credKey struct {
	Host  string
	Token string
}

// keyedByCred is a Stringer map whose String() prints its keys.
type keyedByCred map[credKey]int

func (m keyedByCred) String() string { return fmt.Sprint(map[credKey]int(m)) }

// credMapError is the same as an error.
type credMapError map[credKey]string

func (m credMapError) Error() string { return fmt.Sprint(map[credKey]string(m)) }

// anyKeyed is a Stringer map with interface keys: the secret-named key is a
// runtime string, not visible in the key type.
type anyKeyed map[any]string

func (m anyKeyed) String() string { return fmt.Sprint(map[any]string(m)) }

// A Stringer or error map whose KEYS carry a secret, by key type or by
// runtime key name, does not log by its own method: hidesSecretFields walks
// keys as well as values (#2074 review).
func TestStringerMapWithSecretKeysIsNotPrinted(t *testing.T) {
	const cred = "mapKeyCredPlain-Hw73Tz" // matches no pattern, not registered
	var buf bytes.Buffer
	slog.New(NewHandler("json", &buf, slog.LevelDebug)).Info("x",
		"stringer", keyedByCred{{Host: "h", Token: cred}: 1},
		"err", credMapError{{Host: "h", Token: cred}: "v"},
		"anykey", anyKeyed{"api_key": cred},
	)
	if out := buf.String(); strings.Contains(out, cred) || !strings.Contains(out, `"msg":"x"`) {
		t.Fatalf("a secret map key leaked through String()/Error(): %s", out)
	}
	for _, v := range []any{keyedByCred{{Token: cred}: 1}, credMapError{{Token: cred}: "v"}, anyKeyed{"api_key": cred}} {
		if !hidesSecretFields(v) {
			t.Fatalf("hidesSecretFields(%T) = false", v)
		}
	}
	if hidesSecretFields(anyKeyed{"host": "h"}) {
		t.Fatal("an innocent runtime key was taken for a secret")
	}
}
