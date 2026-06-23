// Copyright (c) 2025 ElcanoTek
// All rights reserved. This is a private repository.

package acpruntime

import (
	"fmt"

	acp "github.com/coder/acp-go-sdk"
)

// checkInitializeResponse validates the negotiated ACP InitializeResponse both
// drivers (native + external) otherwise discard, turning a silent
// protocol/auth mismatch — which would otherwise fail opaquely later at
// NewSession/Prompt — into a clear, actionable diagnostic at spawn time.
//
//   - ProtocolVersion mismatch: fleet implements exactly acp.ProtocolVersionNumber.
//     A peer that negotiates a different version is incompatible; fail loud.
//   - AuthMethods advertised: fleet drives agents via model_env credentials and
//     does NOT perform the ACP `authenticate` handshake. A peer that requires
//     auth cannot be driven yet; fail with a config error rather than hanging at
//     the next call. (Wire a real client-side Authenticate when a provider that
//     advertises authMethods is onboarded.)
//
// agentLabel names the peer in the error (e.g. "external agent", "native agent").
func checkInitializeResponse(resp acp.InitializeResponse, agentLabel string) error {
	if resp.ProtocolVersion != acp.ProtocolVersionNumber {
		return fmt.Errorf("%s negotiated ACP protocol version %d, but fleet implements version %d — incompatible agent/bridge",
			agentLabel, resp.ProtocolVersion, acp.ProtocolVersionNumber)
	}
	if len(resp.AuthMethods) > 0 {
		return fmt.Errorf("%s advertises %d ACP authentication method(s), which fleet does not support — fleet drives agents via model_env credentials, not the ACP authenticate handshake",
			agentLabel, len(resp.AuthMethods))
	}
	return nil
}
