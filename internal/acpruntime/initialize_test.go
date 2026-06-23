// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package acpruntime

import (
	"strings"
	"testing"

	acp "github.com/coder/acp-go-sdk"
)

// TestCheckInitializeResponse is the regression guard for #86: the negotiated
// InitializeResponse is validated (it was previously discarded), turning a
// protocol/auth mismatch into a clear diagnostic at spawn instead of an opaque
// later failure.
func TestCheckInitializeResponse(t *testing.T) {
	// Happy path: matching version, no auth methods.
	if err := checkInitializeResponse(acp.InitializeResponse{ProtocolVersion: acp.ProtocolVersionNumber}, "external agent"); err != nil {
		t.Fatalf("matching version + no auth should pass, got: %v", err)
	}

	// Protocol-version mismatch → clear error naming both versions.
	err := checkInitializeResponse(acp.InitializeResponse{ProtocolVersion: acp.ProtocolVersionNumber - 1}, "external agent")
	if err == nil || !strings.Contains(err.Error(), "protocol version") {
		t.Fatalf("version mismatch should error about the protocol version, got: %v", err)
	}

	// Advertised auth methods → clear config error (fleet uses model_env, not ACP auth).
	err = checkInitializeResponse(acp.InitializeResponse{
		ProtocolVersion: acp.ProtocolVersionNumber,
		AuthMethods:     []acp.AuthMethod{{}},
	}, "external agent")
	if err == nil || !strings.Contains(err.Error(), "authentication") {
		t.Fatalf("advertised auth methods should error about authentication, got: %v", err)
	}
}
