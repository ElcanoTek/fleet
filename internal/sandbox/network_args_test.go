package sandbox

import (
	"strings"
	"testing"
)

func TestNetworkArgs(t *testing.T) {
	join := func(ss []string) string { return strings.Join(ss, " ") }

	t.Run("lockdown wins over proxy", func(t *testing.T) {
		got := networkArgs(true, "http://tok:@10.0.2.2:9000")
		if join(got) != "--network=none" {
			t.Fatalf("lockdown args = %v, want only --network=none", got)
		}
	})

	t.Run("open is the slirp default (no args)", func(t *testing.T) {
		if got := networkArgs(false, ""); len(got) != 0 {
			t.Fatalf("open args = %v, want none", got)
		}
	})

	t.Run("allowlisted enables host-loopback + proxy env", func(t *testing.T) {
		url := "http://tok:@10.0.2.2:9000"
		got := join(networkArgs(false, url))
		for _, want := range []string{
			"--network=slirp4netns:allow_host_loopback=true",
			"HTTPS_PROXY=" + url,
			"HTTP_PROXY=" + url,
			"https_proxy=" + url,
			"NO_PROXY=localhost,127.0.0.1,10.0.2.2",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("allowlisted args missing %q\n  got: %s", want, got)
			}
		}
		// Must NOT seal the network in allowlisted mode (proxy is unreachable otherwise).
		if strings.Contains(got, "--network=none") {
			t.Error("allowlisted mode must not use --network=none")
		}
	})
}
