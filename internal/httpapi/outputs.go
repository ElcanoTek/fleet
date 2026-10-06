// What a chat's OUTPUTS are (ADR-0079): the workspace files the agent
// presented in its replies — the file chips the chat renders.
//
// Two halves, both host-side:
//
//   - presentedWorkspacePaths reads the assistant's TEXT replies and collects
//     every markdown link / image destination that the web UI would rewrite
//     into a workspace download. The rule is a port of
//     web/src/app/chat/ui/workspaceHref.ts (resolveScopedWorkspaceHref) so
//     "what the owner sees as a chip" and "what a teammate may download" can
//     never disagree: the same sandbox-prefix stripping, the same absolute-URL
//     bailout, the same `.`/`..` reject (encoded forms included), the same
//     per-segment percent-decoding. Code spans and fenced blocks are skipped
//     exactly as the TS redactor skips them — a path quoted in code is not a
//     chip.
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
	"io/fs"
	"net/url"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"
	"syscall"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/store"
	"github.com/ElcanoTek/fleet/internal/tools"
)

// uploadsDir is the workspace subdirectory uploads are staged into
// (stageAttachmentsIntoWorkspace). Nothing under it is ever an output.
const uploadsDir = "attachments"

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

// Markdown shapes, ported from workspaceHref.ts so the two parsers agree on
// what a link is. MD_DEST: an angle-bracketed destination or a bare run that
// may carry balanced parens and backslash escapes; MD_TITLE: an optional
// title.
const (
	mdDest  = `(?:<([^<>\n]*)>|((?:[^\s()\\]|\\.|\([^\s()]*\))+))?`
	mdTitle = `(?:\s+(?:"[^"]*"|'[^']*'|\([^()]*\)))?`
)

var (
	mdImage    = regexp.MustCompile(`!\[([^\]]*)\]\(\s*` + mdDest + mdTitle + `\s*\)`)
	mdLink     = regexp.MustCompile(`(!?)\[([^\]]*)\]\(\s*` + mdDest + mdTitle + `\s*\)`)
	mdRefUse   = regexp.MustCompile(`(!?)\[([^\]]*)\](?:\[([^\]]*)\])?`)
	mdRefDef   = regexp.MustCompile(`^\s{0,3}\[([^\]]+)\]:\s*(?:<([^<>\n]*)>|(\S+))`)
	codeFence  = regexp.MustCompile("^\\s{0,3}(`{3,}|~{3,})")
	inlineCode = regexp.MustCompile("`+[^`]*`+")
	mdEscape   = regexp.MustCompile(`\\([!-/:-@\[-` + "`" + `{-~])`)
)

// unescapeDest drops CommonMark backslash escapes (any ASCII punctuation)
// from a link destination — bare or angle-bracketed, inline or in a reference
// definition — as the markdown renderer does before the href reaches
// resolveWorkspaceHref. workspaceHref.ts (unescapeMarkdownDest) applies the
// same rule, so `[r](my\_file.csv)` is one path on both sides.
func unescapeDest(s string) string { return mdEscape.ReplaceAllString(s, "$1") }

func normalizeRefLabel(label string) string {
	return strings.ToLower(strings.Join(strings.Fields(label), " "))
}

// scanFencedLines tags each line with whether it sits inside (or is) a fenced
// code block, with CommonMark's closing rule: same character, at least as
// long as the opener. A port of workspaceHref.ts scanFenced.
func scanFencedLines(lines []string) []bool {
	code := make([]bool, len(lines))
	fence := ""
	for i, line := range lines {
		m := codeFence.FindStringSubmatch(line)
		if m == nil {
			code[i] = fence != ""
			continue
		}
		marker := m[1]
		code[i] = true
		switch {
		case fence == "":
			fence = marker
		case marker[0] == fence[0] && len(marker) >= len(fence):
			fence = ""
		}
	}
	return code
}

// outsideInlineCode returns line with every inline code span blanked, so the
// link passes see prose only.
func outsideInlineCode(line string) string {
	return inlineCode.ReplaceAllStringFunc(line, func(m string) string {
		return strings.Repeat(" ", len(m))
	})
}

// presentedWorkspacePaths returns every workspace-relative path the
// assistant's text replies link or embed, in first-seen order, deduplicated.
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
	for _, e := range history {
		if e.Role != "assistant" || e.Type != "text" {
			continue
		}
		var tc agent.TextContent
		if err := json.Unmarshal(e.Content, &tc); err != nil || tc.Text == "" {
			continue
		}
		for _, d := range markdownDestinations(tc.Text) {
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
	seen := map[string]bool{}
	for i := len(history) - 1; i >= 0; i-- {
		e := history[i]
		if e.Role != "assistant" || e.Type != "text" {
			continue
		}
		var tc agent.TextContent
		if err := json.Unmarshal(e.Content, &tc); err != nil || tc.Text == "" {
			continue
		}
		for _, d := range markdownDestinations(tc.Text) {
			p, ok := resolveWorkspaceRelPath(d)
			if !ok || seen[p] || isUploadPath(p) {
				continue
			}
			if len(paths) >= limit {
				return paths, true
			}
			seen[p] = true
			paths = append(paths, p)
		}
	}
	return paths, false
}

// markdownDestinations extracts the destinations of every inline link, inline
// image and USED reference-style link/image in one markdown document,
// skipping fenced blocks and inline code.
func markdownDestinations(markdown string) []string {
	lines := strings.Split(markdown, "\n")
	code := scanFencedLines(lines)

	defs := map[string]string{}
	defLine := map[int]bool{}
	for i, line := range lines {
		if code[i] {
			continue
		}
		m := mdRefDef.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		dest := m[2]
		if dest == "" {
			dest = m[3]
		}
		dest = unescapeDest(dest)
		key := normalizeRefLabel(m[1])
		if _, dup := defs[key]; !dup { // CommonMark: the first definition wins
			defs[key] = dest
		}
		defLine[i] = true
	}

	var out []string
	pick := func(angled, bare string) string {
		if angled != "" {
			return unescapeDest(angled)
		}
		return unescapeDest(bare)
	}
	for i, line := range lines {
		if code[i] || defLine[i] {
			continue
		}
		prose := outsideInlineCode(line)
		// Images first, blanked once read, so an image nested inside a link
		// label (`[![alt](chart.png)](chart.png)`) cannot hide the outer link.
		prose = mdImage.ReplaceAllStringFunc(prose, func(m string) string {
			sm := mdImage.FindStringSubmatch(m)
			out = append(out, pick(sm[2], sm[3]))
			return strings.Repeat(" ", len(m))
		})
		prose = mdLink.ReplaceAllStringFunc(prose, func(m string) string {
			sm := mdLink.FindStringSubmatch(m)
			out = append(out, pick(sm[3], sm[4]))
			return strings.Repeat(" ", len(m))
		})
		if len(defs) == 0 {
			continue
		}
		for _, loc := range mdRefUse.FindAllStringSubmatchIndex(prose, -1) {
			// `[x](…)` is an inline link the passes above already took; RE2
			// has no lookahead, so the `(?!\()` of the TS pattern is this.
			if loc[1] < len(prose) && prose[loc[1]] == '(' {
				continue
			}
			label := prose[loc[4]:loc[5]]
			ref := ""
			if loc[6] >= 0 {
				ref = prose[loc[6]:loc[7]]
			}
			key := label
			if strings.TrimSpace(ref) != "" {
				key = ref
			}
			if dest, ok := defs[normalizeRefLabel(key)]; ok {
				out = append(out, dest)
			}
		}
	}
	return out
}

// outputFile is one output on the wire (GET /conversations/{id}/outputs, the
// team view's `files`). Shared means "not excluded by the owner" — it is
// independent of whether the chat itself is currently shared.
type outputFile struct {
	Path       string `json:"path"`
	Name       string `json:"name"`
	Size       int64  `json:"size"`
	ModifiedAt int64  `json:"modified_at"`
	Shared     bool   `json:"shared"`
}

// isUploadPath reports whether rel lives under the uploads dir.
func isUploadPath(rel string) bool {
	return rel == uploadsDir || strings.HasPrefix(rel, uploadsDir+"/")
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
	f, err := cur.OpenFile(leaf, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
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
