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
