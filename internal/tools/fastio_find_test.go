package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/mcp"
)

// recordingCaller is a fakeMCPCaller variant that lets the test queue a
// SEQUENCE of responses, since fastio_find issues two calls (search +
// bulk details) and we want to assert each one separately. Built on top
// of mcp.ToolResult so the test fixture matches the real return shape.
type recordingCaller struct {
	calls     []fakeMCPCall
	responses []*mcp.ToolResult
	idx       int
}

func (r *recordingCaller) CallTool(_ context.Context, toolName string, args map[string]interface{}) (*mcp.ToolResult, error) {
	cp := make(map[string]interface{}, len(args))
	for k, v := range args {
		cp[k] = v
	}
	r.calls = append(r.calls, fakeMCPCall{toolName: toolName, args: cp})
	if r.idx >= len(r.responses) {
		return okResult(""), nil
	}
	out := r.responses[r.idx]
	r.idx++
	return out, nil
}

// realFastIOSearchTerseELC00109 is a verbatim, captured response from
// fast.io for `storage action=search query="ELC00109" detail=terse`.
// Keeping it real (not a synthesized mock) keeps the parser honest
// against future fast.io output drift — if they change column
// ordering or the meta-section names, this fixture rots loudly.
const realFastIOSearchTerseELC00109 = `**Result:** success

# files
- **2xjcp-ospjo-thypx-7qmav-7z2yl-se5x:**
  - **name:** ABC_ELC00109_Overall_Report.csv
  - **parent_id:** 2xtko-e6gtx-vh25h-wcs2d-24dos-34ko
  - **type:** file
  - **web_url:** https://elcano.fast.io/workspace/3117763504443666597-general/preview/2xjcp-ospjo-thypx-7qmav-7z2yl-se5x

# pagination
- **total:** 12
- **limit:** 100
- **offset:** 0
- **has_more:** false

# _buildHash
2026.05.7-b118774ec8

# _fetched_ids
- 2xjcp-ospjo-thypx-7qmav-7z2yl-se5x
- 2a5fj-opahz-4hsza-xiehb-nscsr-eq7a
- 2csss-4lokh-zuezi-a2ybi-uybml-oqof
- 2qbap-m3zzb-4ggdy-gnz23-vb2nh-a4fs

# _total_fetched
12

# _displayed
1
`

// realFastIODetailsTableELC00109 is a verbatim bulk-details response
// for three of the ABC_ELC00109 nodes. JSON cells (previews, ai,
// summary, origin) are present in the same shape fast.io returns
// them so the parser is exercised against the real-world column
// layout, including the cells with `{"thumbnail":{"ready":true},...}`
// objects that would naively split on `|`.
const realFastIODetailsTableELC00109 = `**Result:** success

# format
multi

# nodes
| id | type | name | parent | version | created | modified | restricted | dmca | locked | is_imported | size | mimetype | mimecategory | previews | ai | summary | metadata | origin | web_url |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 2xjcp-ospjo-thypx-7qmav-7z2yl-se5x | file | ABC_ELC00109_Overall_Report.csv | root | 37du7-l34yh | 2026-05-18 19:26:25 UTC | 2026-05-18 19:26:37 UTC | false | false | false | false | 10205 | text/csv | text | {"thumbnail":{"ready":false}} | {"state":"ready"} | {"title":"Digital Ad Campaign Metrics"} | — | {"creator":"294"} | https://elcano.fast.io/preview/2xjcp |
| 2mzo3-a32fw-sxu6r-cjzp6-o6552-wis7 | file | ABC_ELC00109_Overall_Report.csv | 2s7no-f6mdk-qdz5o-yvm6o-upd6i-vymo | 3aovf-fglyy | 2026-05-18 20:38:37 UTC | 2026-05-18 20:38:54 UTC | false | false | false | false | 10205 | text/csv | text | {"thumbnail":{"ready":true}} | {"state":"ready"} | {"title":"Digital Ad Campaign Metrics"} | — | {"creator":"294"} | https://elcano.fast.io/preview/2mzo3 |
| 2qbap-m3zzb-4ggdy-gnz23-vb2nh-a4fs | file | ABC_ELC00109_Overall Report.csv | root | 3wfw2-nq6mf | 2026-05-18 19:11:03 UTC | 2026-05-18 19:11:20 UTC | false | false | false | false | 86 | text/csv | text | {"thumbnail":{"ready":true}} | {"state":"ready"} | {"title":"Ad Campaign Performance Data"} | — | {"creator":"294"} | https://elcano.fast.io/preview/2qbap |

# errors
—

# node_count
3

# error_count
0

# requested_count
3
`

// emptySearchResponse — when fast.io's keyword AND-tokenizer rejects
// the natural-language query (the original ABC plumbing bug). The
// `# files` block exists but is empty; `_fetched_ids` is absent.
const emptySearchResponse = `**Result:** success

# files

# pagination
- **total:** 0
- **limit:** 100
- **offset:** 0
- **has_more:** false

# _total_fetched
0

# _displayed
0
`

func TestPlanFastIOSearchPasses_PromotesELCCode(t *testing.T) {
	got := planFastIOSearchPasses("ABC plumbing ELC00109")
	if len(got) != 2 {
		t.Fatalf("expected [original, code] passes; got %v", got)
	}
	if got[0] != "ABC plumbing ELC00109" || got[1] != "ELC00109" {
		t.Errorf("unexpected pass order: %v", got)
	}
}

func TestPlanFastIOSearchPasses_NoCodeMeansSinglePass(t *testing.T) {
	got := planFastIOSearchPasses("ABC plumbing")
	if len(got) != 1 || got[0] != "ABC plumbing" {
		t.Errorf("expected [original]; got %v", got)
	}
}

func TestPlanFastIOSearchPasses_QueryIsJustTheCode(t *testing.T) {
	got := planFastIOSearchPasses("ELC00109")
	if len(got) != 1 || got[0] != "ELC00109" {
		t.Errorf("expected to skip the redundant fallback; got %v", got)
	}
}

func TestPlanFastIOSearchPasses_CaseInsensitive(t *testing.T) {
	got := planFastIOSearchPasses("elc00109 abc")
	if len(got) != 2 || got[1] != "elc00109" {
		t.Errorf("expected case-insensitive code extraction with original casing preserved; got %v", got)
	}
}

func TestParseFastIOSearchIDs_FetchedIDsBlock(t *testing.T) {
	ids := parseFastIOSearchIDs(realFastIOSearchTerseELC00109)
	if len(ids) != 4 {
		t.Fatalf("expected 4 fetched ids; got %d (%v)", len(ids), ids)
	}
	if ids[0] != "2xjcp-ospjo-thypx-7qmav-7z2yl-se5x" {
		t.Errorf("first id wrong: %s", ids[0])
	}
}

func TestParseFastIOSearchIDs_FallsBackToFilesBullets(t *testing.T) {
	// Same payload but with the `_fetched_ids` section stripped.
	idx := strings.Index(realFastIOSearchTerseELC00109, "# _fetched_ids")
	if idx < 0 {
		t.Fatalf("test fixture missing _fetched_ids section; can't exercise the fallback path")
	}
	clipped := realFastIOSearchTerseELC00109[:idx]
	ids := parseFastIOSearchIDs(clipped)
	if len(ids) != 1 {
		t.Fatalf("expected 1 id from files-bullet fallback; got %d (%v)", len(ids), ids)
	}
}

// TestParseFastIODetailsSingle covers the format fast.io returns when
// `node_ids` happens to have exactly one entry. Live test caught this
// gap in the original implementation — the single-id response uses a
// `# format single` + `# node` bullet block instead of the table
// shape, and the bulk-only parser returned empty.
func TestParseFastIODetailsSingle_RealRow(t *testing.T) {
	body := `**Result:** success

# format
single

# node
- **id:** 26n2i-xygnf-vorvt-lhy3w-5laen-oybp
- **type:** file
- **name:** TWC_KOC_Scale Marketing_ABC Plumbing_ELC00109.xlsx
- **parent:** 2ndsi-usktv-w5phj-uddpc-xz33r-e47h
- **version:** 33qfb
- **created:** 2026-05-19 15:49:09 UTC
- **modified:** 2026-05-19 15:49:27 UTC
- **size:** 40988
- **mimetype:** application/vnd.openxmlformats-officedocument.spreadsheetml.sheet
- **mimecategory:** spreadsheet
- **previews:**
  - **thumbnail:**
    - **ready:** true

# _buildHash
2026.05.7-b118774ec8

# web_url
https://elcano.fast.io/preview/26n2i

# _next
- Download: download action file-url
`
	rows := parseFastIODetailsTable(body)
	if len(rows) != 1 {
		t.Fatalf("expected 1 row from single format; got %d", len(rows))
	}
	got := rows[0]
	if got.ID != "26n2i-xygnf-vorvt-lhy3w-5laen-oybp" {
		t.Errorf("id wrong: %s", got.ID)
	}
	if got.Name != "TWC_KOC_Scale Marketing_ABC Plumbing_ELC00109.xlsx" {
		t.Errorf("name wrong: %s", got.Name)
	}
	if got.Size != 40988 {
		t.Errorf("size wrong: %d", got.Size)
	}
	if got.Modified.IsZero() {
		t.Errorf("modified wasn't parsed")
	}
	if got.WebURL != "https://elcano.fast.io/preview/26n2i" {
		t.Errorf("web_url wrong: %s", got.WebURL)
	}
}

func TestParseFastIODetailsTable_RealRows(t *testing.T) {
	rows := parseFastIODetailsTable(realFastIODetailsTableELC00109)
	if len(rows) != 3 {
		t.Fatalf("expected 3 rows; got %d", len(rows))
	}
	if rows[0].ID != "2xjcp-ospjo-thypx-7qmav-7z2yl-se5x" {
		t.Errorf("row[0].id wrong: %s", rows[0].ID)
	}
	if rows[0].Name != "ABC_ELC00109_Overall_Report.csv" {
		t.Errorf("row[0].name wrong: %s", rows[0].Name)
	}
	if rows[0].Size != 10205 {
		t.Errorf("row[0].size wrong: %d", rows[0].Size)
	}
	if rows[0].Mimetype != "text/csv" {
		t.Errorf("row[0].mimetype wrong: %s", rows[0].Mimetype)
	}
	if rows[0].Modified.IsZero() {
		t.Errorf("row[0].modified wasn't parsed")
	}
	// The middle row was modified ~hour later — newest of the three.
	if !rows[1].Modified.After(rows[0].Modified) {
		t.Errorf("expected row[1] modified after row[0]; got %s vs %s", rows[1].Modified, rows[0].Modified)
	}
}

func TestRunFastIOFind_UnionsELCFallbackWithNaturalQuery(t *testing.T) {
	caller := &recordingCaller{
		responses: []*mcp.ToolResult{
			okResult(emptySearchResponse),            // pass 1: "ABC plumbing" — empty
			okResult(realFastIOSearchTerseELC00109),  // pass 2: "ELC00109" — hits
			okResult(realFastIODetailsTableELC00109), // bulk details
		},
	}
	out, err := runFastIOFind(context.Background(), caller, FastIOFindParams{
		Query:       "ABC plumbing ELC00109",
		WorkspaceID: "4817763504744262145",
		Limit:       10,
	})
	if err != nil {
		t.Fatalf("runFastIOFind: %v", err)
	}

	if len(caller.calls) != 3 {
		t.Fatalf("expected 3 MCP calls (search, search, details); got %d", len(caller.calls))
	}
	// Both search calls fire — we union their hits rather than stopping
	// at the first non-empty result.
	if got := caller.calls[0].args["query"]; got != "ABC plumbing ELC00109" {
		t.Errorf("call 0 query = %v, want natural query", got)
	}
	if got := caller.calls[1].args["query"]; got != "ELC00109" {
		t.Errorf("call 1 query = %v, want \"ELC00109\"", got)
	}
	if got := caller.calls[2].args["action"]; got != "details" {
		t.Errorf("call 2 action = %v, want \"details\"", got)
	}
	if _, ok := caller.calls[2].args["node_ids"].([]string); !ok {
		t.Errorf("call 2 node_ids should be []string; got %T", caller.calls[2].args["node_ids"])
	}

	// Output discloses both searches so the agent can explain the
	// behavior to the user.
	if !strings.Contains(out, `union of 2 searches`) {
		t.Errorf("output should disclose the union strategy:\n%s", out)
	}
	if !strings.Contains(out, `"ELC00109"`) {
		t.Errorf("output should list the ELC fallback attempt:\n%s", out)
	}
	// Tight rendering — no fast.io meta sections.
	if strings.Contains(out, "_buildHash") || strings.Contains(out, "_next") {
		t.Errorf("output leaked fast.io meta sections:\n%s", out)
	}
	if !strings.Contains(out, "| id | name |") {
		t.Errorf("output missing the results table header:\n%s", out)
	}
}

func TestRunFastIOFind_SortsNewestFirst(t *testing.T) {
	caller := &recordingCaller{
		responses: []*mcp.ToolResult{
			okResult(realFastIOSearchTerseELC00109),
			okResult(realFastIODetailsTableELC00109),
		},
	}
	out, err := runFastIOFind(context.Background(), caller, FastIOFindParams{
		Query:       "ELC00109",
		WorkspaceID: "4817763504744262145",
	})
	if err != nil {
		t.Fatalf("runFastIOFind: %v", err)
	}

	// The newest row (2mzo3, modified 2026-05-18 20:38) should appear
	// before the older 2xjcp row (modified 19:26) which should appear
	// before the oldest 2qbap (19:11).
	posNewest := strings.Index(out, "2mzo3-a32fw")
	posMiddle := strings.Index(out, "2xjcp-ospjo")
	posOldest := strings.Index(out, "2qbap-m3zzb")
	if posNewest <= 0 || posMiddle <= posNewest || posOldest <= posMiddle {
		t.Errorf("rows not sorted newest-first; got positions %d %d %d\n%s",
			posNewest, posMiddle, posOldest, out)
	}
}

func TestRunFastIOFind_NoHits(t *testing.T) {
	caller := &recordingCaller{
		responses: []*mcp.ToolResult{
			okResult(emptySearchResponse), // pass 1: empty
			okResult(emptySearchResponse), // pass 2 (ELC fallback): also empty
		},
	}
	out, err := runFastIOFind(context.Background(), caller, FastIOFindParams{
		Query:       "ABC plumbing ELC99999",
		WorkspaceID: "4817763504744262145",
	})
	if err != nil {
		t.Fatalf("runFastIOFind: %v", err)
	}
	if !strings.Contains(out, "No files matched") {
		t.Errorf("expected no-hits message; got:\n%s", out)
	}
	if !strings.Contains(out, "ELC99999") {
		t.Errorf("expected the no-hits message to list the fallback ELC code; got:\n%s", out)
	}
	// We made TWO search attempts (one fallback), no details call.
	if len(caller.calls) != 2 {
		t.Errorf("expected 2 calls (search + ELC-fallback search); got %d", len(caller.calls))
	}
}

func TestRunFastIOFind_RequiresWorkspaceID(t *testing.T) {
	caller := &recordingCaller{}
	_, err := runFastIOFind(context.Background(), caller, FastIOFindParams{
		Query: "anything",
	})
	if err == nil || !strings.Contains(err.Error(), "workspace_id") {
		t.Errorf("expected workspace_id error; got %v", err)
	}
	if len(caller.calls) != 0 {
		t.Errorf("no MCP calls should happen when validation fails")
	}
}

func TestRunFastIOFind_RequiresQuery(t *testing.T) {
	caller := &recordingCaller{}
	_, err := runFastIOFind(context.Background(), caller, FastIOFindParams{
		WorkspaceID: "4817763504744262145",
	})
	if err == nil || !strings.Contains(err.Error(), "query") {
		t.Errorf("expected query error; got %v", err)
	}
}

// TestRunFastIOFind_MultiMatchHardStopsForUserChoice locks in the
// behavior contract: when 2+ files match, the response must contain
// an explicit "STOP and ask the user" directive PLUS a concrete
// recommendation (the newest row) and a pre-written question the
// agent can quote back. Without that "easy yes" path, busy users
// (Jeanne's case) get an open-ended question that's hard to answer
// quickly.
func TestRunFastIOFind_MultiMatchHardStopsForUserChoice(t *testing.T) {
	caller := &recordingCaller{
		responses: []*mcp.ToolResult{
			okResult(realFastIOSearchTerseELC00109),
			okResult(realFastIODetailsTableELC00109),
		},
	}
	out, err := runFastIOFind(context.Background(), caller, FastIOFindParams{
		Query: "ELC00109", WorkspaceID: "ws-1",
	})
	if err != nil {
		t.Fatalf("runFastIOFind: %v", err)
	}
	for _, must := range []string{
		"STOP and ask the user",
		"Do NOT pick on their behalf",
		"**Recommended (newest):**",
		"Suggested phrasing",
		"Should I use that one, or did you want a different version",
	} {
		if !strings.Contains(out, must) {
			t.Errorf("multi-match output missing required text %q:\n%s", must, out)
		}
	}
	// Recommendation should point to the newest row in the fixture
	// (2mzo3 — modified 2026-05-18 20:38, newer than 2xjcp and 2qbap).
	if !strings.Contains(out, "2mzo3-a32fw-sxu6r-cjzp6-o6552-wis7") {
		t.Errorf("recommendation should cite the newest row's id; got:\n%s", out)
	}
}

// TestRunFastIOFind_SingleMatchAllowsAutoProceed covers the comple-
// mentary case: with exactly 1 match, the response invites the agent
// to proceed without an extra round-trip to the user.
func TestRunFastIOFind_SingleMatchAllowsAutoProceed(t *testing.T) {
	// Fixture: search returns one id, details bulk returns one row.
	singleSearch := `**Result:** success

# files
- **abc-id:**
  - **name:** only.csv

# _fetched_ids
- abc-id

# _total_fetched
1
# _displayed
1
`
	singleDetails := `**Result:** success

# format
multi

# nodes
| id | type | name | parent | version | created | modified | restricted | dmca | locked | is_imported | size | mimetype | mimecategory | previews | ai | summary | metadata | origin | web_url |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| abc-id | file | only.csv | root | v1 | 2026-05-18 10:00:00 UTC | 2026-05-18 10:00:00 UTC | false | false | false | false | 100 | text/csv | text | {} | {} | {} | — | {} | https://e.fast.io/abc |

# errors
—
# node_count
1
`
	caller := &recordingCaller{
		responses: []*mcp.ToolResult{okResult(singleSearch), okResult(singleDetails)},
	}
	out, err := runFastIOFind(context.Background(), caller, FastIOFindParams{
		Query: "only", WorkspaceID: "ws-1",
	})
	if err != nil {
		t.Fatalf("runFastIOFind: %v", err)
	}
	if strings.Contains(out, "STOP and ask the user") {
		t.Errorf("single-match output must NOT hard-stop the agent:\n%s", out)
	}
	if !strings.Contains(out, "Exactly one match") {
		t.Errorf("single-match output should say so explicitly:\n%s", out)
	}
}

// TestRunFastIOFind_DedupsRepeatedIDs covers the duplicate-id defense.
// A future fast.io build (or a partial search response merged with a
// retry) could return the same id twice — without dedup, bulk-details
// would either reject the request or return a duplicate row. We dedup
// first so the agent never sees the duplicate.
func TestRunFastIOFind_DedupsRepeatedIDs(t *testing.T) {
	// Hand-build a search response with the same id twice in _fetched_ids.
	dupSearch := `**Result:** success

# files
- **abc-def-ghi:**
  - **name:** dup.csv

# _fetched_ids
- abc-def-ghi
- abc-def-ghi
- xyz-uvw-rst

# _total_fetched
3
# _displayed
3
`
	dupDetails := `**Result:** success

# format
multi

# nodes
| id | type | name | parent | version | created | modified | restricted | dmca | locked | is_imported | size | mimetype | mimecategory | previews | ai | summary | metadata | origin | web_url |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| abc-def-ghi | file | dup.csv | root | v1 | 2026-05-18 10:00:00 UTC | 2026-05-18 10:00:00 UTC | false | false | false | false | 100 | text/csv | text | {} | {} | {} | — | {} | https://e.fast.io/abc |
| xyz-uvw-rst | file | other.csv | root | v1 | 2026-05-18 11:00:00 UTC | 2026-05-18 11:00:00 UTC | false | false | false | false | 200 | text/csv | text | {} | {} | {} | — | {} | https://e.fast.io/xyz |

# errors
—
# node_count
2
`
	caller := &recordingCaller{
		responses: []*mcp.ToolResult{okResult(dupSearch), okResult(dupDetails)},
	}
	_, err := runFastIOFind(context.Background(), caller, FastIOFindParams{
		Query: "dup", WorkspaceID: "ws-1",
	})
	if err != nil {
		t.Fatalf("runFastIOFind: %v", err)
	}
	// The bulk-details call should have received the dedup'd id list.
	gotIDs, _ := caller.calls[1].args["node_ids"].([]string)
	if len(gotIDs) != 2 {
		t.Errorf("expected dedup'd ids; got %v", gotIDs)
	}
}

// TestSplitMarkdownTableRow_HonorsEscapedPipes is the customer-named-
// file defense. A user could push `Q4|2026 report.csv` to fast.io;
// fast.io would emit `Q4\|2026 report.csv` in the markdown table; a
// naive splitter would cut the filename in half. This test pins the
// behavior.
func TestSplitMarkdownTableRow_HonorsEscapedPipes(t *testing.T) {
	cells := splitMarkdownTableRow(`| abc-def | Q4\|2026 report.csv | root |`)
	if len(cells) != 3 {
		t.Fatalf("expected 3 cells; got %d (%v)", len(cells), cells)
	}
	if cells[1] != "Q4|2026 report.csv" {
		t.Errorf("escaped pipe wasn't preserved; got %q", cells[1])
	}
}

// TestEscapeMarkdownTableCell pins the rendering side of the pipe-in-
// filename problem — we escape on the way OUT too so the table the
// agent reads back is well-formed even when names carry pipes.
func TestEscapeMarkdownTableCell(t *testing.T) {
	cases := []struct{ in, want string }{
		{"plain", "plain"},
		{"Q4|2026.csv", `Q4\|2026.csv`},
		{"weird\\name", `weird\\name`},
		{"two\nlines.csv", "two lines.csv"},
	}
	for _, c := range cases {
		if got := escapeMarkdownTableCell(c.in); got != c.want {
			t.Errorf("escapeMarkdownTableCell(%q) = %q; want %q", c.in, got, c.want)
		}
	}
}

// TestParseFastIODetailsErrors covers the partial-failure path: a
// bulk-details call where some ids hydrated and some didn't (e.g. one
// was trashed between the search and the details lookup). We want to
// surface the failures inline rather than silently drop them.
func TestParseFastIODetailsErrors(t *testing.T) {
	body := `**Result:** success

# format
multi

# nodes
| id | name |
| --- | --- |
| abc | a.csv |

# errors
| id | error |
| --- | --- |
| xyz | not found |
| pqr | permission denied |

# node_count
1
`
	errs := parseFastIODetailsErrors(body)
	if len(errs) != 2 {
		t.Fatalf("expected 2 errors; got %d (%v)", len(errs), errs)
	}
	if errs["xyz"] != "not found" || errs["pqr"] != "permission denied" {
		t.Errorf("error rows decoded wrong: %v", errs)
	}
}

func TestParseFastIODetailsErrors_NoErrors(t *testing.T) {
	body := `**Result:** success

# nodes
| id |
| --- |
| abc |

# errors
—
`
	errs := parseFastIODetailsErrors(body)
	if len(errs) != 0 {
		t.Errorf("expected no errors for the —-only block; got %v", errs)
	}
}

// TestPlanFastIOSearchPasses_CapsFallbacks defends the worst-case
// query (multiple ELC codes) from blowing up into too many round-
// trips. Two fallbacks max, regardless of how many codes appear.
func TestPlanFastIOSearchPasses_CapsFallbacks(t *testing.T) {
	got := planFastIOSearchPasses("compare ELC00109 ELC00115 ELC00118 ELC00129")
	// 1 original + max 2 fallbacks = 3 passes.
	if len(got) != 1+fastIOFindMaxFallbacks {
		t.Errorf("expected %d passes; got %v", 1+fastIOFindMaxFallbacks, got)
	}
}

func TestRunFastIOFind_ClampsLimit(t *testing.T) {
	caller := &recordingCaller{
		responses: []*mcp.ToolResult{okResult(emptySearchResponse)},
	}
	_, _ = runFastIOFind(context.Background(), caller, FastIOFindParams{
		Query:       "ELC00109",
		WorkspaceID: "4817763504744262145",
		Limit:       500, // way over the max
	})
	got := caller.calls[0].args["display_limit"]
	if got != fastIOFindMaxLimit {
		t.Errorf("expected display_limit clamped to %d; got %v", fastIOFindMaxLimit, got)
	}
}
