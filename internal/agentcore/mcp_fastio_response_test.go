package agentcore

import (
	"strings"
	"testing"
)

// Real fast.io storage.search response captured against the live
// workspace (query="ABC"). Verbatim so the trim parser is exercised
// against the exact byte shape production sees.
const realFastIOSearchResponse = `**Result:** success

# files
- **26n2i-xygnf-vorvt-lhy3w-5laen-oybp:**
  - **name:** TWC_KOC_Scale Marketing_ABC Plumbing_ELC00109.xlsx
  - **parent_id:** 2ndsi-usktv-w5phj-uddpc-xz33r-e47h
  - **type:** file
  - **web_url:** https://elcano.fast.io/workspace/3117763504443666597-general/preview/26n2i-xygnf-vorvt-lhy3w-5laen-oybp

# pagination
- **total:** 1
- **limit:** 100
- **offset:** 0
- **has_more:** false

# _buildHash
2026.05.7-b118774ec8

# _fetched_ids
- 26n2i-xygnf-vorvt-lhy3w-5laen-oybp

# _total_fetched
1

# _displayed
1

# _next
- Search hits at detail='terse' do NOT indicate trash status, and content_snippet is platform-capped at ~200 bytes (hits ending in ` + "`" + `…` + "`" + ` have more content available). Bump to detail='standard' for both trash visibility (the ` + "`" + `deleted` + "`" + `/` + "`" + `deleted_from` + "`" + ` lifecycle fields) and a longer snippet (~600 bytes), or detail='full' for the untruncated snippet.
- Upload a file: upload action create-session with profile_type="workspace", profile_id="***REMOVED***" (then POST /blob + chunk + finalize), or upload action web-import for URLs
- Search files: storage action search with profile_type="workspace", profile_id="***REMOVED***"
- Download a file: download action file-url with profile_type="workspace", profile_id="***REMOVED***"
- Ask AI about files: ai action chat-create with workspace_id="***REMOVED***"
- ` + "`" + ` fetch one with storage action='details' node_id=<id> (use entries from _fetched_ids to recover specific results) ` + "`" + `
`

// Real fast.io storage.details (single) response captured live. Has
// the AI-summary `long` paragraph, virus block, origin block, and
// nested previews — all the bullet-block shapes we strip.
const realFastIODetailsSingleResponse = `**Result:** success

# format
single

# node
- **id:** ***REMOVED***
- **type:** file
- **name:** ABC_ELC00109_Overall_Report.csv
- **parent:** root
- **version:** 37du7-l34yh-f3yjx-cz4nw-4uhfv-oexb
- **created:** 2026-05-18 19:26:25 UTC
- **modified:** 2026-05-18 19:26:37 UTC
- **size:** 10205
- **hash:** baae921bc6fa1244ec3e2caca4fef5a41a245324abbd834eeeadd062520d6a64
- **hash_algo:** sha256
- **mimetype:** text/csv
- **mimecategory:** text
- **previews:**
  - **thumbnail:**
    - **state:** not generated
  - **image:**
    - **state:** not generated
- **virus:**
  - **status:** unscanned
  - **reason:** scan not run
- **ai:**
  - **state:** ready
  - **attach:** true
  - **summary:** true
- **summary:**
  - **title:** Digital Ad Campaign Metrics
  - **short:** This document presents a daily breakdown of digital advertising campaign performance across various platforms and deals.
  - **long:**
    ` + "```" + `
    The provided data offers a detailed look into a digital advertising campaign, meticulously logging performance metrics on a daily basis from May 11th to May 17th, 2026.

    Across the dataset, a variety of campaigns are tracked, including those with "ABCP_GUM_PMP_VIANT_OLV", "ABCP_MGNTE_PMP_VIANT_CTV", "ABCP_MGNTE_PMP_VIANT_OLV", "ABCP_ROKU_PMP_VIANT_CTV", "ABCP_The Weather Company_PMP_VIANT_CTV", and "Scale_Magnite" identifiers.

    The data illustrates the ebb and flow of campaign performance with daily totals.
    ` + "```" + `
- **origin:**
  - **type:** chunked
  - **creator:** 2947763503651577469
  - **upload_session_id:** 5qil2jrne7553u3poqdud3kdsu4bc

# _buildHash
2026.05.7-b118774ec8

# web_url
https://elcano.fast.io/workspace/3117763504443666597-general/preview/***REMOVED***

# _next
- Download: download action file-url
- Comment: comment action add
`

// Real fast.io download.file-url response. The JWT in download_token
// is the same JWT embedded in download_url's ?token= query — pure
// duplication.
const realFastIODownloadResponse = `**Result:** success

# download_token
***REMOVED***

# download_url
https://api.fast.io/current/workspace/***REMOVED***/storage/***REMOVED***/read/?token=***REMOVED***

# resource_uri
download://workspace/***REMOVED***/***REMOVED***

# web_url
https://elcano.fast.io/workspace/3117763504443666597-general/preview/***REMOVED***

# _next
- Return the download_url to the user — it provides direct file access

# _warnings
- Download token is temporary (~2 hours). If expired, call download action file-url again to get a new URL.
`

// Real fast.io upload.create-session response. Has the upload_method
// constant-text H1 and the blob_upload.important boilerplate inside.
const realFastIOUploadCreateSessionResponse = `**Result:** success

# upload_id
5sfmz-7zqnu-cdeci-qdxwz-ay3hu-qmwm

# session_verified
confirmed

# api_min_chunk_bytes
1048576

# upload_method
Use the curl command in blob_upload to POST file data, then chunk with blob_id

# blob_upload
- **blob_endpoint:** https://mcp.fast.io/blob
- **session_id:** d74392e8ed14341783b825f849777becd6082a03894a7c365e12c1f44e9f1f44
- **curl_command:**
  ` + "```" + `
  curl -X POST "https://mcp.fast.io/blob" -H "Mcp-Session-Id: d74392e8ed14341783b825f849777becd6082a03894a7c365e12c1f44e9f1f44" -H "Content-Type: application/octet-stream" --data-binary @<file>
  ` + "```" + `
- **important:** You MUST use this exact Mcp-Session-Id when POSTing to /blob. The blob is stored in this MCP session's memory — using a different session ID will stage the blob in a different location, and the chunk action will not find it.

# _next
- POST file data to /blob using the curl command in blob_upload, then upload action chunk with blob_id, then upload action finalize.

# _warnings
- Reminder: filesize was set to 100 bytes — your chunk uploads must total exactly 100 bytes.
`

// === Search response (H1-section drops only) ===

func TestTrimFastIOResponse_SearchStripsBuildHashAndNext(t *testing.T) {
	got := trimFastIOResponse(realFastIOSearchResponse)
	for _, must := range []string{"# _buildHash", "# _next", "2026.05.7-b118774ec8", "Upload a file: upload action create-session"} {
		if strings.Contains(got, must) {
			t.Errorf("trimmed output still contains %q:\n%s", must, got)
		}
	}
	for _, keep := range []string{
		"# files",
		"TWC_KOC_Scale Marketing_ABC Plumbing_ELC00109.xlsx",
		"# pagination",
		"# _fetched_ids", "# _total_fetched", "# _displayed",
		"26n2i-xygnf-vorvt-lhy3w-5laen-oybp",
	} {
		if !strings.Contains(got, keep) {
			t.Errorf("trimmed output dropped a section we wanted to keep: %q\n%s", keep, got)
		}
	}
}

// === Details response (nested-bullet drops) ===

func TestTrimFastIOResponse_DetailsStripsAISummaryAndVirusAndOrigin(t *testing.T) {
	got := trimFastIOResponse(realFastIODetailsSingleResponse)

	for _, must := range []string{
		"- **virus:**",
		"- **origin:**",
		"- **long:**",
		"upload_session_id",
		"ABCP_GUM_PMP_VIANT_OLV", // body of summary.long
		"unscanned",              // virus.status
		"scan not run",           // virus.reason
		"2947763503651577469",    // origin.creator
	} {
		if strings.Contains(got, must) {
			t.Errorf("trimmed output still contains %q\n--- output ---\n%s", must, got)
		}
	}

	// Keep the useful parts of the node block.
	for _, keep := range []string{
		"# node",
		"ABC_ELC00109_Overall_Report.csv",
		"- **modified:**", "- **size:**", "- **mimetype:**",
		"- **hash:**",                 // sha256 is short, agent may use it
		"- **short:**",                // 1-line summary stays
		"Digital Ad Campaign Metrics", // summary.title
		"# web_url",
	} {
		if !strings.Contains(got, keep) {
			t.Errorf("trimmed output dropped a section we wanted to keep: %q\n--- output ---\n%s", keep, got)
		}
	}
}

func TestTrimFastIOResponse_DetailsSavings(t *testing.T) {
	before := len(realFastIODetailsSingleResponse)
	after := len(trimFastIOResponse(realFastIODetailsSingleResponse))
	if after >= before*70/100 {
		t.Errorf("expected significant trim on details; before=%d after=%d", before, after)
	}
	t.Logf("details trim: %d → %d bytes (%.1f%% saved)", before, after, 100*float64(before-after)/float64(before))
}

// === Download response (drop JWT duplication + resource_uri) ===

func TestTrimFastIOResponse_DownloadDropsTokenAndResourceURI(t *testing.T) {
	got := trimFastIOResponse(realFastIODownloadResponse)
	for _, must := range []string{
		"# download_token",
		"# resource_uri",
	} {
		if strings.Contains(got, must) {
			t.Errorf("trimmed output still contains %q\n%s", must, got)
		}
	}
	// The download_url body is the SAME JWT — that copy stays, because
	// the agent uses the URL. We only drop the standalone duplicate.
	for _, keep := range []string{
		"# download_url",
		"# web_url",
		"# _warnings",
		"Download token is temporary",
	} {
		if !strings.Contains(got, keep) {
			t.Errorf("trimmed output dropped %q\n%s", keep, got)
		}
	}
}

// === Upload create-session (drop upload_method + blob_upload.important) ===

func TestTrimFastIOResponse_UploadDropsMethodAndImportant(t *testing.T) {
	got := trimFastIOResponse(realFastIOUploadCreateSessionResponse)
	for _, must := range []string{
		"# upload_method",
		"- **important:**",
		"Use the curl command in blob_upload to POST file data", // upload_method body
		"You MUST use this exact Mcp-Session-Id",                // important body
	} {
		if strings.Contains(got, must) {
			t.Errorf("trimmed output still contains %q\n%s", must, got)
		}
	}
	// Keep curl_command and session_id — the agent NEEDS those.
	for _, keep := range []string{
		"# upload_id",
		"# blob_upload",
		"- **blob_endpoint:**",
		"- **session_id:**",
		"- **curl_command:**",
		"# _warnings",
	} {
		if !strings.Contains(got, keep) {
			t.Errorf("trimmed output dropped %q\n%s", keep, got)
		}
	}
}

// === Helpers ===

func TestTrimFastIOResponse_NontrivialByteSavings(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"search", realFastIOSearchResponse},
		{"details", realFastIODetailsSingleResponse},
		{"download", realFastIODownloadResponse},
		{"upload", realFastIOUploadCreateSessionResponse},
	}
	for _, c := range cases {
		before := len(c.in)
		after := len(trimFastIOResponse(c.in))
		t.Logf("%s: %d → %d bytes (%.1f%% saved)", c.name, before, after, 100*float64(before-after)/float64(before))
		// At least 20% savings on every real fixture — sanity floor;
		// the realized numbers are much higher (60%+ on search).
		if after >= before*80/100 {
			t.Errorf("%s: expected ≥20%% savings; got before=%d after=%d", c.name, before, after)
		}
	}
}

func TestTrimFastIOResponse_NoMetaSectionsIsNoop(t *testing.T) {
	plain := "**Result:** success\n\n# files\n- a.csv\n\n# pagination\n- total: 1\n"
	got := trimFastIOResponse(plain)
	if got != plain {
		t.Errorf("input without trim triggers should pass through verbatim\n  in:  %q\n  out: %q", plain, got)
	}
}

func TestTrimFastIOResponse_EmptyAndAllNoise(t *testing.T) {
	if trimFastIOResponse("") != "" {
		t.Errorf("empty input should stay empty")
	}
	noiseOnly := "# _buildHash\n1.2.3\n\n# _next\n- do a thing\n"
	got := trimFastIOResponse(noiseOnly)
	if got != "" {
		t.Errorf("all-noise input should trim to empty; got %q", got)
	}
}

func TestParseFastIOH1Header(t *testing.T) {
	cases := []struct {
		in     string
		name   string
		wantOk bool
	}{
		{"# _buildHash", "_buildHash", true},
		{"# _next", "_next", true},
		{"# download_token", "download_token", true},
		{"# resource_uri", "resource_uri", true},
		{"# upload_method", "upload_method", true},
		{"# files", "files", true}, // accepts but won't be in the drop set
		{"  # _next", "", false},   // leading whitespace — not a header
		{"## _next", "", false},    // H2, not H1
		{"# important note", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		got, ok := parseFastIOH1Header(c.in)
		if ok != c.wantOk || got != c.name {
			t.Errorf("parseFastIOH1Header(%q) = (%q, %v); want (%q, %v)",
				c.in, got, ok, c.name, c.wantOk)
		}
	}
}

func TestParseNoisyBulletKey(t *testing.T) {
	cases := []struct {
		in     string
		key    string
		wantOk bool
	}{
		{"- **virus:**", "virus", true},
		{"- **origin:**", "origin", true},
		{"  - **long:**", "long", true},
		{"    - **important:** body text here", "important", true},
		{"  - **short:** scalar value here", "short", true},
		{"- not a bullet", "", false},
		{"- **bad-format", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		got, ok := parseNoisyBulletKey(c.in)
		if ok != c.wantOk || got != c.key {
			t.Errorf("parseNoisyBulletKey(%q) = (%q, %v); want (%q, %v)",
				c.in, got, ok, c.key, c.wantOk)
		}
	}
}

// TestStripNoisyNestedBullets_PreservesSiblings is the regression
// guard for the indentation scanner — when we drop a `- **virus:**`
// block, the following sibling bullet (at the SAME indentation level)
// must NOT be swallowed. Caught a real bug in the first iteration.
func TestStripNoisyNestedBullets_PreservesSiblings(t *testing.T) {
	lines := []string{
		"- **id:** abc",
		"- **virus:**",
		"  - **status:** unscanned",
		"  - **reason:** scan not run",
		"- **size:** 10205", // sibling after the dropped block — MUST survive
		"- **origin:**",
		"  - **creator:** 12345",
		"- **mimetype:** text/csv", // ditto
	}
	out := stripNoisyNestedBullets(lines, map[string]bool{
		"virus":  true,
		"origin": true,
	})
	got := strings.Join(out, "\n")
	for _, must := range []string{"- **id:** abc", "- **size:** 10205", "- **mimetype:** text/csv"} {
		if !strings.Contains(got, must) {
			t.Errorf("sibling bullet dropped accidentally; expected %q in:\n%s", must, got)
		}
	}
	for _, mustnt := range []string{"- **virus:**", "- **origin:**", "unscanned", "12345"} {
		if strings.Contains(got, mustnt) {
			t.Errorf("targeted block survived; %q should be gone in:\n%s", mustnt, got)
		}
	}
}

// TestStripNoisyNestedBullets_NestedDeepKey is the regression for
// drop-keys that legitimately appear as nested children (e.g. the
// `long:` bullet inside `summary:`). We must drop the nested `long`
// without dropping its siblings or the parent `summary:` itself.
func TestStripNoisyNestedBullets_NestedDeepKey(t *testing.T) {
	lines := []string{
		"- **summary:**",
		"  - **title:** Digital Ad Campaign Metrics",
		"  - **short:** This document presents a daily breakdown.",
		"  - **long:**",
		"    ```",
		"    Multi-paragraph AI text here.",
		"    More content here too.",
		"    ```",
		"- **mimetype:** text/csv",
	}
	out := stripNoisyNestedBullets(lines, map[string]bool{"long": true})
	got := strings.Join(out, "\n")
	for _, must := range []string{
		"- **summary:**",
		"- **title:** Digital Ad Campaign Metrics",
		"- **short:** This document presents",
		"- **mimetype:** text/csv",
	} {
		if !strings.Contains(got, must) {
			t.Errorf("expected to keep %q; got:\n%s", must, got)
		}
	}
	for _, mustnt := range []string{
		"- **long:**", "Multi-paragraph AI text", "More content here too",
	} {
		if strings.Contains(got, mustnt) {
			t.Errorf("expected to drop %q; got:\n%s", mustnt, got)
		}
	}
}

func TestLeadingSpaces(t *testing.T) {
	cases := map[string]int{
		"":            0,
		"hello":       0,
		"  hello":     2,
		"    hello":   4,
		"\thello":     1,
		"  \t  hello": 5,
		"  ":          2, // counts even if line is whitespace-only
	}
	for in, want := range cases {
		if got := leadingSpaces(in); got != want {
			t.Errorf("leadingSpaces(%q) = %d; want %d", in, got, want)
		}
	}
}
