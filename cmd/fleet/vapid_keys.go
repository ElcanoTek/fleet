package main

import (
	"fmt"
	"io"

	webpushgo "github.com/SherClockHolmes/webpush-go"
)

// `fleet generate-vapid-keys` (#292): generate a fresh VAPID (RFC 8292)
// ECDSA P-256 key pair and print the env lines the operator pastes into
// their env-file to enable browser Web Push. Boots nothing — it works on a
// box where the DBs and sandbox are down, like `fleet version`.
//
// The PRIVATE key is a secret: it is printed exactly once here (that is the
// point — the operator must transcribe it) and must live only in the host
// env-file, never in the repo or a log.

// runGenerateVAPIDKeys generates the pair and writes the env lines to w.
// Returns the process exit code.
func runGenerateVAPIDKeys(w io.Writer) int {
	priv, pub, err := webpushgo.GenerateVAPIDKeys()
	if err != nil {
		fmt.Fprintf(w, "generate VAPID keys: %v\n", err)
		return 1
	}
	fmt.Fprint(w, vapidEnvLines(pub, priv))
	return 0
}

// vapidEnvLines renders the three env-file lines (# comments included, so the
// block can be pasted verbatim). Split from runGenerateVAPIDKeys so the exact
// output contract is unit-testable without generating keys.
func vapidEnvLines(publicKey, privateKey string) string {
	return "# Browser Web Push (#292) — add these to your fleet env-file.\n" +
		"# FLEET_VAPID_PRIVATE_KEY is a SECRET: keep it host-side only.\n" +
		"# Set FLEET_VAPID_CONTACT to a real operator address (mailto: or https:).\n" +
		"FLEET_VAPID_PUBLIC_KEY=" + publicKey + "\n" +
		"FLEET_VAPID_PRIVATE_KEY=" + privateKey + "\n" +
		"FLEET_VAPID_CONTACT=mailto:admin@example.com\n"
}
