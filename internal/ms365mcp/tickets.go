package ms365mcp

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	maxAttachmentTickets = 256
	ticketRandomBytes    = 32
)

// AttachmentTicket holds the single-use Graph download target and bearer token
// for an attachment download URL.
type AttachmentTicket struct {
	id          string
	Token       string
	Target      string
	Filename    string
	ContentType string
	ExpiresAt   time.Time
}

// AttachmentTicketStore manages short-lived, single-use attachment download
// tickets in memory.
type AttachmentTicketStore struct {
	mu      sync.Mutex
	tickets []AttachmentTicket
	nowFn   func() time.Time
}

// NewAttachmentTicketStore creates an in-memory single-use ticket store.
func NewAttachmentTicketStore() *AttachmentTicketStore {
	return &AttachmentTicketStore{
		nowFn: time.Now,
	}
}

func (s *AttachmentTicketStore) now() time.Time {
	if s != nil && s.nowFn != nil {
		return s.nowFn()
	}
	return time.Now()
}

func (s *AttachmentTicketStore) sweepLocked(now time.Time) {
	kept := s.tickets[:0]
	for _, t := range s.tickets {
		if now.Before(t.ExpiresAt) {
			kept = append(kept, t)
		}
	}
	s.tickets = kept
}

// Issue mints a single-use ticket for a relative Microsoft Graph path (such as
// /me/messages/{id}/attachments/{id}/$value).
func (s *AttachmentTicketStore) Issue(token, target string, ttlSeconds int, filename, contentType string) (string, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return "", fmt.Errorf("issue requires a non-empty access token")
	}
	if !isValidGraphTargetPath(target) {
		return "", fmt.Errorf("issue requires a relative Graph path starting with /")
	}
	if ttlSeconds <= 0 {
		ttlSeconds = defaultAttachmentURLTTLSeconds
	}
	ttlSeconds = min(max(ttlSeconds, minAttachmentURLTTLSeconds), maxAttachmentURLTTLSeconds)

	raw := make([]byte, ticketRandomBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate ticket id: %w", err)
	}
	id := base64.RawURLEncoding.EncodeToString(raw)

	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()

	s.sweepLocked(now)
	for len(s.tickets) >= maxAttachmentTickets {
		s.tickets = s.tickets[1:]
	}
	s.tickets = append(s.tickets, AttachmentTicket{
		id:          id,
		Token:       token,
		Target:      target,
		Filename:    strings.TrimSpace(filename),
		ContentType: strings.TrimSpace(contentType),
		ExpiresAt:   now.Add(time.Duration(ttlSeconds) * time.Second),
	})
	return id, nil
}

// Redeem atomically looks up and consumes a single-use attachment ticket.
func (s *AttachmentTicketStore) Redeem(id string) (AttachmentTicket, bool) {
	id = strings.TrimSpace(id)
	if id == "" {
		return AttachmentTicket{}, false
	}
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()

	s.sweepLocked(now)
	for i, t := range s.tickets {
		if t.id == id {
			s.tickets = append(s.tickets[:i], s.tickets[i+1:]...)
			return t, true
		}
	}
	return AttachmentTicket{}, false
}

var encodedTraversalOrSlashRe = regexp.MustCompile(`(?i)%2e|%2f|%5c`)

func isValidGraphTargetPath(target string) bool {
	if target == "" || !strings.HasPrefix(target, "/") || strings.HasPrefix(target, "//") {
		return false
	}
	if strings.Contains(target, "://") || strings.Contains(target, `\`) {
		return false
	}
	if strings.ContainsAny(target, "\r\n\x00") {
		return false
	}
	if encodedTraversalOrSlashRe.MatchString(target) {
		return false
	}
	pathPart := target
	if q := strings.IndexByte(pathPart, '?'); q >= 0 {
		pathPart = pathPart[:q]
	}
	for _, seg := range strings.Split(pathPart, "/") {
		if seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

func buildContentDisposition(filename string) string {
	var cleanBuilder strings.Builder
	for _, r := range filename {
		if r < 0x20 || r == 0x7f || r == '"' || r == '\\' {
			cleanBuilder.WriteByte('_')
		} else {
			cleanBuilder.WriteRune(r)
		}
	}
	clean := strings.TrimSpace(cleanBuilder.String())
	if clean == "" {
		return "attachment"
	}
	var asciiBuilder strings.Builder
	allASCII := true
	for _, r := range clean {
		if r > 0x7e {
			allASCII = false
			asciiBuilder.WriteByte('_')
		} else {
			asciiBuilder.WriteRune(r)
		}
	}
	asciiFallback := asciiBuilder.String()
	if allASCII {
		return fmt.Sprintf(`attachment; filename="%s"`, asciiFallback)
	}
	encoded := url.PathEscape(clean)
	return fmt.Sprintf(`attachment; filename="%s"; filename*=UTF-8''%s`, asciiFallback, encoded)
}

func inferFilenameFromTarget(target string) string {
	pathPart := target
	if q := strings.IndexByte(pathPart, '?'); q >= 0 {
		pathPart = pathPart[:q]
	}
	parts := strings.Split(pathPart, "/")
	for i := len(parts) - 1; i >= 0; i-- {
		seg := strings.TrimSpace(parts[i])
		if seg == "" || strings.EqualFold(seg, "$value") || strings.EqualFold(seg, "content") {
			continue
		}
		if decoded, err := url.PathUnescape(seg); err == nil && decoded != "" {
			return decoded
		}
		return seg
	}
	return ""
}

func (h *Handler) handleAttachment(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	ticketID := strings.TrimSpace(r.URL.Query().Get("t"))
	if ticketID == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"error":             "invalid_ticket",
			"error_description": "Missing attachment download ticket",
		})
		return
	}
	if h.cfg.AttachmentProxy != nil {
		h.cfg.AttachmentProxy.ServeHTTP(w, r)
		return
	}
	ticket, ok := h.tickets.Redeem(ticketID)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"error":             "invalid_ticket",
			"error_description": "Invalid or expired attachment download ticket",
		})
		return
	}

	upstreamURL := strings.TrimRight(h.cfg.GraphBaseURL, "/") + ticket.Target
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, upstreamURL, nil) //nolint:gosec // upstreamURL is pinned to configured GraphBaseURL and validated ticket target.
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error":             "upstream_fetch_failed",
			"error_description": err.Error(),
		})
		return
	}
	req.Header.Set("Authorization", "Bearer "+ticket.Token)

	resp, err := h.streamHTTPClient().Do(req) //nolint:gosec // upstreamURL is pinned to configured GraphBaseURL and validated ticket target.
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error":             "upstream_fetch_failed",
			"error_description": err.Error(),
		})
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		errDesc := strings.TrimSpace(string(bodyBytes))
		if errDesc == "" {
			errDesc = resp.Status
		}
		writeJSON(w, resp.StatusCode, map[string]any{
			"error":             "upstream_error",
			"status":            resp.StatusCode,
			"error_description": errDesc,
		})
		return
	}

	upstreamCT := strings.TrimSpace(resp.Header.Get("Content-Type"))
	if upstreamCT == "" {
		upstreamCT = "application/octet-stream"
	}
	baseCT := strings.ToLower(strings.TrimSpace(strings.Split(upstreamCT, ";")[0]))
	isGenericUpstream := baseCT == "" || baseCT == "application/octet-stream"
	contentType := upstreamCT
	if isGenericUpstream && ticket.ContentType != "" && !strings.EqualFold(ticket.ContentType, "application/octet-stream") {
		contentType = ticket.ContentType
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		w.Header().Set("Content-Length", cl)
	}

	upstreamCD := resp.Header.Get("Content-Disposition")
	switch {
	case ticket.Filename != "":
		w.Header().Set("Content-Disposition", buildContentDisposition(ticket.Filename))
	case upstreamCD != "":
		w.Header().Set("Content-Disposition", upstreamCD)
	default:
		if inferred := inferFilenameFromTarget(ticket.Target); inferred != "" {
			w.Header().Set("Content-Disposition", buildContentDisposition(inferred))
		} else {
			w.Header().Set("Content-Disposition", "attachment")
		}
	}

	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

// AttachmentOnlyHandler wraps h so only GET /attachment requests are served.
// Used on the credential-owning mcp-broker child's private Unix domain socket.
func AttachmentOnlyHandler(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL == nil || r.URL.Path != "/attachment" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// NewUnixSocketAttachmentProxy returns an http.Handler that forwards token-free
// GET /attachment?t=<ticket> requests over a private Unix domain socket to the
// credential-owning mcp-broker process and streams the response back.
func NewUnixSocketAttachmentProxy(socketPath string) http.Handler {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return dialer.DialContext(ctx, "unix", socketPath)
			},
			ResponseHeaderTimeout: 30 * time.Second,
			IdleConnTimeout:       90 * time.Second,
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ticketID := strings.TrimSpace(r.URL.Query().Get("t"))
		if ticketID == "" {
			writeJSON(w, http.StatusUnauthorized, map[string]any{
				"error":             "invalid_ticket",
				"error_description": "Missing attachment download ticket",
			})
			return
		}
		targetURL := "http://unix/attachment?t=" + url.QueryEscape(ticketID)
		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, targetURL, nil) //nolint:gosec // targetURL dials the fixed broker Unix domain socket.
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{
				"error":             "upstream_fetch_failed",
				"error_description": err.Error(),
			})
			return
		}
		resp, err := client.Do(req) //nolint:gosec // targetURL dials the fixed broker Unix domain socket.
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{
				"error":             "upstream_fetch_failed",
				"error_description": err.Error(),
			})
			return
		}
		defer resp.Body.Close()

		for _, hdr := range []string{"Content-Type", "Content-Disposition", "Content-Length", "Cache-Control", "X-Content-Type-Options"} {
			if val := resp.Header.Get(hdr); val != "" {
				w.Header().Set(hdr, val)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	})
}
