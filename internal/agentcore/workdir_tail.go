package agentcore

import (
	"fmt"
	"path/filepath"

	"charm.land/fantasy"
)

// WorkingDirTurnSuffix is the per-run message-tail block that names the run's
// working directory and spells out the one rule MCP file outputs need.
//
// Interactive turns already get this in the system prompt (agent/prompt.go
// "## Working directory") because they carry a conversation id. Scheduled runs
// and delegated children do not — they carry a FORCED working dir instead —
// and so far told the model nothing. The consequence was watched in a daily
// refresh: the agent passed `output_dir: "sources"` to an email MCP tool, the
// MCP server (a separate process at the server root) wrote the attachment
// relative to ITSELF and reported success, the sandbox never saw the file, and
// the run correctly refused to publish with a source it could not read. Three
// retries, three different guesses, no guidance. Like the date tail, this
// lives in the evolving tail so the cached system prefix stays stable.
func WorkingDirTurnSuffix(dir string) string {
	abs := dir
	if a, err := filepath.Abs(dir); err == nil {
		abs = a
	}
	return fmt.Sprintf("## Working directory (this run)\n\n"+
		"Your working directory for this run is:\n\n    %s\n\n"+
		"- bash, run_python, view_file, write_file, and download_url already resolve relative paths against it: bare relative paths work there, and `download_url output_dir=\"sources\"` lands under it.\n"+
		"- MCP tools run in separate processes that do NOT share it. Whenever an MCP tool takes an `output_dir` (or equivalent output-path) argument, pass this ABSOLUTE path or a subdirectory of it. A relative or omitted output_dir makes the MCP server write into its own directory: the call reports success, but the file is not where your shell can read it.\n"+
		"- Prefer tools that hand back a URL or fleet-download handle and fetch it with download_url; that path always lands inside this directory.\n",
		abs)
}

// appendWorkingDirMessage appends the working-directory suffix as a trailing
// user message (same append-only pattern as the runtime-date tail).
func appendWorkingDirMessage(messages []fantasy.Message, dir string) []fantasy.Message {
	return append(messages, fantasy.NewUserMessage(WorkingDirTurnSuffix(dir)))
}
