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
	maxAttachmentTickets = 4096
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
	if !isAllowedTicketTargetPath(target) {
		return "", fmt.Errorf("issue requires a valid relative Graph attachment $value or driveItem content path")
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
	if len(s.tickets) >= maxAttachmentTickets {
		return "", fmt.Errorf("too many active attachment download tickets; please retry shortly")
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

func isAllowedTicketTargetPath(target string) bool {
	if !isValidGraphTargetPath(target) {
		return false
	}
	pathPart := target
	if q := strings.IndexByte(pathPart, '?'); q >= 0 {
		pathPart = pathPart[:q]
	}
	return mailAttachmentValueTargetRe.MatchString(pathPart) ||
		eventAttachmentValueTargetRe.MatchString(pathPart) ||
		driveItemContentTargetRe.MatchString(pathPart)
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

// AttachmentOnlyHandler wraps h so only /attachment, /authorize, and /token
// requests are served on the credential-owning mcp-broker child's private Unix
// domain socket.
func AttachmentOnlyHandler(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL == nil {
			http.NotFound(w, r)
			return
		}
		switch r.URL.Path {
		case "/attachment":
			if r.Method != http.MethodGet {
				w.Header().Set("Allow", http.MethodGet)
				http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
				return
			}
		case "/authorize":
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
				return
			}
			if ip := strings.TrimSpace(r.Header.Get("X-Ms365-Client-IP")); ip != "" {
				r = r.Clone(r.Context())
				r.RemoteAddr = net.JoinHostPort(ip, "0")
			}
		case "/token":
			if r.Method != http.MethodPost {
				http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
				return
			}
		default:
			http.NotFound(w, r)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// NewUnixSocketAttachmentProxy returns an http.Handler that forwards
// /attachment, /authorize, and /token requests over a private Unix domain
// socket to the credential-owning mcp-broker process.
func NewUnixSocketAttachmentProxy(socketPath string) http.Handler {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return dialer.DialContext(ctx, "unix", socketPath)
			},
			ResponseHeaderTimeout: 30 * time.Second,
			IdleConnTimeout:       90 * time.Second,
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var targetURL string
		switch r.URL.Path {
		case "/attachment":
			ticketID := strings.TrimSpace(r.URL.Query().Get("t"))
			if ticketID == "" {
				writeJSON(w, http.StatusUnauthorized, map[string]any{
					"error":             "invalid_ticket",
					"error_description": "Missing attachment download ticket",
				})
				return
			}
			targetURL = "http://unix/attachment?t=" + url.QueryEscape(ticketID)
		case "/authorize", "/token":
			targetURL = "http://unix" + r.URL.Path
			if r.URL.RawQuery != "" {
				targetURL += "?" + r.URL.RawQuery
			}
		default:
			http.NotFound(w, r)
			return
		}
		var body io.Reader
		if r.Method == http.MethodPost && r.Body != nil {
			body = io.LimitReader(r.Body, 1<<20)
		}
		req, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL, body) //nolint:gosec // targetURL dials the fixed broker Unix domain socket.
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{
				"error":             "upstream_fetch_failed",
				"error_description": err.Error(),
			})
			return
		}
		for _, hdr := range []string{"Content-Type", "X-Ms365-Client-IP"} {
			if val := r.Header.Get(hdr); val != "" {
				req.Header.Set(hdr, val)
			}
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

		for _, hdr := range []string{"Location", "Content-Type", "Content-Disposition", "Content-Length", "Cache-Control", "X-Content-Type-Options"} {
			if val := resp.Header.Get(hdr); val != "" {
				w.Header().Set(hdr, val)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	})
}
