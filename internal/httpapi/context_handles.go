package httpapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ElcanoTek/fleet/internal/tools"
)

// Composer context handles (#517): a chat message may inline `@url:<url>` and
// `@file:"path"` handles; the server expands them (concurrently) into the turn
// context BEFORE the run — @url via the SSRF-guarded host-side fetcher, @file
// read through the conversation workspace opened without following a symbolic
// link (tools.OpenDirNoFollow, WorkspaceDir.OpenResolved). This
// is a host-side I/O pass that prepares the model's input; the sandbox remains
// the execution boundary and credentials never enter the prompt. Expansion NEVER
// fails the turn — a bad handle degrades to a notice appended for the model +
// user. OPT-IN (FLEET_CONTEXT_HANDLES_ENABLED): @url fetching a user-supplied URL
// is a host-side outbound surface, so it is off by default.

const (
	// maxContextHandles caps how many handles one message may expand (bounds
	// concurrent fetches/reads from a single turn).
	maxContextHandles = 8
	// maxContextFileBytes caps a single @file read so a large workspace file can't
	// blow out the context window.
	maxContextFileBytes = 256 * 1024
	// contextExpandTimeout bounds the whole concurrent expansion.
	contextExpandTimeout = 25 * time.Second
)

var (
	// urlHandleRe matches `@url:<url>` — the URL is a non-whitespace run.
	urlHandleRe = regexp.MustCompile(`@url:(\S+)`)
	// fileHandleRe matches `@file:"path"` — quoted so paths with spaces work; the
	// path is resolved relative to the conversation workspace.
	fileHandleRe = regexp.MustCompile(`@file:"([^"\n]+)"`)
)

// applyContextHandles expands the message's context handles into userMessage when
// the feature is enabled (#517), else returns userMessage unchanged. Kept as a
// method so postChat stays a single statement (no added branch / complexity).
func (s *Server) applyContextHandles(ctx context.Context, userMessage, rawMessage, conversationID string) string {
	if !s.cfg.LiveContextHandlesEnabled() {
		return userMessage
	}
	blocks, notices := expandContextHandles(ctx, rawMessage, tools.WorkspaceDirForConversation(conversationID))
	return appendContextHandleBlocks(userMessage, blocks, notices)
}

type expandedHandle struct {
	order  int
	block  string // expanded content (empty on failure)
	notice string // human-readable failure notice (empty on success)
}

// expandContextHandles parses and concurrently expands @url / @file handles in
// message. workspaceRoot gates @file (empty disables it). Returns content blocks
// (in the order the handles appear) plus notices for any that failed. Never
// errors: the turn proceeds regardless.
func expandContextHandles(ctx context.Context, message, workspaceRoot string) (blocks []string, notices []string) {
	type handleMatch struct {
		kind  string // "url" | "file"
		arg   string
		start int // byte offset of the handle in message
	}
	// Collect BOTH kinds with their offsets and order them by position in the
	// message, so the blocks (and the cap below) follow the order the user
	// wrote. Appending all URL matches before all file matches — the previous
	// shape — put every @file after every @url regardless of the text, and
	// a message with maxContextHandles URLs then dropped its files entirely.
	var matches []handleMatch
	for _, m := range urlHandleRe.FindAllStringSubmatchIndex(message, -1) {
		matches = append(matches, handleMatch{kind: "url", arg: message[m[2]:m[3]], start: m[0]})
	}
	for _, m := range fileHandleRe.FindAllStringSubmatchIndex(message, -1) {
		matches = append(matches, handleMatch{kind: "file", arg: message[m[2]:m[3]], start: m[0]})
	}
	if len(matches) == 0 {
		return nil, nil
	}
	sort.SliceStable(matches, func(a, b int) bool { return matches[a].start < matches[b].start })
	dropped := 0
	if len(matches) > maxContextHandles {
		dropped = len(matches) - maxContextHandles
		matches = matches[:maxContextHandles]
	}

	ctx, cancel := context.WithTimeout(ctx, contextExpandTimeout)
	defer cancel()

	// Each expansion reports on a buffered channel, and collection stops at the
	// deadline: a handle still being fetched or read then becomes a notice, and
	// its goroutine finishes into the buffer without holding the turn.
	done := make(chan expandedHandle, len(matches))
	for i, mt := range matches {
		go func(i int, mt handleMatch) {
			eh := expandedHandle{order: i}
			defer func() { done <- eh }()
			switch mt.kind {
			case "url":
				text, err := tools.FetchURLForContext(ctx, mt.arg)
				if err != nil {
					eh.notice = fmt.Sprintf("could not fetch @url:%s", mt.arg)
				} else {
					eh.block = fmt.Sprintf("**Fetched @url:%s**\n\n%s", mt.arg, text)
				}
			case "file":
				eh.block, eh.notice = expandFileHandle(ctx, workspaceRoot, mt.arg)
			}
		}(i, mt)
	}
	results := make([]expandedHandle, 0, len(matches))
	finished := make([]bool, len(matches))
collect:
	for range matches {
		select {
		case eh := <-done:
			results = append(results, eh)
			finished[eh.order] = true
		case <-ctx.Done():
			break collect
		}
	}
	for i, mt := range matches {
		if !finished[i] {
			results = append(results, expandedHandle{order: i, notice: fmt.Sprintf("@%s:%s timed out", mt.kind, mt.arg)})
		}
	}

	sort.SliceStable(results, func(a, b int) bool { return results[a].order < results[b].order })
	for _, r := range results {
		if r.block != "" {
			blocks = append(blocks, r.block)
		}
		if r.notice != "" {
			notices = append(notices, r.notice)
		}
	}
	if dropped > 0 {
		notices = append(notices, fmt.Sprintf("%d additional context handle(s) ignored (max %d per message)", dropped, maxContextHandles))
	}
	return blocks, notices
}

// expandFileHandle reads relPath from the conversation workspace: the
// workspace directory is opened without following a symbolic link at its own
// name (the sandbox can replace it with one), relPath is resolved inside it
// (absolute paths, "..", and links that leave it are refused) and opened with
// no link followed at any component, required to be a regular file on the
// opened descriptor, and size-capped. Returns (block, notice); exactly one is
// non-empty.
func expandFileHandle(ctx context.Context, workspaceRoot, relPath string) (block, notice string) {
	if strings.TrimSpace(workspaceRoot) == "" {
		return "", fmt.Sprintf("@file is unavailable for this conversation: %s", relPath)
	}
	// Syntactic gate first, so a path that is not allowed reads as such
	// rather than as "not found".
	if tools.CheckRelativePath(relPath) != nil {
		return "", fmt.Sprintf("@file path not allowed: %s", relPath)
	}
	wsDir, err := tools.OpenDirNoFollow(ctx, workspaceRoot)
	if err != nil {
		if errors.Is(err, tools.ErrWorkspaceDirLink) {
			return "", fmt.Sprintf("@file could not be read: %s", relPath)
		}
		return "", fmt.Sprintf("@file not found: %s", relPath)
	}
	defer wsDir.Close()
	f, _, _, err := wsDir.OpenResolved(relPath)
	switch {
	case errors.Is(err, tools.ErrPathEscapesWorkspace), errors.Is(err, tools.ErrUnsafePath):
		return "", fmt.Sprintf("@file path not allowed: %s", relPath)
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, tools.ErrNotRegularFile):
		return "", fmt.Sprintf("@file not found: %s", relPath)
	case err != nil:
		return "", fmt.Sprintf("@file could not be read: %s", relPath)
	}
	defer f.Close()

	data, err := io.ReadAll(io.LimitReader(f, maxContextFileBytes+1))
	if err != nil {
		return "", fmt.Sprintf("@file could not be read: %s", relPath)
	}
	truncated := len(data) > maxContextFileBytes
	if truncated {
		data = data[:maxContextFileBytes]
	}
	content := string(data)
	if !utf8.ValidString(content) {
		return "", fmt.Sprintf("@file is not UTF-8 text: %s", relPath)
	}
	block = fmt.Sprintf("**Contents of @file:%q:**\n\n```\n%s\n```", relPath, content)
	if truncated {
		block += "\n_(truncated)_"
	}
	return block, ""
}

// appendContextHandleBlocks appends the expanded blocks + notices to the user
// message (mirrors appendAttachmentsBlock / appendWorkspaceInventoryBlock). A
// no-op when there is nothing to append.
func appendContextHandleBlocks(message string, blocks, notices []string) string {
	if len(blocks) == 0 && len(notices) == 0 {
		return message
	}
	var b strings.Builder
	b.WriteString(message)
	for _, blk := range blocks {
		b.WriteString("\n\n---\n")
		b.WriteString(blk)
	}
	if len(notices) > 0 {
		b.WriteString("\n\n---\n**Context handle notices:**\n")
		for _, n := range notices {
			b.WriteString("- ")
			b.WriteString(n)
			b.WriteString("\n")
		}
	}
	return b.String()
}
