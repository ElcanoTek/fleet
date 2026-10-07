// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package remotemcp

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"syscall"
	"testing"

	"github.com/ElcanoTek/fleet/internal/mcp"
)

// TestVendorOutageSeparatesOutagesFromRot pins which live-smoke failures only
// warn (the vendor is down tonight) and which fail the nightly lane (the
// listing is wrong). It runs in the PR gate — no network — so a change to the
// transient classifier cannot quietly turn catalog rot into a warning.
func TestVendorOutageSeparatesOutagesFromRot(t *testing.T) {
	dial := func(err error) error {
		return &url.Error{Op: "Post", URL: "https://mcp.vendor.example", Err: &net.OpError{Op: "dial", Net: "tcp", Err: err}}
	}
	handshake := func(err error) error {
		return fmt.Errorf("failed to initialize server: %w", err)
	}
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"503 on notifications/initialized", handshake(fmt.Errorf("notification notifications/initialized: %w", &mcp.HTTPStatusError{StatusCode: 503})), true},
		{"502 at initialize", handshake(&mcp.HTTPStatusError{StatusCode: 502}), true},
		{"429", handshake(&mcp.HTTPStatusError{StatusCode: 429}), true},
		{"Cloudflare 522 (origin timed out)", handshake(&mcp.HTTPStatusError{StatusCode: 522}), true},
		{"Cloudflare 530", handshake(&mcp.HTTPStatusError{StatusCode: 530}), true},
		{"507", handshake(&mcp.HTTPStatusError{StatusCode: 507}), true},
		{"501 Not Implemented", handshake(&mcp.HTTPStatusError{StatusCode: 501}), false},
		{"connection refused", dial(os.NewSyscallError("connect", syscall.ECONNREFUSED)), true},
		{"temporary DNS failure", dial(&net.DNSError{Err: "temporary failure in name resolution", Name: "mcp.vendor.example", IsTemporary: true}), true},
		{"host gone", dial(&net.DNSError{Err: "no such host", Name: "mcp.vendor.example", IsNotFound: true}), false},
		{"404", handshake(&mcp.HTTPStatusError{StatusCode: 404}), false},
		{"401 on an open entry", handshake(&mcp.HTTPStatusError{StatusCode: 401}), false},
		{"not an MCP server", handshake(errors.New("MCP http: unparsable response")), false},
	}
	for _, c := range cases {
		if got := vendorOutage(c.err); got != c.want {
			t.Errorf("%s: vendorOutage = %v, want %v (err: %v)", c.name, got, c.want, c.err)
		}
	}
}
