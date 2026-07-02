package httpapi

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/ElcanoTek/fleet/internal/tools"
)

// Composer context handles (#517): a chat message may inline `@url:<url>` and
// `@file:"path"` handles; the server expands them (concurrently) into the turn
// context BEFORE the run — @url via the SSRF-guarded host-side fetcher, @file
// read from the conversation workspace and path-gated by SafeWorkspaceJoin. This
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
	if !s.cfg.ContextHandlesEnabled {
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
		kind string // "url" | "file"
		arg  string
	}
	var matches []handleMatch
	for _, m := range urlHandleRe.FindAllStringSubmatch(message, -1) {
		matches = append(matches, handleMatch{kind: "url", arg: m[1]})
	}
	for _, m := range fileHandleRe.FindAllStringSubmatch(message, -1) {
		matches = append(matches, handleMatch{kind: "file", arg: m[1]})
	}
	if len(matches) == 0 {
		return nil, nil
	}
	dropped := 0
	if len(matches) > maxContextHandles {
		dropped = len(matches) - maxContextHandles
		matches = matches[:maxContextHandles]
	}

	ctx, cancel := context.WithTimeout(ctx, contextExpandTimeout)
	defer cancel()

	results := make([]expandedHandle, len(matches))
	var wg sync.WaitGroup
	for i, mt := range matches {
		wg.Add(1)
		go func(i int, mt handleMatch) {
			defer wg.Done()
			eh := expandedHandle{order: i}
			switch mt.kind {
			case "url":
				text, err := tools.FetchURLForContext(ctx, mt.arg)
				if err != nil {
					eh.notice = fmt.Sprintf("could not fetch @url:%s", mt.arg)
				} else {
					eh.block = fmt.Sprintf("**Fetched @url:%s**\n\n%s", mt.arg, text)
				}
			case "file":
				eh.block, eh.notice = expandFileHandle(workspaceRoot, mt.arg)
			}
			results[i] = eh
		}(i, mt)
	}
	wg.Wait()

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

// expandFileHandle reads relPath from the conversation workspace, path-gated by
// SafeWorkspaceJoin (rejects absolute paths, "..", and symlink escapes) and
// size-capped. Returns (block, notice); exactly one is non-empty.
func expandFileHandle(workspaceRoot, relPath string) (block, notice string) {
	if strings.TrimSpace(workspaceRoot) == "" {
		return "", fmt.Sprintf("@file is unavailable for this conversation: %s", relPath)
	}
	// Syntactic gate first (abs / ".." / NUL), so a legitimately MISSING file reads
	// as "not found" rather than SafeWorkspaceJoin's existence-dependent symlink
	// resolution error. After this gate a naive join stays within the workspace.
	if filepath.IsAbs(relPath) || strings.ContainsRune(relPath, 0) || hasDotDotComponent(relPath) {
		return "", fmt.Sprintf("@file path not allowed: %s", relPath)
	}
	// relPath is syntactically gated (no abs / ".." / NUL) above, so this join stays
	// under workspaceRoot; it is only an existence probe (SafeWorkspaceJoin does the
	// authoritative symlink-safe resolution below).
	if info, statErr := os.Stat(filepath.Join(workspaceRoot, relPath)); statErr != nil || !info.Mode().IsRegular() {
		return "", fmt.Sprintf("@file not found: %s", relPath)
	}
	// Existence + syntactic safety confirmed → resolve symlinks safely (catches a
	// symlink inside the workspace that escapes it).
	full, err := tools.SafeWorkspaceJoin(workspaceRoot, relPath)
	if err != nil {
		return "", fmt.Sprintf("@file path not allowed: %s", relPath)
	}
	f, err := os.Open(full) //nolint:gosec // full is validated by tools.SafeWorkspaceJoin to live under the conversation workspace root
	if err != nil {
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
	block = fmt.Sprintf("**Contents of @file:\"%s\":**\n\n```\n%s\n```", relPath, content)
	if truncated {
		block += "\n_(truncated)_"
	}
	return block, ""
}

// hasDotDotComponent reports whether relPath contains a ".." path component
// (mirrors SafeWorkspaceJoin's syntactic reject).
func hasDotDotComponent(relPath string) bool {
	for _, c := range strings.Split(filepath.ToSlash(relPath), "/") {
		if c == ".." {
			return true
		}
	}
	return false
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
