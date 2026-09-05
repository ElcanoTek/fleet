package tools

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"log"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Fast.io download responses contain short-lived bearer URLs. Sending those
// URLs through the normal tool-result path is unsafe (they grant direct file
// access), but redacting the token makes the documented
// mcp_fast_io_download -> download_url flow unusable. Keep the real URL in a
// process-local vault and expose only an unguessable handle to the model.
const (
	downloadURLHandlePrefix     = "fleet-download://"
	downloadURLHandleTTL        = 2 * time.Hour
	downloadURLHandleMaxEntries = 4096
)

type downloadURLHandleEntry struct {
	url       string
	expiresAt time.Time
}

var downloadURLHandles = struct {
	sync.Mutex
	entries map[string]downloadURLHandleEntry
}{entries: make(map[string]downloadURLHandleEntry)}

var httpURLInLine = regexp.MustCompile(`https?://[^\s<>"']+`)

// downloadURLDriftOnce rate-limits the format-drift warning to once per
// process: the condition is a property of the upstream response shape, not of
// one call, and the message is the same every time.
var downloadURLDriftOnce sync.Once

// ProtectFastIODownloadURLs replaces bearer URLs in the download_url/zip_url
// sections of a successful Fast.io MCP response with process-local opaque
// handles. Call this before secret redaction and before the response is logged,
// streamed, or returned to the model.
//
// The function keys off the response section rather than a host allowlist:
// Fast.io may return its own API host, a CDN, or object storage. The caller
// already establishes provenance by invoking this only for the trusted
// mcp_fast_io_download tool.
//
// The section headings are an UPSTREAM contract fleet does not control, so
// this must not fail open when they drift (a renamed heading, a dropped "# ",
// a URL folded into prose): a URL carrying a credential-looking query parameter
// (see hasCredentialQuery) is vaulted wherever it appears, and the drift is
// logged once so the heading list gets fixed. The cost of a false positive is a
// handle the model has to pass to download_url instead of a clickable link; the
// cost of a false negative is a bearer in the model context, the SSE stream and
// the turn log — so the fallback errs toward vaulting.
func ProtectFastIODownloadURLs(text string) string {
	if text == "" {
		return text
	}

	lines := strings.Split(text, "\n")
	protectSection := false
	protected := make(map[string]string)
	changed := false
	drift := false

	vault := func(raw string) string {
		if handle, ok := protected[raw]; ok {
			return handle
		}
		handle := registerDownloadURLHandle(raw)
		protected[raw] = handle
		changed = true
		return handle
	}

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "# ") {
			section := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(trimmed, "# ")))
			protectSection = section == "download_url" || section == "zip_url"
			continue
		}
		if protectSection {
			lines[i] = httpURLInLine.ReplaceAllStringFunc(line, vault)
			continue
		}
		// Outside a protected section: vault anyway when the URL itself says it
		// is a bearer (fail loud, not open).
		lines[i] = httpURLInLine.ReplaceAllStringFunc(line, func(raw string) string {
			if !hasCredentialQuery(raw) {
				return raw
			}
			drift = true
			return vault(raw)
		})
	}

	if drift {
		downloadURLDriftOnce.Do(func() {
			// No URL, no handle: the message is about the shape, and the bearer
			// is exactly what must not reach a log line.
			log.Printf("download_url: a Fast.io response carried a credential-bearing URL outside a download_url/zip_url section; vaulted it anyway — the upstream response format may have changed, check the section headings ProtectFastIODownloadURLs expects")
		})
	}
	if !changed {
		return text
	}
	return strings.Join(lines, "\n")
}

// credentialQueryParams are the query-parameter names (lower-cased) that mark
// a URL as a bearer in its own right: the generic token/signature spellings,
// plus the SigV4 (X-Amz-*) and GCS (X-Goog-*) presigned-URL families, matched
// by prefix. A URL that carries one is a credential wherever it appears.
var credentialQueryParams = map[string]bool{
	"token":        true,
	"access_token": true,
	"signature":    true,
	"sig":          true,
}

var credentialQueryPrefixes = []string{"x-amz-", "x-goog-"}

// hasCredentialQuery reports whether raw is an http(s) URL whose query string
// names a credential-looking parameter. Unparseable URLs are not credentials
// (they would not fetch either).
func hasCredentialQuery(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.RawQuery == "" {
		return false
	}
	for key := range u.Query() {
		k := strings.ToLower(key)
		if credentialQueryParams[k] {
			return true
		}
		for _, prefix := range credentialQueryPrefixes {
			if strings.HasPrefix(k, prefix) {
				return true
			}
		}
	}
	return false
}

func registerDownloadURLHandle(rawURL string) string {
	idBytes := make([]byte, 24)
	if _, err := rand.Read(idBytes); err != nil {
		// crypto/rand failure is exceptionally unlikely. Hashing the already
		// high-entropy signed URL with a timestamp still avoids returning the
		// bearer credential to the model if the host entropy source fails.
		sum := sha256.Sum256([]byte(fmt.Sprintf("%s:%d", rawURL, time.Now().UnixNano())))
		idBytes = sum[:24]
	}
	id := base64.RawURLEncoding.EncodeToString(idBytes)
	now := time.Now()

	downloadURLHandles.Lock()
	defer downloadURLHandles.Unlock()
	pruneDownloadURLHandlesLocked(now)
	downloadURLHandles.entries[id] = downloadURLHandleEntry{
		url:       rawURL,
		expiresAt: now.Add(downloadURLHandleTTL),
	}
	return downloadURLHandlePrefix + id
}

// resolveDownloadURLHandle returns raw unchanged when it is an ordinary URL.
// A fleet-download handle is fail-closed: unknown/expired handles never reach
// the HTTP client as a malformed URL and never reveal vault contents.
func resolveDownloadURLHandle(raw string) (resolved string, protected bool, err error) {
	if !strings.HasPrefix(raw, downloadURLHandlePrefix) {
		return raw, false, nil
	}

	id := strings.TrimPrefix(raw, downloadURLHandlePrefix)
	if id == "" || strings.ContainsAny(id, "/?#") {
		return "", true, fmt.Errorf("download handle is invalid or expired; call mcp_fast_io_download again to generate a fresh handle")
	}

	now := time.Now()
	downloadURLHandles.Lock()
	defer downloadURLHandles.Unlock()
	entry, ok := downloadURLHandles.entries[id]
	if !ok || !entry.expiresAt.After(now) {
		delete(downloadURLHandles.entries, id)
		return "", true, fmt.Errorf("download handle is invalid or expired; call mcp_fast_io_download again to generate a fresh handle")
	}
	return entry.url, true, nil
}

func pruneDownloadURLHandlesLocked(now time.Time) {
	for id, entry := range downloadURLHandles.entries {
		if !entry.expiresAt.After(now) {
			delete(downloadURLHandles.entries, id)
		}
	}
	if len(downloadURLHandles.entries) < downloadURLHandleMaxEntries {
		return
	}

	// Bound memory even under a burst of abandoned runs. Expiry times track
	// insertion order closely, so evicting the earliest entry preserves the
	// freshest handles.
	var oldestID string
	var oldestExpiry time.Time
	for id, entry := range downloadURLHandles.entries {
		if oldestID == "" || entry.expiresAt.Before(oldestExpiry) {
			oldestID = id
			oldestExpiry = entry.expiresAt
		}
	}
	delete(downloadURLHandles.entries, oldestID)
}
