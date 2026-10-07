// What a chat's OUTPUTS are (ADR-0079): the workspace files the agent
// presented in its replies — the file chips the chat renders.
//
// Two halves, both host-side:
//
//   - presentedWorkspacePaths reads the assistant's TEXT replies, grouped and
//     normalized exactly as the chat renders them, parses each with a
//     CommonMark + GFM parser (markdownDestinations), and collects the href of
//     every link / image the chat RENDERS that the web UI would rewrite into a
//     workspace download. Link-shaped text that renders as no link — inside a
//     code span, a fenced or indented code block, an HTML comment or raw HTML,
//     an image's alt text, or behind a backslash-escaped `[` — is not a chip
//     and never an output. The href rule is a port of
//     web/src/app/chat/ui/workspaceHref.ts (resolveScopedWorkspaceHref) so
//     "what the owner sees as a chip" and "what a teammate may download" can
//     never disagree: the same sandbox-prefix stripping, the same absolute-URL
//     bailout, the same `.`/`..` reject (encoded forms included), the same
//     per-segment percent-decoding.
//   - conversationOutputs keeps only presented paths that exist RIGHT NOW as
//     regular files in the conversation's workspace, reached without
//     traversing a symlink, and not under attachments/ (uploads are never
//     outputs).
//
// A file the agent wrote but never linked is not an output: it is never
// shared or counted, and stays a download-only entry in its owner's Sources.

package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/store"
	"github.com/ElcanoTek/fleet/internal/tools"
)

// uploadsDir is the workspace subdirectory uploads are staged into
// (stageAttachmentsIntoWorkspace). Nothing under it is ever an output.
const uploadsDir = "attachments"

// privateWorkspaceDirs are the workspace subdirectories fleet itself writes
// for the OWNER alone, which therefore can never be outputs, whatever a reply
// links: never shared or counted, never copied into a teammate's branch,
// refused by the team-files gate, and skipped by the Sources walk.
//
//   - uploadsDir: the owner's uploads.
//   - userSkillsRoot: the owner's PRIVATE skills, materialized as
//     user-skills/<name>/SKILL.md into every workspace of theirs
//     (materializeUserSkills) — a reply that links one must not hand it to
//     the team.
//
// The other fleet-made entries in a chat workspace — the bundle doc
// symlinks EnsureWorkspaceDir seeds (protocols, personas, system_prompts,
// skills, shared) — are symlinks, which openWorkspaceFileNoFollow refuses
// and the Sources walk does not follow, so they need no entry here.
var privateWorkspaceDirs = []string{uploadsDir, userSkillsRoot}

var (
	sandboxSchemePrefix = regexp.MustCompile(`(?i)^sandbox:/*`)
	sandboxWorkspaceDir = regexp.MustCompile(`(?i)^/?opt/chat/workspace/(?:[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/)?`)
	uriScheme           = regexp.MustCompile(`(?i)^[a-z][a-z0-9+.-]*:`)
)

// decodeSegmentSafe is JS decodeURIComponent with its fallback: an invalid
// escape leaves the segment as written.
func decodeSegmentSafe(seg string) string {
	out, err := url.PathUnescape(seg)
	if err != nil {
		return seg
	}
	return out
}

// fullyDecodeSegment decodes until stable (bounded), so `%252e%252e` cannot
// slip a `..` past a single decode — workspaceHref.ts's fullyDecodeSegment.
func fullyDecodeSegment(seg string) string {
	cur := seg
	for range 5 {
		next := decodeSegmentSafe(cur)
		if next == cur {
			return cur
		}
		cur = next
	}
	return cur
}

// resolveWorkspaceRelPath is the Go port of resolveScopedWorkspaceHref: it
// returns the workspace-relative path a markdown destination names, and false
// for everything the web UI would NOT rewrite into a workspace download
// (absolute URLs, `//`, site-root paths, anchors, queries, traversal).
//
// The returned path is the DECODED form, segments joined by "/", and it
// additionally passes store.ValidOutputPath — a decoded segment that itself
// contains a `/..` (from `%2F..%2F`) is refused here even though the web UI
// would merely re-encode it.
func resolveWorkspaceRelPath(raw string) (string, bool) {
	if raw == "" {
		return "", false
	}
	normalized := sandboxSchemePrefix.ReplaceAllString(raw, "")
	normalized = sandboxWorkspaceDir.ReplaceAllString(normalized, "")
	if uriScheme.MatchString(normalized) ||
		strings.HasPrefix(normalized, "//") ||
		strings.HasPrefix(normalized, "/") ||
		strings.HasPrefix(normalized, "#") ||
		strings.HasPrefix(normalized, "?") {
		return "", false
	}
	var segs []string
	for _, s := range strings.Split(normalized, "/") {
		if s != "" {
			segs = append(segs, s)
		}
	}
	if len(segs) == 0 {
		return "", false
	}
	for _, s := range segs {
		if d := fullyDecodeSegment(s); d == "." || d == ".." {
			return "", false
		}
	}
	decoded := make([]string, len(segs))
	for i, s := range segs {
		decoded[i] = decodeSegmentSafe(s)
	}
	rel := strings.Join(decoded, "/")
	if !store.ValidOutputPath(rel) {
		return "", false
	}
	return rel, true
}

// markdownParser is the CommonMark + GFM parser discovery reads replies
// with — the same dialect the chat renders them in (react-markdown with
// remark-gfm). A real parser, not a set of link-shaped regexes, because the
// question is "what did the owner SEE as a link", and only a parser answers
// it: a backslash-escaped `\[x](a.csv)`, an HTML comment, an indented or
// fenced code block, a code span, raw HTML and an image's alt text all CONTAIN
// link-shaped text that renders as no link at all. A regex that matched any of
// them would turn a file the owner was never shown as a chip into a shared
// output. Footnotes are on because remark-gfm renders links inside them.
var markdownParser = goldmark.New(goldmark.WithExtensions(extension.GFM, extension.Footnote)).Parser()

var (
	// AssistantContent.tsx normalizes the reply before rendering it; the two
	// rewrites that can change what parses as a link are mirrored here. The
	// first drops a dangling `**` that opens a line; the second turns a
	// "Label: <code span>" line into a bold label and a PLAIN value — which
	// un-codes the value, so a link written inside that span renders as one.
	mdDanglingBold = regexp.MustCompile(`(?m)^\*\*([^*\n:]+)$`)
	mdLabelCode    = regexp.MustCompile("(?m)^([A-Za-z][A-Za-z /]+):\\s*`([^`]+)`")
	htmlDocStart   = regexp.MustCompile(`(?i)<!DOCTYPE\s+html|<html[\s>]`)
	htmlDocLine    = regexp.MustCompile(`(?i)^\s*(<!DOCTYPE\s+html|<html[\s>])`)
	htmlDocEnd     = regexp.MustCompile(`(?i)</html>\s*$`)
	fenceLine      = regexp.MustCompile("^\\s*```")
)

// autoFenceRawHTMLDocument is AssistantContent.tsx autoFenceRawHtmlDocument:
// an unfenced `<!DOCTYPE html>…</html>` document is wrapped in an ```html
// fence before rendering, so nothing inside it renders as markdown.
func autoFenceRawHTMLDocument(content string) string {
	if !htmlDocStart.MatchString(content) {
		return content
	}
	lines := strings.Split(content, "\n")
	out := make([]string, 0, len(lines)+2)
	inFence, inHTML := false, false
	for _, line := range lines {
		if fenceLine.MatchString(line) {
			if inHTML {
				out = append(out, "```")
				inHTML = false
			}
			inFence = !inFence
			out = append(out, line)
			continue
		}
		if inFence {
			out = append(out, line)
			continue
		}
		if !inHTML && htmlDocLine.MatchString(line) {
			out = append(out, "```html", line)
			inHTML = true
			continue
		}
		out = append(out, line)
		if inHTML && htmlDocEnd.MatchString(line) {
			out = append(out, "```")
			inHTML = false
		}
	}
	if inHTML {
		out = append(out, "```")
	}
	return strings.Join(out, "\n")
}

// normalizeLikeRenderer applies AssistantContent.tsx's pre-render rewrites.
func normalizeLikeRenderer(content string) string {
	content = mdDanglingBold.ReplaceAllString(content, "$1")
	content = mdLabelCode.ReplaceAllString(content, "**$1:** $2")
	return autoFenceRawHTMLDocument(content)
}

// markdownDestinations returns the href of every link and image the chat
// RENDERS for one message's markdown, in document order: inline and
// reference-style links and images (a reference resolves to its FIRST
// definition), and URL autolinks. Each href is what the renderer puts on the
// element — backslash escapes and entity references resolved, then
// percent-encoded — so resolveWorkspaceRelPath sees exactly what the web's
// resolveWorkspaceHref does. Nothing inside an image's alt text counts (it
// renders as plain text), and email autolinks are mailto: links, never files.
func markdownDestinations(markdown string) []string {
	src := []byte(normalizeLikeRenderer(markdown))
	doc := markdownParser.Parse(text.NewReader(src))
	var out []string
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch v := n.(type) {
		case *ast.Image:
			out = append(out, string(util.URLEscape(v.Destination, true)))
			return ast.WalkSkipChildren, nil
		case *ast.Link:
			out = append(out, string(util.URLEscape(v.Destination, true)))
		case *ast.AutoLink:
			if v.AutoLinkType == ast.AutoLinkURL {
				out = append(out, string(util.URLEscape(v.URL(src), false)))
			}
		}
		return ast.WalkContinue, nil
	})
	return out
}

// Discovery's visit budget. The 500-path bound (maxOutputReferences) caps
// what discovery KEEPS, but on its own it does not cap what it READS: a long
// chat with no links at all would still have every reply decoded and parsed
// on each outputs listing, branch copy, team-files download and team-view
// poll. So the newest-first walk also stops after this many rendered replies
// or this many bytes of reply payload, whichever comes first, and reports the
// cut through the same truncated flag the path bound uses.
const (
	maxDiscoveryReplies = 2000
	maxDiscoveryBytes   = 4 << 20 // 4 MiB of stored reply text
)

// isRenderedMessageBoundary reports whether e ends the assistant message the
// chat is rendering: a user turn, a compaction summary, or the content-free
// boundary that stands in for a summary in a teammate's view and branch.
func isRenderedMessageBoundary(e agent.HistoryEntry) bool {
	return e.Type == "summary" || e.Type == agent.EntryTypeSummaryBoundary ||
		(e.Role == "user" && e.Type == "text")
}

// recentRenderedReplies groups the assistant's text entries into the messages
// the chat renders (web history.ts historyToMessages): consecutive assistant
// text entries are ONE message, concatenated as-is, until a user text entry or
// a compaction summary (or its boundary) starts a new one. Discovery must
// parse the same units the owner saw — a fence opened in one entry and closed
// in the next is one code block on screen, and a link inside it is not a chip.
//
// Replies come back NEWEST FIRST, and the walk is bounded by maxReplies and
// maxBytes (see maxDiscoveryReplies); truncated reports that older replies
// were not visited. Only WHOLE replies are returned: a reply the byte budget
// would cut is dropped entirely, because its tail parsed without its head is
// a different Markdown document (a fence opened earlier would not be open),
// and the gate must only ever narrow — never see a link the owner did not.
func recentRenderedReplies(history []agent.HistoryEntry, maxReplies, maxBytes int) (replies []string, truncated bool) {
	var parts []string // the open reply's entries, newest first
	open := false
	used := 0
	flush := func() {
		if open {
			var b strings.Builder
			for i := len(parts) - 1; i >= 0; i-- {
				b.WriteString(parts[i])
			}
			if b.Len() > 0 {
				replies = append(replies, b.String())
			}
		}
		parts = parts[:0]
		open = false
	}
	for i := len(history) - 1; i >= 0; i-- {
		e := history[i]
		switch {
		case isRenderedMessageBoundary(e):
			flush()
		case e.Role == "assistant" && e.Type == "text":
			if !open && len(replies) >= maxReplies {
				return replies, true
			}
			used += len(e.Content)
			if used > maxBytes {
				// Drop the partial reply (see above) and stop.
				return replies, true
			}
			var tc agent.TextContent
			if err := json.Unmarshal(e.Content, &tc); err == nil {
				parts = append(parts, tc.Text)
			}
			open = true
		}
	}
	flush()
	return replies, false
}

// discoveryCutoff decides, row by row and newest first, when
// recentRenderedReplies would stop reading a history — the same reply and
// byte budgets, the same accounting. It is what lets the store read only as
// much history as discovery will visit (LoadDiscoveryHistory): more reports
// whether the walk still wants rows OLDER than e. The row on which it answers
// false is kept, so the walk itself sees the budget being crossed and reports
// truncated exactly as it would over the full history.
type discoveryCutoff struct {
	maxReplies, maxBytes int
	replies, used        int
	open, nonEmpty       bool
}

func (c *discoveryCutoff) more(e agent.HistoryEntry) bool {
	switch {
	case isRenderedMessageBoundary(e):
		// recentRenderedReplies' flush: an open reply counts once it has text.
		if c.open && c.nonEmpty {
			c.replies++
		}
		c.open, c.nonEmpty = false, false
	case e.Role == "assistant" && e.Type == "text":
		if !c.open && c.replies >= c.maxReplies {
			return false
		}
		c.used += len(e.Content)
		if c.used > c.maxBytes {
			return false
		}
		var tc agent.TextContent
		if err := json.Unmarshal(e.Content, &tc); err == nil && tc.Text != "" {
			c.nonEmpty = true
		}
		c.open = true
	}
	return true
}

// presentedWorkspacePaths returns every workspace-relative path the
// assistant's text replies link or embed (within the discovery visit budget),
// in first-seen order, deduplicated.
// Only role=assistant, type=text entries count: a path in a user message, a
// tool result or reasoning is not a file chip.
func presentedWorkspacePaths(history []agent.HistoryEntry) []string {
	seen := map[string]bool{}
	var out []string
	add := func(dest string) {
		if p, ok := resolveWorkspaceRelPath(dest); ok && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	replies, _ := recentRenderedReplies(history, maxDiscoveryReplies, maxDiscoveryBytes)
	for i := len(replies) - 1; i >= 0; i-- {
		for _, d := range markdownDestinations(replies[i]) {
			add(d)
		}
	}
	return out
}

// maxOutputReferences bounds how many distinct presented paths output
// discovery considers per conversation. Discovery opens and stats every path
// it considers, and it runs on hot paths — the outputs listing, Sources, the
// branch copy, the team-files download gate and the team view's 12 s poll —
// so an unbounded transcript would turn each of those into thousands of
// syscalls. The bound keeps the MOST RECENT references (by message order):
// the files a long chat is still working with are the ones a reader wants.
const maxOutputReferences = 500

// recentPresentedPaths is presentedWorkspacePaths, bounded: walking the
// assistant's text replies newest message first, it keeps at most limit
// distinct non-upload paths and reports truncated when an older reference
// was dropped. Uploads are skipped before they count — they are never
// outputs, so they must not crowd real ones out of the bound.
func recentPresentedPaths(history []agent.HistoryEntry, limit int) (paths []string, truncated bool) {
	return boundedPresentedPaths(history, limit, false)
}

// boundedPresentedPaths is the walk behind recentPresentedPaths; with
// includeUploads it also keeps upload references (a teammate branch records
// those as withheld — they are references it does not have a copy of).
func boundedPresentedPaths(history []agent.HistoryEntry, limit int, includeUploads bool) (paths []string, truncated bool) {
	seen := map[string]bool{}
	replies, budgetCut := recentRenderedReplies(history, maxDiscoveryReplies, maxDiscoveryBytes)
	for _, reply := range replies {
		// Newest first within a reply too: a long agentic turn renders as
		// ONE message, and its last references are its most recent.
		dests := markdownDestinations(reply)
		for j := len(dests) - 1; j >= 0; j-- {
			d := dests[j]
			p, ok := resolveWorkspaceRelPath(d)
			if !ok || seen[p] || (!includeUploads && isPrivateWorkspacePath(p)) {
				continue
			}
			if len(paths) >= limit {
				return paths, true
			}
			seen[p] = true
			paths = append(paths, p)
		}
	}
	return paths, budgetCut
}

// outputFile is one output on the wire (GET /conversations/{id}/outputs, the
// team view's `files`). Shared means "not excluded by the owner" — it is
// independent of whether the chat itself is currently shared.
type outputFile struct {
	Path       string `json:"path"`
	Name       string `json:"name"`
	Size       int64  `json:"size"`
	ModifiedAt int64  `json:"modified_at"`
	// Rev identifies this version of the file's bytes as far as a stat can:
	// the modification time in nanoseconds and the size. The team viewer puts
	// it in each file URL so an overwrite at the same path is fetched again;
	// modified_at alone is whole seconds and misses a same-second rewrite.
	Rev    string `json:"rev,omitempty"`
	Shared bool   `json:"shared"`
}

// isPrivateWorkspacePath reports whether rel lives under one of the owner-private
// workspace dirs (privateWorkspaceDirs) — uploads, and the owner's
// materialized private skills. Nothing there is ever an output.
func isPrivateWorkspacePath(rel string) bool {
	for _, d := range privateWorkspaceDirs {
		if rel == d || strings.HasPrefix(rel, d+"/") {
			return true
		}
	}
	return false
}

// errNotAWorkspaceFile covers every way a path fails to name a regular file
// reachable without a symlink: missing, a directory, a symlink anywhere along
// it, or swapped out from under the check.
var errNotAWorkspaceFile = errors.New("not a regular workspace file")

// openWorkspaceFileNoFollow opens rel inside wsDir for reading, refusing a
// symlink at ANY component — not just a symlink that escapes the workspace.
//
// os.Root alone confines resolution to the workspace, but it follows symlinks
// that stay inside it. For a file served to SOMEONE OTHER than the owner that
// is not enough: the owner's sandbox can write into this tree, so a shared
// `out/report.csv` swapped for a symlink to `attachments/<upload>` (or to an
// unchecked output) would hand a teammate exactly the bytes the owner held
// back, without escaping anything. So each directory is Lstat'ed (must be a
// real directory), opened, and proven to be the SAME directory that was
// Lstat'ed; the leaf is Lstat'ed (must be a regular file), opened with
// O_NOFOLLOW, and proven to be that same file. A swap between check and open
// fails the identity check rather than redirecting the read.
func openWorkspaceFileNoFollow(wsDir, rel string) (*os.File, fs.FileInfo, error) {
	if !store.ValidOutputPath(rel) {
		return nil, nil, errNotAWorkspaceFile
	}
	root, err := os.OpenRoot(wsDir)
	if err != nil {
		return nil, nil, err
	}
	roots := []*os.Root{root}
	defer func() {
		for _, r := range roots {
			_ = r.Close()
		}
	}()
	segs := strings.Split(rel, "/")
	cur := root
	for _, dir := range segs[:len(segs)-1] {
		want, err := cur.Lstat(dir)
		if err != nil {
			return nil, nil, err
		}
		if !want.IsDir() { // a symlink's Lstat mode is ModeSymlink, never a dir
			return nil, nil, errNotAWorkspaceFile
		}
		next, err := cur.OpenRoot(dir)
		if err != nil {
			return nil, nil, err
		}
		roots = append(roots, next)
		got, err := next.Stat(".")
		if err != nil {
			return nil, nil, err
		}
		if !os.SameFile(want, got) {
			return nil, nil, errNotAWorkspaceFile
		}
		cur = next
	}
	leaf := segs[len(segs)-1]
	want, err := cur.Lstat(leaf)
	if err != nil {
		return nil, nil, err
	}
	if !want.Mode().IsRegular() {
		return nil, nil, errNotAWorkspaceFile
	}
	if workspaceOpenAfterLstat != nil {
		workspaceOpenAfterLstat(filepath.Join(wsDir, filepath.FromSlash(rel)))
	}
	// O_NONBLOCK: the leaf was a regular file when Lstat'ed, but the owner's
	// sandbox can swap it for a FIFO before the open, and a blocking open of
	// a FIFO with no writer never returns — a request (or a branch copy)
	// hung for good. Non-blocking, the open returns at once and the SameFile
	// + IsRegular check below refuses it. For a regular file O_NONBLOCK is a
	// no-op on Linux: reads behave exactly as before.
	f, err := cur.OpenFile(leaf, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, err
	}
	got, err := f.Stat()
	if err != nil || !got.Mode().IsRegular() || !os.SameFile(want, got) {
		_ = f.Close()
		return nil, nil, errNotAWorkspaceFile
	}
	return f, got, nil
}

// workspaceOpenAfterLstat is a test seam run between the leaf's Lstat and
// its open in openWorkspaceFileNoFollow; nil in production.
var workspaceOpenAfterLstat func(fullPath string)

// statWorkspaceFileNoFollow is openWorkspaceFileNoFollow for a listing: the
// same no-symlink rule, the descriptor closed straight away.
func statWorkspaceFileNoFollow(wsDir, rel string) (fs.FileInfo, bool) {
	f, info, err := openWorkspaceFileNoFollow(wsDir, rel)
	if err != nil {
		return nil, false
	}
	_ = f.Close()
	return info, true
}

// conversationOutputs resolves a conversation's presented paths against its
// workspace on disk and applies the owner's exclusions. Newest first
// (modified_at desc, then path for a stable order).
//
// Only the maxOutputReferences most recent distinct references are
// considered; truncated reports that older ones were not. A path beyond the
// bound is not an output for any caller — not listed, not copied into a
// branch, and refused by the team-files gate — so the gate stays an exact
// match against this (bounded) list and never widens.
func conversationOutputs(convID string, history []agent.HistoryEntry, excluded map[string]bool) (out []outputFile, truncated bool) {
	out = []outputFile{}
	presented, truncated := recentPresentedPaths(history, maxOutputReferences)
	if len(presented) == 0 {
		return out, truncated
	}
	wsDir := tools.WorkspaceDirForConversation(convID)
	for _, rel := range presented {
		info, ok := statWorkspaceFileNoFollow(wsDir, rel)
		if !ok {
			continue
		}
		out = append(out, outputFile{
			Path:       rel,
			Name:       path.Base(rel),
			Size:       info.Size(),
			ModifiedAt: info.ModTime().Unix(),
			Rev:        fmt.Sprintf("%d-%d", info.ModTime().UnixNano(), info.Size()),
			Shared:     !excluded[rel],
		})
	}
	sortOutputsNewestFirst(out)
	return out, truncated
}

func sortOutputsNewestFirst(out []outputFile) {
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].ModifiedAt != out[j].ModifiedAt {
			return out[i].ModifiedAt > out[j].ModifiedAt
		}
		return out[i].Path < out[j].Path
	})
}

// countShared is the number of outputs not excluded by the owner.
func countShared(outs []outputFile) int {
	n := 0
	for _, o := range outs {
		if o.Shared {
			n++
		}
	}
	return n
}
