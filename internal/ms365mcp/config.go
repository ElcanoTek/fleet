// Package ms365mcp implements Fleet's built-in Microsoft 365 OAuth 2.0 proxy
// and stateless Streamable HTTP Graph MCP server.
package ms365mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/config/envreg"
	"github.com/ElcanoTek/fleet/internal/httpmw"
)

const (
	defaultTenantID                = "common"
	defaultAttachmentURLTTLSeconds = 300
	minAttachmentURLTTLSeconds     = 1
	maxAttachmentURLTTLSeconds     = 3600
	defaultGraphBaseURL            = "https://graph.microsoft.com/v1.0"
	defaultAuthBaseURL             = "https://login.microsoftonline.com"
)

// DefaultAllowedScopes lists the delegated Microsoft Graph and OpenID scopes
// requested during the /authorize redirect when the caller omits or requests
// non-Graph scopes.
var DefaultAllowedScopes = []string{
	"Calendars.Read",
	"Calendars.ReadWrite",
	"Channel.ReadBasic.All",
	"ChannelMessage.Read.All",
	"ChannelMessage.Send",
	"Chat.Create",
	"Chat.Read",
	"Chat.ReadWrite",
	"ChatMember.Read",
	"ChatMessage.Send",
	"Contacts.Read",
	"Contacts.ReadWrite",
	"Files.Read",
	"Files.Read.All",
	"Files.ReadWrite",
	"Mail.Read",
	"Mail.ReadWrite",
	"Mail.Send",
	"MailboxSettings.Read",
	"People.Read",
	"Sites.Read.All",
	"Tasks.Read",
	"Tasks.ReadWrite",
	"Team.ReadBasic.All",
	"TeamMember.Read.All",
	"User.Read",
	"offline_access",
	"openid",
	"profile",
}

// Config holds the runtime configuration for the built-in Microsoft 365 OAuth
// and MCP surface.
type Config struct {
	ClientID                string
	TenantID                string
	ClientSecret            string
	PublicURL               string
	Aliases                 []string
	AllowedRedirectURIs     []string
	AttachmentURLTTLSeconds int
	TrustedProxies          httpmw.TrustedProxies

	// When true (set on the parent web server when the credential-owning
	// mcp-broker child process is active), unauthenticated POST /mcp probes
	// still return 401 + WWW-Authenticate for RFC 9728 discovery, while
	// bearer-authenticated POST /mcp requests are refused so Graph tokens never
	// execute in the parent process.
	DisableAuthenticatedMCP bool

	// Optional token-free proxy for GET /attachment?t=<ticket> that forwards
	// ticket redemption to the credential-owning mcp-broker child process over a
	// private Unix domain socket.
	AttachmentProxy http.Handler

	// Optional bundle-supplied tool catalog JSON (loaded from
	// <FLEET_CLIENT_CONFIG_DIR>/ms365_tools.json when present); falls back to
	// the default Microsoft Graph v1.0 catalog when empty.
	CatalogJSON []byte
	// Non-nil when <FLEET_CLIENT_CONFIG_DIR>/ms365_tools.json exists (or stat
	// fails with a non-ENOENT error) and cannot be read or parsed; causes
	// Enabled() to fail closed.
	CatalogLoadError error

	// Test seams (defaulted automatically when empty/nil).
	GraphBaseURL     string
	AuthBaseURL      string
	HTTPClient       *http.Client
	StreamHTTPClient *http.Client
}

func newDefaultStreamHTTPClient() *http.Client {
	return &http.Client{
		// Deliberately no whole-exchange Timeout so large /attachment downloads
		// can stream for as long as they take; connection, TLS, and response
		// header phases remain bounded on the transport.
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   10 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
		},
	}
}

// ConfigFromEnv builds the parent-safe Config from registered environment
// variables and the loaded Fleet config without reading MS365_MCP_CLIENT_SECRET.
func ConfigFromEnv(fleetCfg *config.Config) Config {
	clientID := strings.TrimSpace(envreg.Get("MS365_MCP_CLIENT_ID"))
	tenantID := strings.TrimSpace(envreg.Get("MS365_MCP_TENANT_ID"))
	if tenantID == "" {
		tenantID = defaultTenantID
	}

	var fleetPublicURL string
	if fleetCfg != nil {
		fleetPublicURL = trimBaseURL(fleetCfg.PublicBaseURL)
	}
	if fleetPublicURL == "" {
		fleetPublicURL = trimBaseURL(envreg.Get("FLEET_PUBLIC_BASE_URL"))
	}
	publicURL := trimBaseURL(envreg.Get("MS365_MCP_PUBLIC_URL"))
	if publicURL == "" {
		publicURL = fleetPublicURL
	}

	var aliases []string
	for _, raw := range strings.Split(envreg.Get("MS365_MCP_ALIASES"), ",") {
		if trimmed := trimBaseURL(raw); trimmed != "" {
			aliases = appendUniqueString(aliases, trimmed)
		}
	}

	var redirectURIs []string
	if publicURL != "" {
		redirectURIs = appendUniqueString(redirectURIs, publicURL+"/api/oauth/mcp/callback")
	}
	if fleetPublicURL != "" {
		redirectURIs = appendUniqueString(redirectURIs, fleetPublicURL+"/api/oauth/mcp/callback")
	}
	for _, alias := range aliases {
		redirectURIs = appendUniqueString(redirectURIs, alias+"/api/oauth/mcp/callback")
	}
	for _, raw := range strings.Split(envreg.Get("MS365_MCP_ALLOWED_REDIRECT_URIS"), ",") {
		if trimmed := strings.TrimSpace(raw); trimmed != "" {
			redirectURIs = appendUniqueString(redirectURIs, trimmed)
		}
	}

	ttlSeconds := defaultAttachmentURLTTLSeconds
	if rawTTL := strings.TrimSpace(envreg.Get("MS365_MCP_ATTACHMENT_URL_TTL_S")); rawTTL != "" {
		if parsed, err := strconv.Atoi(rawTTL); err == nil && parsed > 0 {
			ttlSeconds = min(max(parsed, minAttachmentURLTTLSeconds), maxAttachmentURLTTLSeconds)
		}
	}

	catalogJSON, catalogErr := LoadCatalogOverrideFromEnv()

	var trustedIPs []net.IP
	if fleetCfg != nil && len(fleetCfg.TrustedProxies) > 0 {
		trustedIPs = fleetCfg.TrustedProxies
	} else if rawTP := strings.TrimSpace(envreg.Get("FLEET_TRUSTED_PROXIES")); rawTP != "" {
		for _, part := range strings.Split(rawTP, ",") {
			if ip := net.ParseIP(strings.TrimSpace(part)); ip != nil {
				trustedIPs = append(trustedIPs, ip)
			}
		}
	}

	return Config{
		ClientID:                clientID,
		TenantID:                tenantID,
		PublicURL:               publicURL,
		Aliases:                 aliases,
		AllowedRedirectURIs:     redirectURIs,
		AttachmentURLTTLSeconds: ttlSeconds,
		TrustedProxies:          httpmw.NewTrustedProxies(trustedIPs),
		CatalogJSON:             catalogJSON,
		CatalogLoadError:        catalogErr,
		GraphBaseURL:            defaultGraphBaseURL,
		AuthBaseURL:             defaultAuthBaseURL,
		HTTPClient:              &http.Client{Timeout: 30 * time.Second},
		StreamHTTPClient:        newDefaultStreamHTTPClient(),
	}
}

// LoadCatalogOverrideFromEnv reads and validates the optional
// <FLEET_CLIENT_CONFIG_DIR>/ms365_tools.json catalog override. When the file
// does not exist, it returns (nil, nil). When the file exists (or stat fails
// with a non-ENOENT error) and cannot be read or parsed, it returns a non-nil
// error so callers fail closed.
func LoadCatalogOverrideFromEnv() ([]byte, error) {
	clientDir := strings.TrimSpace(envreg.Get("FLEET_CLIENT_CONFIG_DIR"))
	if clientDir == "" {
		return nil, nil
	}
	overridePath := filepath.Clean(filepath.Join(clientDir, "ms365_tools.json"))
	if _, statErr := os.Stat(overridePath); statErr == nil {
		b, readErr := os.ReadFile(overridePath)
		switch {
		case readErr != nil:
			return nil, fmt.Errorf("read %s: %w", overridePath, readErr)
		case len(bytes.TrimSpace(b)) == 0:
			return nil, fmt.Errorf("read %s: empty catalog file", overridePath)
		default:
			var probe []ToolSpec
			if unmarshalErr := json.Unmarshal(b, &probe); unmarshalErr != nil {
				return nil, fmt.Errorf("parse %s: %w", overridePath, unmarshalErr)
			}
			return b, nil
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return nil, fmt.Errorf("stat %s: %w", overridePath, statErr)
	}
	return nil, nil
}

// BrokerConfigFromEnv builds the credential-owning broker's Config, including
// the optional MS365_MCP_CLIENT_SECRET.
func BrokerConfigFromEnv(fleetCfg *config.Config) Config {
	cfg := ConfigFromEnv(fleetCfg)
	cfg.ClientSecret = strings.TrimSpace(envreg.Get("MS365_MCP_CLIENT_SECRET"))
	return cfg
}

// Enabled reports whether the built-in Microsoft 365 MCP surface has active
// Entra credentials (from env or workspace DB settings) and a valid catalog.
func (c Config) Enabled() bool {
	if c.CatalogLoadError != nil {
		return false
	}
	_, _, _, ok := c.EffectiveCredentials(context.Background())
	return ok
}

// CanMount reports whether the built-in Microsoft 365 routes and in-process
// transport should be wired at server startup so that either boot-time env
// credentials or live workspace DB credentials can activate the connector
// without a service restart.
func (c Config) CanMount() bool {
	if c.CatalogLoadError != nil {
		return false
	}
	return strings.TrimSpace(c.ClientID) != "" || strings.TrimSpace(c.PublicURL) != ""
}

// EffectiveCredentials resolves the active Microsoft Entra ClientID and
// TenantID, preferring Config.ClientID (environment) and falling back to the
// registered workspace DB credentials provider. source is "env", "db", or "none".
func (c Config) EffectiveCredentials(ctx context.Context) (clientID, tenantID, source string, ok bool) {
	if cid, tid, dynOK := lookupDynamicCredentials(ctx); dynOK && cid != "" {
		if tid == "" {
			tid = defaultTenantID
		}
		return cid, tid, "db", true
	}
	if cid := strings.TrimSpace(c.ClientID); cid != "" {
		tid := strings.TrimSpace(c.TenantID)
		if tid == "" {
			tid = defaultTenantID
		}
		return cid, tid, "env", true
	}
	return "", defaultTenantID, "none", false
}

const dynamicCredsCacheTTL = 2 * time.Second

var (
	dynCredsMu       sync.RWMutex
	dynCredsProvider func(ctx context.Context) (clientID, tenantID string, ok bool)
	dynCachedClient  string
	dynCachedTenant  string
	dynCachedOK      bool
	dynCachedAt      time.Time
)

// RegisterCredentialsProvider registers (or clears when nil) the dynamic
// workspace credentials lookup (typically backed by store.MS365MCPConfig).
func RegisterCredentialsProvider(fn func(ctx context.Context) (clientID, tenantID string, ok bool)) {
	dynCredsMu.Lock()
	dynCredsProvider = fn
	dynCachedClient = ""
	dynCachedTenant = ""
	dynCachedOK = false
	dynCachedAt = time.Time{}
	dynCredsMu.Unlock()
}

// SetCachedCredentials immediately updates the in-memory dynamic credentials
// cache after an admin save or reset so subsequent requests in the same
// process see the new Entra app settings with zero latency.
func SetCachedCredentials(clientID, tenantID string) {
	cid := strings.TrimSpace(clientID)
	tid := strings.TrimSpace(tenantID)
	if tid == "" {
		tid = defaultTenantID
	}
	dynCredsMu.Lock()
	dynCachedClient = cid
	dynCachedTenant = tid
	dynCachedOK = cid != ""
	if cid != "" {
		dynCachedAt = time.Now()
	} else {
		dynCachedAt = time.Time{}
	}
	dynCredsMu.Unlock()
}

func lookupDynamicCredentials(ctx context.Context) (string, string, bool) {
	now := time.Now()
	dynCredsMu.RLock()
	prov := dynCredsProvider
	if dynCachedOK && !dynCachedAt.IsZero() && now.Sub(dynCachedAt) < dynamicCredsCacheTTL {
		cid, tid, ok := dynCachedClient, dynCachedTenant, dynCachedOK
		dynCredsMu.RUnlock()
		return cid, tid, ok
	}
	dynCredsMu.RUnlock()
	if prov == nil {
		return "", "", false
	}
	if ctx == nil {
		ctx = context.Background()
	}
	cid, tid, ok := prov(ctx)
	cid = strings.TrimSpace(cid)
	tid = strings.TrimSpace(tid)
	if tid == "" {
		tid = defaultTenantID
	}
	ok = ok && cid != ""
	dynCredsMu.Lock()
	dynCachedClient = cid
	dynCachedTenant = tid
	dynCachedOK = ok
	if ok {
		dynCachedAt = time.Now()
	} else {
		dynCachedAt = time.Time{}
	}
	dynCredsMu.Unlock()
	return cid, tid, ok
}

// IsMS365Path reports whether path is one of the HTTP paths served by the
// Microsoft 365 OAuth and MCP surface.
func IsMS365Path(path string) bool {
	switch path {
	case "/.well-known/oauth-authorization-server",
		"/.well-known/oauth-protected-resource",
		"/register",
		"/authorize",
		"/token",
		"/mcp",
		"/attachment":
		return true
	}
	return strings.HasPrefix(path, "/.well-known/oauth-protected-resource/")
}

// MatchesInProcessURL reports whether an outbound request to u targets this
// Fleet instance's own Microsoft 365 OAuth/MCP surface (via PublicURL or any
// configured alias in Aliases) and should be handled in-process.
func (c Config) MatchesInProcessURL(u *url.URL) bool {
	if u == nil || !IsMS365Path(u.Path) {
		return false
	}
	targetOrigin := canonicalOrigin(u)
	if targetOrigin == "" {
		return false
	}
	matchedOrigin := false
	if c.PublicURL != "" {
		if pub, err := url.Parse(c.PublicURL); err == nil && canonicalOrigin(pub) == targetOrigin {
			matchedOrigin = true
		}
	}
	if !matchedOrigin {
		for _, alias := range c.Aliases {
			if parsed, err := url.Parse(alias); err == nil && canonicalOrigin(parsed) == targetOrigin {
				matchedOrigin = true
				break
			}
		}
	}
	if !matchedOrigin {
		return false
	}
	return c.Enabled()
}

func (c Config) baseURLForRequest(r *http.Request) string {
	reqOrigin := requestOrigin(r)
	if reqOrigin != "" {
		if reqURL, err := url.Parse(reqOrigin); err == nil {
			reqCanon := canonicalOrigin(reqURL)
			for _, alias := range c.Aliases {
				if parsed, err := url.Parse(alias); err == nil && canonicalOrigin(parsed) == reqCanon {
					return alias
				}
			}
		}
	}
	if c.PublicURL != "" {
		return c.PublicURL
	}
	if reqOrigin != "" {
		return reqOrigin
	}
	return "http://localhost:3000"
}

func requestOrigin(r *http.Request) string {
	if r == nil {
		return ""
	}
	if r.URL != nil && (r.URL.Scheme == "http" || r.URL.Scheme == "https") && r.URL.Host != "" {
		return r.URL.Scheme + "://" + r.URL.Host
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if xfProto := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0]); xfProto == "https" || xfProto == "http" {
		scheme = xfProto
	}
	host := strings.TrimSpace(r.Host)
	if host == "" && r.URL != nil {
		host = strings.TrimSpace(r.URL.Host)
	}
	if host == "" {
		return ""
	}
	return scheme + "://" + host
}

func trimBaseURL(s string) string {
	return strings.TrimRight(strings.TrimSpace(s), "/")
}

func canonicalOrigin(u *url.URL) string {
	if u == nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	scheme := strings.ToLower(strings.TrimSpace(u.Scheme))
	host := strings.ToLower(strings.TrimSpace(u.Host))
	if h, p, err := net.SplitHostPort(host); err == nil {
		if (scheme == "https" && p == "443") || (scheme == "http" && p == "80") {
			host = h
		}
	}
	return scheme + "://" + host
}

func appendUniqueString(list []string, item string) []string {
	for _, existing := range list {
		if existing == item {
			return list
		}
	}
	return append(list, item)
}
