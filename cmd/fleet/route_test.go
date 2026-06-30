package main

import "testing"

// TestClassifyInvocation locks the unified-CLI routing contract (#461). The
// critical case is back-compat: bare `fleet` (no args) MUST classify as serve so
// a historical systemd unit (ExecStart=/usr/local/bin/fleet, no subcommand)
// keeps starting the daemon — otherwise a restart mid-upgrade would brick the
// box. `fleet serve` is the explicit equivalent; admin verbs route to admincli.
func TestClassifyInvocation(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		want invocation
	}{
		{"bare fleet → serve (legacy ExecStart compat)", nil, invokeServe},
		{"empty argv → serve", []string{}, invokeServe},
		{"explicit serve", []string{"serve"}, invokeServe},
		{"serve with extra args still serves", []string{"serve", "--whatever"}, invokeServe},
		{"version", []string{"version"}, invokeVersion},
		{"--version", []string{"--version"}, invokeVersion},
		{"-v", []string{"-v"}, invokeVersion},
		{"mcp-broker", []string{"mcp-broker"}, invokeMCPBroker},
		{"validate-config", []string{"validate-config", "--strict"}, invokeValidateConfig},
		{"update → admin", []string{"update"}, invokeAdmin},
		{"status → admin", []string{"status"}, invokeAdmin},
		{"chat → admin (TUI + chat-user admin)", []string{"chat"}, invokeAdmin},
		{"bootstrap → admin", []string{"bootstrap"}, invokeAdmin},
		{"unknown verb → admin (admincli prints usage)", []string{"frobnicate"}, invokeAdmin},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyInvocation(tc.argv); got != tc.want {
				t.Fatalf("classifyInvocation(%q) = %d, want %d", tc.argv, got, tc.want)
			}
		})
	}
}
