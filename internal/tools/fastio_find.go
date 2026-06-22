// Package tools — fastio_find.
//
// Discovery wrapper around fast.io's `storage` MCP tool. The remote
// `storage action=search` endpoint does AND-tokenized keyword matching
// against filename + AI-extracted summary text, which means natural-
// language phrasings like "ABC plumbing" miss the actual report files
// when the report's filename only carries the ELC code (e.g.
// `ABC_ELC00109_Overall_Report.csv`). fastio_find paves over that with:
//
//  1. ELC-code auto-promotion. If the query contains an `ELCxxxxx`
//     code, the search retries with just the code when the natural
//     query returns nothing — that's the canonical cross-filename
//     identifier our reporting workflow uses.
//  2. Lean response shape. The raw `storage search` response carries
//     ~600 bytes of `_buildHash`/`_next` boilerplate per call plus a
//     verbose markdown-list-of-bullets per file. fastio_find returns
//     a single tight markdown table — id, name, parent, modified,
//     size, mimetype — sorted by modified DESC so the newest variant
//     of a same-name file is at the top.
//  3. One discovery turn, not nine. The bug we're fixing: a single
//     ABC-plumbing reporting turn fired 9 storage calls (search → list
//     → details → details → details → search again …) trying to find
//     the right file. fastio_find compresses that into search (terse,
//     for ids) + one bulk details call (≤25 nodes), and returns
//     everything the agent needs to pick the right node.
//
// Out of scope (use the raw `mcp_fast_io_storage` tool for these):
//   - move / rename / delete operations (destructive, must stay explicit)
//   - cursor pagination beyond the first ~25 hits (rare in practice;
//     bumping the search limit and re-running covers it)
//   - semantic / RAG-mode search (workspace intelligence is off by
//     default; turning it on costs credits per page)
package tools

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"charm.land/fantasy"
)

// emDashCell is the placeholder fast.io list/search columns use for
// empty cells (size, modified, mime, parent).
const emDashCell = "—"

// FastIOFindParams is the typed surface of fastio_find. Keep it
// minimal — the whole point of this wrapper is that the agent does
// not have to think about detail levels, pagination, or two-step
// search→details flows.
type FastIOFindParams struct {
	Query       string `json:"query" description:"Free-text search. Filenames, account/project codes, names, etc. all work — the tool auto-promotes account codes (ELCxxxxx-style) when the original query returns nothing, because such a code is often the canonical cross-file identifier in a workspace. Examples: \"ELC00109\", \"ABC plumbing ELC00109\", \"Overall Report\"."`
	WorkspaceID string `json:"workspace_id" description:"19-digit fast.io workspace id. Get it once per conversation via mcp_fast_io_workspace action=list (the first row's id) and reuse the same value for every find/upload/download call."`
	Limit       int    `json:"limit,omitempty" description:"Max results returned (1-25). Default 10. Higher values cost one bulk-details lookup, no extra round-trips. Use 25 to see every variant when triaging same-name duplicates."`
}

// fastIOFindDefaultLimit is the result cap when the caller omits Limit.
// Tuned to match fast.io's bulk-details cap of 25 ids per call — going
// above this would require a second details round-trip, which defeats
// the one-discovery-turn promise. 10 is plenty for the typical "pick
// the latest report" case and small enough that the response stays
// well under any context-bloat threshold.
const fastIOFindDefaultLimit = 10

// fastIOFindMaxLimit is the hard ceiling on Limit. Matches fast.io's
// bulk-details ≤25-ids rule so we never need to chunk client-side.
const fastIOFindMaxLimit = 25

// fastIOFindMaxFallbacks caps how many ELC-code fallback queries we'll
// run when the original returns zero. A user message could in theory
// list 5+ ELC codes ("compare ELC00109 and ELC00115 and …"); without
// a cap that turns into 5 round-trips on a workspace that doesn't
// have any of them. Two fallbacks covers every multi-client query
// we've actually seen in production and keeps the worst-case round-
// trip count predictable (1 original + 2 fallbacks + 1 details = 4
// max, vs the 9–12 calls the unwrapped flow burns).
const fastIOFindMaxFallbacks = 2

// elcCodeRe matches ELC-style account codes — ELC followed by 3–6
// digits, with word boundaries so we don't match e.g. "telco" or a
// substring of another token. Case-insensitive because the codes show
// up as `ELC00109`, `elc00109`, and `Elc00109` interchangeably in
// user messages and filenames. (A common account-code convention; the
// heuristic is harmless when a workspace doesn't use such codes.)
var elcCodeRe = regexp.MustCompile(`(?i)\bELC\d{3,6}\b`)

// Storage tool argument keys. Pulled out as constants so the dispatch
// payloads stay tidy and so a future refactor of fast.io's parameter
// names (the `context_type`/`context_id` aliases, mostly) is a one-
// place change. The shared MCP-tool name `storage` lives here too;
// every search/details call routes through the same handler.
const (
	fastIOStorageToolName    = "storage"
	fastIOArgAction          = "action"
	fastIOArgProfileType     = "profile_type"
	fastIOArgProfileID       = "profile_id"
	fastIOArgQuery           = "query"
	fastIOArgDetail          = "detail"
	fastIOArgDisplayLimit    = "display_limit"
	fastIOArgNodeIDs         = "node_ids"
	fastIOActionSearch       = "search"
	fastIOActionDetails      = "details"
	fastIODetailTerse        = "terse"
	fastIODetailStandard     = "standard"
	fastIOProfileTypeValue   = "workspace"
	fastIOEmptyBodyPlacehold = "(no body)"
)

const fastIOFindDescription = "Find files in fast.io by name, account code, or any keyword — efficiently and without bloating context. " +
	"This is the right tool for `find the latest report for X`, `which files match Y`, `is there an X file in fast.io`, and similar discovery questions. " +
	"It wraps `mcp_fast_io_storage action=search` with smarter query handling and a compact response format:\n\n" +
	"BEHAVIOR:\n" +
	"  - Auto-detects ELC-style account codes in the query (e.g. `ELC00109`) and unions an extra code-only search with the user's natural phrasing. " +
	"This matters when report files are named by account code (`ABC_ELC00109_Overall_Report.csv`), so a natural-language query like \"ABC plumbing\" would otherwise miss them.\n" +
	"  - Returns a single tight markdown table — id, name, parent, modified, size, mimetype — sorted newest-first. " +
	"Use the node id with `mcp_fast_io_download action=file-url` to fetch a result; use the parent id with `mcp_fast_io_storage action=list` to see siblings.\n\n" +
	"FILE-PICK POLICY (enforced by the response, not optional):\n" +
	"  - Exactly 1 match → proceed; use that id.\n" +
	"  - 2+ matches → STOP and ask the user which one. Do NOT pick on their behalf even when one row is clearly newest. " +
	"Same-name duplicates exist for real reasons (per-week copies, per-folder isolation, hash-suffixed dedup variants); the user is the only authority on which is canonical for their current task. " +
	"Quote the names and modified dates back and let them choose. Auto-picking from a duplicate-rich workspace has burned us in production — draft against the wrong file.\n\n" +
	"REQUIRED: `query` (free-text), `workspace_id` (19-digit, from `mcp_fast_io_workspace action=list`).\n" +
	"OPTIONAL: `limit` (1-25, default 10).\n\n" +
	"PREFER OVER `mcp_fast_io_storage action=search` for any file-discovery question — this tool burns fewer round-trips, returns a tighter response, handles the ELC-code edge case automatically, and enforces the file-pick policy. " +
	"Use the raw storage tool only for move / rename / delete / list-by-folder operations that fastio_find does not cover."

// NewFastIOFindTool returns the fastio_find native tool bound to the
// given MCP client. A nil caller surfaces a clear "fast.io not
// configured" error at invocation, same pattern as the upload tool.
func NewFastIOFindTool(caller MCPCaller) fantasy.AgentTool {
	return fantasy.NewAgentTool("fastio_find", fastIOFindDescription,
		func(ctx context.Context, params FastIOFindParams, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			payload, err := runFastIOFind(ctx, caller, params)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			return fantasy.NewTextResponse(payload), nil
		})
}

// runFastIOFind is the testable core: plan queries, fetch ids, hydrate
// to standard detail, sort + render. Pure with respect to the caller
// interface, so tests can substitute a recording fake.
func runFastIOFind(ctx context.Context, caller MCPCaller, params FastIOFindParams) (string, error) {
	if caller == nil {
		return "", fmt.Errorf("fastio_find is unavailable: FAST_IO_MCP_TOKEN is not configured on this server")
	}

	query := strings.TrimSpace(params.Query)
	if query == "" {
		return "", fmt.Errorf("query is required")
	}
	workspaceID := strings.TrimSpace(params.WorkspaceID)
	if workspaceID == "" {
		return "", fmt.Errorf("workspace_id is required — call `mcp_fast_io_workspace action=list` once per conversation and reuse the 19-digit numeric id")
	}

	limit := params.Limit
	if limit <= 0 {
		limit = fastIOFindDefaultLimit
	}
	if limit > fastIOFindMaxLimit {
		limit = fastIOFindMaxLimit
	}

	// Plan and execute search passes. Two strategies depending on the
	// query shape:
	//
	//   - Query carries an ELC code: run BOTH the natural pass and the
	//     ELC-only fallback and union their ids. Fast.io's AND-
	//     tokenized search returns at most one wrong file for a
	//     phrase like "ABC plumbing ELC00109" (it matches only the
	//     campaign-setup XLSX where every token appears in the
	//     filename/summary) while the ELC pass returns every actual
	//     report (named by ELC code alone). Running both means the
	//     agent sees the full set and the user's natural phrasing
	//     still surfaces the file that legitimately matched it.
	//
	//   - Query has no ELC code: run only the natural pass; no
	//     useful fallback exists.
	//
	// We cap total round-trips at 1 original + fastIOFindMaxFallbacks
	// regardless, so the worst-case fastio_find latency stays bounded.
	passes := planFastIOSearchPasses(query)
	var ids []string
	var winningQuery string
	attempts := make([]string, 0, len(passes))
	for _, q := range passes {
		attempts = append(attempts, q)
		got, err := fastIOSearchIDs(ctx, caller, workspaceID, q, limit)
		if err != nil {
			return "", err
		}
		if len(got) > 0 && winningQuery == "" {
			// First pass to return any hits "owns" the description
			// line ("via fallback X"). When the natural query already
			// matched, the original query wins — we just augment with
			// fallback hits. When only the fallback matched, the
			// fallback wins.
			winningQuery = q
		}
		ids = append(ids, got...)
	}
	// Surface fallback promotions in the server log so operators can
	// spot patterns ("we keep falling back to ELC codes — should the
	// natural query be tuned?"). Cheap; one line per find call at most.
	if len(attempts) > 1 && winningQuery != "" {
		log.Printf("fastio_find: query=%q attempts=%v winner=%q ids=%d (post-dedup)",
			query, attempts, winningQuery, len(ids))
	}

	// Dedup ids before bulk-details. fast.io's `_fetched_ids` is the
	// authoritative list and shouldn't have duplicates, but the
	// `# files` bullet fallback parser could in theory pick up a
	// repeat if a future server build changed the response shape.
	// Cheaper to defend here than to debug "Why are there two rows
	// for the same node" later.
	ids = dedupStrings(ids)
	if len(ids) == 0 {
		return renderFastIOFindNoHits(query, attempts), nil
	}
	// Cap to fastIOFindMaxLimit just in case the search ignored
	// display_limit (defensive — fast.io currently honors it, but
	// bulk-details rejects >25 ids client-side and there's no good
	// reason to make the agent recover from that).
	if len(ids) > fastIOFindMaxLimit {
		ids = ids[:fastIOFindMaxLimit]
	}

	// Phase 2: hydrate the ids with a single bulk-details call.
	rows, errs, err := fastIODetailsTable(ctx, caller, workspaceID, ids)
	if err != nil {
		return "", err
	}

	// Sort newest-first. Same-name duplicates across different parents
	// stay distinct; the agent picks based on the displayed modified
	// stamp. Rows that fast.io failed to hydrate (`errs`) get listed
	// at the bottom with what we know — name comes from the search
	// pass, the rest is `—`.
	sort.SliceStable(rows, func(i, j int) bool {
		return rows[i].Modified.After(rows[j].Modified)
	})

	return renderFastIOFindResults(query, winningQuery, attempts, rows, errs, limit), nil
}

// dedupStrings returns a copy of `in` with duplicates removed, order
// of first occurrence preserved.
func dedupStrings(in []string) []string {
	if len(in) <= 1 {
		return in
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// planFastIOSearchPasses returns the ordered list of search queries to
// try. The first pass is always the user's original query — anything
// else would silently change behavior they expect. Fallbacks only kick
// in when the original returns zero hits.
func planFastIOSearchPasses(query string) []string {
	passes := []string{query}
	codes := elcCodeRe.FindAllString(query, -1)
	// If the user's query already IS just the ELC code, no fallback is
	// useful — the second pass would be identical.
	if len(codes) == 1 && strings.EqualFold(strings.TrimSpace(query), codes[0]) {
		return passes
	}
	// Add each unique ELC code as its own fallback. Most queries carry
	// at most one, but a "compare ELC00109 vs ELC00115" prompt could
	// reasonably carry two — we honor both up to the fallback cap
	// rather than guessing which matters more.
	seen := map[string]bool{strings.ToUpper(query): true}
	fallbacks := 0
	for _, code := range codes {
		key := strings.ToUpper(code)
		if seen[key] {
			continue
		}
		seen[key] = true
		passes = append(passes, code)
		fallbacks++
		if fallbacks >= fastIOFindMaxFallbacks {
			break
		}
	}
	return passes
}

// fastIOSearchIDs runs a single storage.search call at detail=terse and
// returns the node ids found, capped to `limit`. We use terse because
// at the search endpoint, no higher detail level adds the fields we
// care about (modified, size, mimetype) anyway — those only come from
// the details endpoint — so terse is the cheapest legal mode.
func fastIOSearchIDs(ctx context.Context, caller MCPCaller, workspaceID, query string, limit int) ([]string, error) {
	args := map[string]interface{}{
		fastIOArgAction:       fastIOActionSearch,
		fastIOArgProfileType:  fastIOProfileTypeValue,
		fastIOArgProfileID:    workspaceID,
		fastIOArgQuery:        query,
		fastIOArgDetail:       fastIODetailTerse,
		fastIOArgDisplayLimit: limit,
	}
	result, err := caller.CallTool(ctx, fastIOStorageToolName, args)
	if err != nil {
		return nil, fmt.Errorf("fast.io search failed: %w", err)
	}
	if result == nil || result.IsError {
		text := joinMCPText(result)
		if text == "" {
			text = fastIOEmptyBodyPlacehold
		}
		return nil, fmt.Errorf("fast.io rejected the search: %s", text)
	}
	return parseFastIOSearchIDs(joinMCPText(result)), nil
}

// parseFastIOSearchIDs extracts node ids from a `storage action=search`
// markdown response. We look at the `_fetched_ids` block (the
// authoritative pre-trim id list, per the storage describe doc) and
// fall back to the `# files` bullets if `_fetched_ids` is absent (which
// it can be at high display_limit or on older server builds).
func parseFastIOSearchIDs(text string) []string {
	ids := parseFastIOFetchedIDs(text)
	if len(ids) > 0 {
		return ids
	}
	return parseFastIOFileBulletIDs(text)
}

func parseFastIOFetchedIDs(text string) []string {
	// `# _fetched_ids` is followed by `- <id>` lines until the next H1.
	const header = "# _fetched_ids"
	idx := strings.Index(text, header)
	if idx < 0 {
		return nil
	}
	rest := text[idx+len(header):]
	// Cut at the next H1 header.
	if end := indexOfNextH1(rest); end >= 0 {
		rest = rest[:end]
	}
	var out []string
	for _, line := range strings.Split(rest, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "- ") {
			continue
		}
		id := strings.TrimSpace(strings.TrimPrefix(line, "- "))
		if id != "" {
			out = append(out, id)
		}
	}
	return out
}

func parseFastIOFileBulletIDs(text string) []string {
	// `# files` block. Each hit starts with `- **<id>:**` on its own
	// line. We pull just the id token.
	const header = "# files"
	idx := strings.Index(text, header)
	if idx < 0 {
		return nil
	}
	rest := text[idx+len(header):]
	if end := indexOfNextH1(rest); end >= 0 {
		rest = rest[:end]
	}
	re := regexp.MustCompile(`(?m)^- \*\*([a-z0-9-]+):\*\*\s*$`)
	matches := re.FindAllStringSubmatch(rest, -1)
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, m[1])
	}
	return out
}

// indexOfNextH1 returns the byte index of the next `# ` (markdown H1)
// header in s, or -1 if none. Skips past the first byte of `s` so a
// header at offset 0 doesn't short-circuit (callers pass slices where
// they've already advanced past the section they care about).
func indexOfNextH1(s string) int {
	// Look for "\n# " — the leading newline anchors us to a real line
	// start instead of a substring like "<something># something".
	idx := strings.Index(s, "\n# ")
	if idx < 0 {
		return -1
	}
	return idx + 1 // include the newline; caller wants the cut to land before "# "
}

// fastIODetailsRow is one row from the bulk-details table. Only the
// fields fastio_find actually surfaces are decoded — adding more is
// cheap (one extra column lookup) but should be motivated by a real
// agent-side need, not "in case".
type fastIODetailsRow struct {
	ID       string
	Name     string
	Parent   string
	Modified time.Time
	Size     int64
	Mimetype string
	WebURL   string
}

// fastIODetailsTable runs one bulk `storage action=details` call at
// detail=standard and parses the markdown table into rows. Fast.io
// caps bulk details at 25 ids per call; the caller is responsible for
// keeping `ids` under that limit (fastIOFindMaxLimit enforces it).
//
// Returns (rows, perNodeErrors, fatal). `perNodeErrors` collects
// per-node error messages reported by fast.io's `# errors` block —
// these are surfaced in the rendered output so the agent can see
// "node X failed: not found" and decide whether to retry. `fatal` is
// only non-nil for transport / 400-class failures that prevent any
// rows from coming back.
func fastIODetailsTable(ctx context.Context, caller MCPCaller, workspaceID string, ids []string) ([]fastIODetailsRow, map[string]string, error) {
	if len(ids) == 0 {
		return nil, nil, nil
	}
	args := map[string]interface{}{
		fastIOArgAction:      fastIOActionDetails,
		fastIOArgProfileType: fastIOProfileTypeValue,
		fastIOArgProfileID:   workspaceID,
		fastIOArgNodeIDs:     ids,
		fastIOArgDetail:      fastIODetailStandard,
	}
	result, err := caller.CallTool(ctx, fastIOStorageToolName, args)
	if err != nil {
		return nil, nil, fmt.Errorf("fast.io bulk-details call failed: %w", err)
	}
	if result == nil || result.IsError {
		text := joinMCPText(result)
		if text == "" {
			text = fastIOEmptyBodyPlacehold
		}
		return nil, nil, fmt.Errorf("fast.io rejected the bulk-details: %s", text)
	}
	body := joinMCPText(result)
	return parseFastIODetailsTable(body), parseFastIODetailsErrors(body), nil
}

// parseFastIODetailsErrors picks the `# errors` block out of a bulk-
// details response. Fast.io's documented shape is a markdown table
// keyed by node_id; if no errors occurred the block reads `—`. We
// return a map so callers can pair errors with the original ids.
func parseFastIODetailsErrors(text string) map[string]string {
	const header = "# errors"
	idx := strings.Index(text, header)
	if idx < 0 {
		return nil
	}
	rest := text[idx+len(header):]
	if end := indexOfNextH1(rest); end >= 0 {
		rest = rest[:end]
	}
	rest = strings.TrimSpace(rest)
	if rest == "" || rest == emDashCell {
		return nil
	}
	// The errors block can be either a table (one row per failed id)
	// or a markdown list. We try the table path first since that's
	// the documented format.
	out := map[string]string{}
	lines := strings.Split(rest, "\n")
	for i, line := range lines {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "|") {
			continue
		}
		// Skip header + separator.
		if i+2 < len(lines) && strings.Contains(line, "id") && strings.Contains(line, "error") {
			continue
		}
		if strings.HasPrefix(line, "| ---") {
			continue
		}
		cells := splitMarkdownTableRow(line)
		if len(cells) < 2 {
			continue
		}
		id := strings.TrimSpace(cells[0])
		msg := strings.TrimSpace(cells[len(cells)-1])
		if id != "" && msg != "" {
			out[id] = msg
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// parseFastIODetailsTable extracts node rows from a bulk-details
// response. Fast.io's response shape varies by request size:
//
//   - 1 id: `# format single` + a `# node` bullet block (one node).
//   - 2+ ids: `# format multi` + a `# nodes` markdown table.
//
// We handle both. The single-format path is a separate parser because
// the bullet structure is `- **key:** value` rather than table cells,
// and pretending it's a 1-row table would be more code than dispatch.
// Returning the union of both means a request that somehow gets a
// mixed response (paranoia; fast.io doesn't do this today) doesn't
// silently drop one shape.
func parseFastIODetailsTable(text string) []fastIODetailsRow {
	var out []fastIODetailsRow
	if row, ok := parseFastIODetailsSingle(text); ok {
		out = append(out, row)
	}
	out = append(out, parseFastIODetailsMulti(text)...)
	return out
}

// parseFastIODetailsSingle parses the `# format single` / `# node`
// bullet block emitted when node_ids has exactly one entry. Returns
// (row, true) on success; (zero, false) if the section isn't present.
func parseFastIODetailsSingle(text string) (fastIODetailsRow, bool) {
	// Cheap probe before parsing.
	if !strings.Contains(text, "# format\nsingle") && !strings.Contains(text, "# format \nsingle") {
		return fastIODetailsRow{}, false
	}
	const header = "# node\n"
	idx := strings.Index(text, header)
	if idx < 0 {
		// Older builds may emit `# node` with trailing carriage
		// returns; try a tolerant probe.
		idx = strings.Index(text, "# node")
		if idx < 0 {
			return fastIODetailsRow{}, false
		}
	}
	rest := text[idx+len("# node"):]
	if end := indexOfNextH1(rest); end >= 0 {
		rest = rest[:end]
	}
	// Bullet lines look like `- **key:** value`. We pull the
	// scalar keys we care about; nested blocks (previews, ai,
	// summary, origin) start with `- **key:**` on their own line
	// and are indented below — those are fine to skip, our scalar
	// matcher only triggers when the value is on the same line.
	scalar := regexp.MustCompile(`^- \*\*([a-zA-Z_]+):\*\*\s+(.+)$`)

	// The web_url for single format lands AFTER the `# node` block
	// as its own H1 (`# web_url`). Hunt for it separately.
	row := fastIODetailsRow{}
	for _, line := range strings.Split(rest, "\n") {
		line = strings.TrimSpace(line)
		m := scalar.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		key, value := m[1], strings.TrimSpace(m[2])
		// Strip the surrounding `_` italic markers that fast.io
		// sometimes adds (rare, but defensive).
		value = strings.Trim(value, "_")
		switch key {
		case "id":
			row.ID = value
		case "name":
			row.Name = value
		case "parent":
			row.Parent = value
		case "mimetype":
			row.Mimetype = value
		case "size":
			if n, err := strconv.ParseInt(value, 10, 64); err == nil {
				row.Size = n
			}
		case "modified":
			if t, err := time.Parse("2006-01-02 15:04:05 MST", value); err == nil {
				row.Modified = t
			}
		}
	}
	// web_url is a top-level H1 outside the # node block.
	if idx := strings.Index(text, "# web_url\n"); idx >= 0 {
		webBody := text[idx+len("# web_url\n"):]
		if end := indexOfNextH1(webBody); end >= 0 {
			webBody = webBody[:end]
		}
		row.WebURL = strings.TrimSpace(webBody)
	}

	if row.ID == "" {
		return fastIODetailsRow{}, false
	}
	return row, true
}

// parseFastIODetailsMulti picks the `# nodes` markdown table out of a
// multi-id bulk-details response and decodes it.
func parseFastIODetailsMulti(text string) []fastIODetailsRow {
	const header = "# nodes"
	idx := strings.Index(text, header)
	if idx < 0 {
		return nil
	}
	rest := text[idx+len(header):]
	if end := indexOfNextH1(rest); end >= 0 {
		rest = rest[:end]
	}
	lines := strings.Split(strings.TrimSpace(rest), "\n")
	// Find the header row — first line starting with `|` and containing
	// an `id` cell. Robust against any leading whitespace fast.io might
	// insert between `# nodes` and the table.
	headerIdx := -1
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "|") && strings.Contains(line, "id") {
			headerIdx = i
			break
		}
	}
	if headerIdx < 0 || headerIdx+2 >= len(lines) {
		return nil
	}
	cols := splitMarkdownTableRow(lines[headerIdx])
	col := func(name string) int {
		for i, c := range cols {
			if strings.EqualFold(c, name) {
				return i
			}
		}
		return -1
	}
	idIdx := col("id")
	nameIdx := col("name")
	parentIdx := col("parent")
	modifiedIdx := col("modified")
	sizeIdx := col("size")
	mimeIdx := col("mimetype")
	webIdx := col("web_url")
	if idIdx < 0 || nameIdx < 0 {
		return nil
	}

	var out []fastIODetailsRow
	// Skip header + separator row.
	for _, line := range lines[headerIdx+2:] {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "|") {
			break
		}
		cells := splitMarkdownTableRow(line)
		get := func(i int) string {
			if i < 0 || i >= len(cells) {
				return ""
			}
			return strings.TrimSpace(cells[i])
		}
		row := fastIODetailsRow{
			ID:       get(idIdx),
			Name:     get(nameIdx),
			Parent:   get(parentIdx),
			Mimetype: get(mimeIdx),
			WebURL:   get(webIdx),
		}
		if s := get(sizeIdx); s != "" && s != emDashCell {
			if n, err := strconv.ParseInt(s, 10, 64); err == nil {
				row.Size = n
			}
		}
		if m := get(modifiedIdx); m != "" && m != emDashCell {
			// fast.io's stamp format is "2026-05-18 19:26:37 UTC".
			if t, err := time.Parse("2006-01-02 15:04:05 MST", m); err == nil {
				row.Modified = t
			}
		}
		if row.ID != "" {
			out = append(out, row)
		}
	}
	return out
}

// splitMarkdownTableRow splits `| a | b | c |` into ["a", "b", "c"],
// honoring `\|` as an escaped pipe inside a cell. Fast.io's markdown
// emitter escapes pipes in user-supplied content (notably filenames —
// a customer-pushed file can be named anything, including `Q4|2026
// report.csv`) and a naive `strings.Split` on `|` would shear that
// filename in half across two cells.
//
// We strip the leading/trailing empty cells the surrounding pipes
// produce and trim each cell. Backslash-escaped pipes are unescaped
// in the output so the caller sees the intended literal.
func splitMarkdownTableRow(line string) []string {
	// Walk the line one rune at a time so we can honor `\|`. Tiny
	// state machine: copy chars into the current cell; on `|` end
	// the cell; a backslash absorbs the next character literally
	// (which is how markdown escapes table-delimiters).
	var cells []string
	var cur strings.Builder
	runes := []rune(line)
	for i := 0; i < len(runes); i++ {
		ch := runes[i]
		if ch == '\\' && i+1 < len(runes) && runes[i+1] == '|' {
			cur.WriteRune('|')
			i++
			continue
		}
		if ch == '|' {
			cells = append(cells, cur.String())
			cur.Reset()
			continue
		}
		cur.WriteRune(ch)
	}
	cells = append(cells, cur.String())
	// `| a | b | c |` produces ["", " a ", " b ", " c ", ""] — drop
	// the surrounding empties. A row missing leading/trailing pipes
	// (which would be a fast.io bug, but defend anyway) lands here
	// without those empty cells and stays intact.
	if len(cells) >= 2 {
		if cells[0] == "" {
			cells = cells[1:]
		}
		if len(cells) > 0 && cells[len(cells)-1] == "" {
			cells = cells[:len(cells)-1]
		}
	}
	out := make([]string, len(cells))
	for i, p := range cells {
		out[i] = strings.TrimSpace(p)
	}
	return out
}

// renderFastIOFindResults builds the final markdown the agent sees.
// One header line summarizing the search, then a markdown table. No
// fast.io meta blocks (`_buildHash`, `_next`), no per-row bulleted
// metadata sprawl — every byte here is information the agent needs
// to pick the right file and act on it.
//
// Behavior contract: when there's exactly ONE matching file, the
// rendering invites the agent to proceed. When there are TWO OR
// MORE, the rendering hard-stops the agent with an explicit
// "do not pick — ask the user" directive. We had a real production
// failure (conv 3460d911) where the agent silently picked the wrong
// file from a list of similarly-named duplicates and the user only
// noticed when the report was already drafted. The directive makes
// that auto-pick antipattern explicit.
//
// Per-node fast.io errors (rare in practice but possible when an
// id surfaced by a stale search has since been trashed) are listed
// inline after the table.
func renderFastIOFindResults(originalQuery, winningQuery string, attempts []string, rows []fastIODetailsRow, errs map[string]string, limit int) string {
	var sb strings.Builder

	// Header line: report total found and which queries contributed.
	// When ≥2 passes ran, list each one so the agent can explain to
	// the user where results came from ("I searched for both your
	// phrasing and the ELC code; 12 of the 13 came from the ELC pass").
	fmt.Fprintf(&sb, "Found %d file(s) for query %q", len(rows), originalQuery)
	if len(attempts) > 1 {
		fmt.Fprintf(&sb, " (union of %d searches: %s)",
			len(attempts), strings.Join(quoteAll(attempts), ", "))
	} else if !strings.EqualFold(winningQuery, originalQuery) {
		// Only one attempt is ever shown when only the fallback ran;
		// surface the substitution explicitly.
		fmt.Fprintf(&sb, " via %q", winningQuery)
	}
	if len(rows) >= limit {
		fmt.Fprintf(&sb, ". Result capped at limit=%d; bump `limit` (max %d) to see more.", limit, fastIOFindMaxLimit)
	} else {
		sb.WriteString(".")
	}
	sb.WriteString(" Sorted newest-first.\n\n")

	sb.WriteString("| id | name | parent | modified | size | mimetype |\n")
	sb.WriteString("| --- | --- | --- | --- | --- | --- |\n")
	for _, r := range rows {
		modified := emDashCell
		if !r.Modified.IsZero() {
			modified = r.Modified.UTC().Format("2006-01-02 15:04")
		}
		size := emDashCell
		if r.Size > 0 {
			size = strconv.FormatInt(r.Size, 10)
		}
		mime := r.Mimetype
		if mime == "" {
			mime = emDashCell
		}
		parent := r.Parent
		if parent == "" {
			parent = emDashCell
		}
		// Escape any embedded pipes in name / parent so the rendered
		// markdown table doesn't fracture if a customer-pushed file
		// is named e.g. `Q4|2026.csv`. Same convention fast.io uses
		// on the way in.
		fmt.Fprintf(&sb, "| %s | %s | %s | %s | %s | %s |\n",
			r.ID,
			escapeMarkdownTableCell(r.Name),
			escapeMarkdownTableCell(parent),
			modified, size, mime)
	}

	if len(errs) > 0 {
		sb.WriteString("\nNodes that fast.io failed to hydrate (likely trashed or permission-denied):\n")
		// Sort error ids for deterministic output.
		errIDs := make([]string, 0, len(errs))
		for id := range errs {
			errIDs = append(errIDs, id)
		}
		sort.Strings(errIDs)
		for _, id := range errIDs {
			fmt.Fprintf(&sb, "  - %s: %s\n", id, errs[id])
		}
	}

	// Behavior contract — explicit so the agent treats it as a hard
	// rule, not a suggestion. The two-row threshold matters because:
	//   - 1 match: there's nothing to choose; proceeding is safe and
	//     skipping the user-confirm step keeps simple lookups one-shot.
	//   - 2+ matches: every duplicate could plausibly be the "right"
	//     one (per-folder copies, hash-suffixed variants, prior weeks).
	//     The agent should NOT guess; the user is the only authority.
	//
	// For the 2+ case we also surface a recommendation (the newest
	// row) and pre-write a suggested question so the agent can quote
	// it back verbatim — that's the "easy yes" path: user reads our
	// suggestion, replies "yes" or "no, the older one", agent proceeds.
	// Without a recommendation, busy users (Jeanne's case) might just
	// pick the wrong one or get frustrated by an open-ended question.
	sb.WriteString("\n")
	switch {
	case len(rows) == 1:
		sb.WriteString("Exactly one match. Use that `id` with `mcp_fast_io_download action=file-url` to fetch it.\n")
	case len(rows) >= 2:
		top := rows[0]
		modified := emDashCell
		if !top.Modified.IsZero() {
			modified = top.Modified.UTC().Format("2006-01-02 15:04 UTC")
		}
		sb.WriteString("**Multiple matches — STOP and ask the user which one to use.** Do NOT pick on their behalf — even when one row is clearly newest, duplicates exist for real reasons (per-week copies, per-folder isolation, hash-suffixed dedup variants), and the user is the only authority on which is canonical for their task.\n\n")
		fmt.Fprintf(&sb, "**Recommended (newest):** `%s` — modified %s, id `%s`.\n\n", top.Name, modified, top.ID)
		sb.WriteString("**Suggested phrasing — quote this back to the user, adapting names/dates to your specific results:**\n")
		fmt.Fprintf(&sb,
			"> I found %d matching file(s). The newest is **%s** (modified %s). Should I use that one, or did you want a different version from the list?\n\n",
			len(rows), top.Name, modified)
		sb.WriteString("If the user confirms the recommendation, use the `id` above. If they pick a different row, use that row's `id` instead. Either way: wait for their reply before downloading.\n")
	}
	sb.WriteString("Once chosen, use the `id` with `mcp_fast_io_download action=file-url`; the `parent` with `mcp_fast_io_storage action=list` to see siblings.\n")
	return sb.String()
}

// quoteAll wraps every string in `xs` in Go-style double quotes for
// list-rendering in the header line. Tiny helper, but keeps the
// formatting call site readable.
func quoteAll(xs []string) []string {
	out := make([]string, len(xs))
	for i, x := range xs {
		out[i] = fmt.Sprintf("%q", x)
	}
	return out
}

// escapeMarkdownTableCell escapes `|` characters in a cell value so a
// filename that legitimately contains a pipe doesn't break the
// rendered table. Newlines get replaced with spaces for the same
// reason — fast.io shouldn't return either in a filename, but
// customers push arbitrary bytes and we'd rather render correctly
// than blow up the parser on the other side.
func escapeMarkdownTableCell(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, "|", `\|`)
	s = strings.ReplaceAll(s, "\n", " ")
	return s
}

// renderFastIOFindNoHits is the empty-search response. We list every
// query we tried so the agent doesn't waste a turn retrying the same
// fallbacks fastio_find already burned. Crucially, the message ends
// with a concrete next action — list the workspace root — so the
// model doesn't dead-end here.
func renderFastIOFindNoHits(originalQuery string, attempts []string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "No files matched %q", originalQuery)
	if len(attempts) > 1 {
		fmt.Fprintf(&sb, " (tried: %s)", strings.Join(attempts, ", "))
	}
	sb.WriteString(".\n\n")
	sb.WriteString("Things to try next:\n")
	sb.WriteString("  - List the workspace root: `mcp_fast_io_storage action=list node_id=\"root\" detail=\"terse\"`\n")
	sb.WriteString("  - If the file was just uploaded, semantic indexing takes 5–30s; retry once.\n")
	sb.WriteString("  - Search by a single distinctive token rather than a multi-word phrase — fast.io's keyword search is AND-tokenized, so every word must match.\n")
	return sb.String()
}
