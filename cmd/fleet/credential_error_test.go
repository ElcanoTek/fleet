package main

import (
	"errors"
	"strings"
	"testing"
)

// Inspect the typed diagnostic without relying on its public/loggable string.
// This preserves the exhaustive alias-refusal assertions while requiring the
// error message to omit credential-related raw configuration.
func overlapNames(t *testing.T, err error) string {
	t.Helper()
	var e *connectorEnvOverlapError
	if !errors.As(err, &e) {
		t.Fatalf("unexpected error type %T", err)
	}
	return strings.Join(e.names, ", ")
}

func TestOverlapErrorDoesNotEchoRawConfiguration(t *testing.T) {
	err := &connectorEnvOverlapError{names: []string{"FAKE_PASTED_CREDENTIAL"}}
	if strings.Contains(err.Error(), "FAKE_PASTED_CREDENTIAL") {
		t.Fatal("raw field leaked")
	}
	if !strings.Contains(err.Error(), "1 fields") {
		t.Fatal("missing actionable diagnostic")
	}
}
