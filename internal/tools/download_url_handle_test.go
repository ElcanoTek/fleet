//go:build fleet_host_executor

package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestProtectFastIODownloadURLs_VaultsBearerURLOnly(t *testing.T) {
	const signed = "https://api.fast.io/current/workspace/123/storage/node/read/?token=secret-bearer-value"
	const web = "https://elcano.fast.io/workspace/general/preview/node"
	input := "**Result:** success\n\n# download_url\n" + signed + "\n\n# web_url\n" + web + "\n"

	got := ProtectFastIODownloadURLs(input)
	if strings.Contains(got, signed) || strings.Contains(got, "secret-bearer-value") {
		t.Fatalf("protected response leaked the signed URL:\n%s", got)
	}
	if !strings.Contains(got, web) {
		t.Fatalf("human-facing web_url should remain unchanged:\n%s", got)
	}

	var handle string
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(line, downloadURLHandlePrefix) {
			handle = line
			break
		}
	}
	if handle == "" {
		t.Fatalf("response did not contain an opaque download handle:\n%s", got)
	}
	resolved, protected, err := resolveDownloadURLHandle(handle)
	if err != nil || !protected || resolved != signed {
		t.Fatalf("resolve handle = (%q, %v, %v), want (%q, true, nil)", resolved, protected, err, signed)
	}
}

func TestDownloadURL_OpaqueHandleFetchesWithoutLeakingBearerURL(t *testing.T) {
	const secretQuery = "token=server-side-only-secret"
	body := []byte("a,b\n1,2\n")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != secretQuery {
			t.Errorf("request query = %q, want %q", r.URL.RawQuery, secretQuery)
		}
		w.Header().Set("Content-Disposition", `attachment; filename="report.csv"`)
		w.Header().Set("Content-Type", "text/csv")
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	ctx, _ := downloadCtx(t)
	raw := srv.URL + "/download?" + secretQuery
	handle := registerDownloadURLHandle(raw)
	res := runDownloadURL(ctx, fsTestSandbox(t), DownloadURLParams{URL: handle})
	if res.Status != downloadStatusSuccess {
		t.Fatalf("download failed: %+v", res)
	}
	if res.URL != handle || res.FinalURL != "" || len(res.RedirectChain) != 0 {
		t.Fatalf("protected result exposed resolved URL metadata: %+v", res)
	}
	if got, err := os.ReadFile(res.SavedTo); err != nil || string(got) != string(body) {
		t.Fatalf("saved bytes = %q, %v; want %q", got, err, body)
	}

	encoded, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), raw) || strings.Contains(string(encoded), "server-side-only-secret") {
		t.Fatalf("tool result leaked bearer URL: %s", encoded)
	}
}

func TestDownloadURL_ExpiredOpaqueHandleFailsClosed(t *testing.T) {
	handle := registerDownloadURLHandle("https://api.fast.io/file?token=expired-secret")
	id := strings.TrimPrefix(handle, downloadURLHandlePrefix)

	downloadURLHandles.Lock()
	entry := downloadURLHandles.entries[id]
	entry.expiresAt = time.Now().Add(-time.Second)
	downloadURLHandles.entries[id] = entry
	downloadURLHandles.Unlock()

	res := runDownloadURL(context.Background(), fsTestSandbox(t), DownloadURLParams{URL: handle})
	if res.Status != downloadStatusError || !strings.Contains(res.Error, "invalid or expired") {
		t.Fatalf("expired handle result = %+v", res)
	}
	if strings.Contains(res.Error, "expired-secret") {
		t.Fatalf("expired handle leaked its former bearer URL: %+v", res)
	}
}

// TestDownloadURL_ProtectedHandleFetchFailureDoesNotLeakURL: the FAILURE path
// must hide the vaulted URL as carefully as the success path does. client.Do
// returns a *url.Error whose Error() prints the full request URL; before
// transportErrForDisplay that string — bearer token included — went straight
// into res.Error → the tool result → the model context on any dial, TLS,
// timeout or redirect failure.
func TestDownloadURL_ProtectedHandleFetchFailureDoesNotLeakURL(t *testing.T) {
	// A server that closes the listener before the request lands → dial error.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
	dead := srv.URL
	srv.Close()

	ctx, _ := downloadCtx(t)
	raw := dead + "/node/read/?token=server-side-only-secret"
	handle := registerDownloadURLHandle(raw)
	res := runDownloadURL(ctx, fsTestSandbox(t), DownloadURLParams{URL: handle})
	if res.Status != downloadStatusError {
		t.Fatalf("expected a failed fetch, got %+v", res)
	}
	if strings.Contains(res.Error, "server-side-only-secret") || strings.Contains(res.Error, dead) {
		t.Fatalf("failed protected fetch leaked the vaulted URL: %q", res.Error)
	}
	if !strings.Contains(res.Error, handle) || !strings.Contains(res.Error, "protected URL") {
		t.Fatalf("error should name the handle and say the URL is protected: %q", res.Error)
	}

	// An unprotected URL keeps the full, actionable error.
	res = runDownloadURL(ctx, fsTestSandbox(t), DownloadURLParams{URL: dead + "/plain"})
	if res.Status != downloadStatusError || !strings.Contains(res.Error, dead) {
		t.Fatalf("unprotected fetch failure should name the URL: %+v", res)
	}
}

// TestProtectFastIODownloadURLs_FailsLoudOnFormatDrift pins the fallback: when
// the upstream response no longer carries the "# download_url" heading the
// section scanner keys off, a URL that is a bearer in its own right (a token=
// / signature= / X-Amz-* query) is vaulted anyway instead of flowing to the
// model, while ordinary URLs — with no query, or with a non-credential query —
// are left alone.
func TestProtectFastIODownloadURLs_FailsLoudOnFormatDrift(t *testing.T) {
	const (
		tokenURL = "https://api.fast.io/current/workspace/123/storage/node/read/?token=secret-bearer-value"
		sigV4URL = "https://bucket.s3.amazonaws.com/obj?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Signature=deadbeef&X-Amz-Credential=AKIA%2F20260905"
		azureURL = "https://acct.blob.core.windows.net/c/b?sv=2022-11-02&sig=abc123"
		web      = "https://elcano.fast.io/workspace/general/preview/node"
		paged    = "https://api.fast.io/current/workspace/123/list?page=2&limit=50"
	)
	// No "# " heading anywhere: the pre-fix scanner returned this verbatim.
	input := "Result: success\nDownload: " + tokenURL + "\nSigned: " + sigV4URL + "\nBlob: " + azureURL + "\nPreview: " + web + "\nNext: " + paged + "\n"

	got := ProtectFastIODownloadURLs(input)
	for _, leaked := range []string{tokenURL, sigV4URL, azureURL, "secret-bearer-value", "deadbeef", "sig=abc123"} {
		if strings.Contains(got, leaked) {
			t.Fatalf("credential-bearing URL leaked past the drift fallback (%q):\n%s", leaked, got)
		}
	}
	for _, kept := range []string{web, paged} {
		if !strings.Contains(got, kept) {
			t.Fatalf("non-credential URL %q should be untouched:\n%s", kept, got)
		}
	}
	if n := strings.Count(got, downloadURLHandlePrefix); n != 3 {
		t.Fatalf("want 3 opaque handles, got %d:\n%s", n, got)
	}
	// Each handle resolves to its bearer URL, so the download_url flow still
	// works end to end for a drifted response.
	for _, line := range strings.Split(got, "\n") {
		i := strings.Index(line, downloadURLHandlePrefix)
		if i < 0 {
			continue
		}
		handle := strings.TrimSpace(line[i:])
		resolved, protected, err := resolveDownloadURLHandle(handle)
		if err != nil || !protected {
			t.Fatalf("resolve %q: (%q, %v, %v)", handle, resolved, protected, err)
		}
		if resolved != tokenURL && resolved != sigV4URL && resolved != azureURL {
			t.Fatalf("handle resolved to an unexpected URL %q", resolved)
		}
	}

	// A response with a heading that DRIFTED (renamed) is covered by the same
	// fallback, and the still-recognized zip_url section keeps its whole-URL
	// vaulting (no credential heuristic needed there).
	drifted := "# file_url\n" + tokenURL + "\n# zip_url\nhttps://cdn.fast.io/opaque/path\n# web_url\n" + web + "\n"
	got = ProtectFastIODownloadURLs(drifted)
	if strings.Contains(got, "secret-bearer-value") || strings.Contains(got, "https://cdn.fast.io/opaque/path") || !strings.Contains(got, web) {
		t.Fatalf("drifted-heading response mis-handled:\n%s", got)
	}
}

func TestHasCredentialQuery(t *testing.T) {
	cases := map[string]bool{
		"https://h/p?token=abc":                        true,
		"https://h/p?TOKEN=abc":                        true,
		"https://h/p?access_token=abc":                 true,
		"https://h/p?signature=abc":                    true,
		"https://h/p?sv=1&sig=abc":                     true,
		"https://h/p?X-Amz-Signature=abc":              true,
		"https://h/p?x-goog-signature=abc":             true,
		"https://h/p":                                  false,
		"https://h/p?page=2&limit=10":                  false,
		"https://h/p?tokenizer=bpe":                    false, // exact key match, not substring
		"https://h/p?q=token":                          false, // credential words in VALUES are not keys
		"https://h/token/signature":                    false, // path, not query
		"https://h/p?%zz=1":                            false, // unparseable
		"https://h/p?utm_source=x&sig=":                true,  // empty value still names the param
		"http://169.254.169.254/latest?token=metadata": true,
	}
	for raw, want := range cases {
		if got := hasCredentialQuery(raw); got != want {
			t.Errorf("hasCredentialQuery(%q) = %v, want %v", raw, got, want)
		}
	}
}
