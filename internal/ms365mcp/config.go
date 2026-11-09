// Package ms365mcp implements Fleet's built-in Microsoft 365 OAuth 2.0 proxy
// and stateless Streamable HTTP Graph MCP server.
package ms365mcp

import (
	"bytes"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

// ConfigFromEnv builds Config from registered environment variables and the
// loaded Fleet config.
func ConfigFromEnv(fleetCfg *config.Config) Config {
	clientID := strings.TrimSpace(envreg.Get("MS365_MCP_CLIENT_ID"))
	tenantID := strings.TrimSpace(envreg.Get("MS365_MCP_TENANT_ID"))
	if tenantID == "" {
		tenantID = defaultTenantID
	}
	clientSecret := strings.TrimSpace(envreg.Get("MS365_MCP_CLIENT_SECRET"))

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

	var catalogJSON []byte
	if clientDir := strings.TrimSpace(envreg.Get("FLEET_CLIENT_CONFIG_DIR")); clientDir != "" {
		overridePath := filepath.Clean(filepath.Join(clientDir, "ms365_tools.json"))
		if b, err := os.ReadFile(overridePath); err == nil && len(bytes.TrimSpace(b)) > 0 {
			catalogJSON = b
		}
	}

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
		ClientSecret:            clientSecret,
		PublicURL:               publicURL,
		Aliases:                 aliases,
		AllowedRedirectURIs:     redirectURIs,
		AttachmentURLTTLSeconds: ttlSeconds,
		TrustedProxies:          httpmw.NewTrustedProxies(trustedIPs),
		CatalogJSON:             catalogJSON,
		GraphBaseURL:            defaultGraphBaseURL,
		AuthBaseURL:             defaultAuthBaseURL,
		HTTPClient:              &http.Client{Timeout: 30 * time.Second},
		StreamHTTPClient:        newDefaultStreamHTTPClient(),
	}
}

// Enabled reports whether the built-in Microsoft 365 MCP surface is active.
func (c Config) Enabled() bool {
	return strings.TrimSpace(c.ClientID) != ""
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
	if !c.Enabled() || u == nil || !IsMS365Path(u.Path) {
		return false
	}
	targetOrigin := canonicalOrigin(u)
	if targetOrigin == "" {
		return false
	}
	if c.PublicURL != "" {
		if pub, err := url.Parse(c.PublicURL); err == nil && canonicalOrigin(pub) == targetOrigin {
			return true
		}
	}
	for _, alias := range c.Aliases {
		if parsed, err := url.Parse(alias); err == nil && canonicalOrigin(parsed) == targetOrigin {
			return true
		}
	}
	return false
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
