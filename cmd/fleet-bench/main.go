// Command fleet-bench is a load-testing tool for a running fleet instance (#296).
//
// It answers capacity-planning questions the correctness test suite can't:
// how many concurrent chats an instance sustains before latency degrades, and
// whether turns leak goroutines/memory. It drives the chat server's HTTP+SSE
// path directly with the two required headers, and — pointed at a fleet whose
// OPENROUTER_BASE_URL is the fake-LLM seam (internal/fakellm) — costs nothing and
// is deterministic (use the [[echo:…]] marker, or a scenario marker).
//
// It is a standalone operator/dev tool: it makes ordinary HTTP requests and
// never imports or mutates the server's internals, so it has zero blast radius
// on a production request path.
//
// Usage:
//
//	fleet-bench chat --server http://127.0.0.1:8080 --email you@org \
//	    --token-env FLEET_SERVER_TOKEN --concurrency 20 --duration 30s
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "chat":
		if err := runChat(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "fleet-bench chat: %v\n", err)
			os.Exit(1)
		}
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `fleet-bench — load-test a running fleet instance (#296)

Subcommands:
  chat    Concurrently drive chat turns (POST /chat + read the SSE stream to
          completion) and report turn latency percentiles, throughput, errors,
          and goroutine/memory growth.

Run "fleet-bench chat -h" for flags.

Point the target fleet at the fake-LLM seam (OPENROUTER_BASE_URL) so runs cost
nothing and are deterministic; use --message "[[echo:hi]] …" for a fixed reply.
`)
}
