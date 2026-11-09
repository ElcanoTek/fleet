// Package ms365mcp implements Fleet's built-in Microsoft 365 OAuth 2.0 proxy
// and stateless Streamable HTTP Graph MCP server.
package ms365mcp

import (
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/config/envreg"
)

const (
	defaultTenantID                = "common"
	defaultAttachmentURLTTLSeconds = 300
	minAttachmentURLTTLSeconds     = 30
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

	// Test seams (defaulted automatically when empty/nil).
	GraphBaseURL string
	AuthBaseURL  string
	HTTPClient   *http.Client
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

	publicURL := strings.TrimSpace(envreg.Get("MS365_MCP_PUBLIC_URL"))
	if publicURL == "" && fleetCfg != nil {
		publicURL = strings.TrimSpace(fleetCfg.PublicBaseURL)
	}
	if publicURL == "" {
		publicURL = strings.TrimSpace(envreg.Get("FLEET_PUBLIC_BASE_URL"))
	}
	publicURL = trimBaseURL(publicURL)

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
			ttlSeconds = clampInt(parsed, minAttachmentURLTTLSeconds, maxAttachmentURLTTLSeconds)
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
		GraphBaseURL:            defaultGraphBaseURL,
		AuthBaseURL:             defaultAuthBaseURL,
		HTTPClient:              &http.Client{Timeout: 30 * time.Second},
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

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
