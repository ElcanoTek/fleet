// Read-only deployment-config endpoints: /personas, /server-config and
// /client-config — the small "what does this server expose" payloads the web
// shell reads at startup. Split out of server.go (#1127).

package httpapi

import (
	"net/http"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/clientconfig"
)

// ── /personas ──────────────────────────────────────────────────────────────

type personasResponse struct {
	Personas []string `json:"personas"`
	Default  string   `json:"default"`
}

// serverConfigResponse is the small "what does this server expose" payload
// the frontend reads at startup to decide which capability-gated UI to
// render. Currently just the lockdown affordance — extend if more
// operator-toggled UI surfaces appear.
//
//   - LockdownAvailable: lockdown UI should be shown (sandbox image is
//     configured).
//   - LockdownOnly: lockdown is enforced for every chat — frontend
//     hides the regular "+" button and always shows the badge.
//   - LockdownAllowedModels: slug allow-list, used by the model picker
//     filter.
type serverConfigResponse struct {
	LockdownAvailable     bool     `json:"lockdown_available"`
	LockdownOnly          bool     `json:"lockdown_only"`
	LockdownAllowedModels []string `json:"lockdown_allowed_models"`
	// UploadMaxBytes: per-file /attachments cap. The composer uses it to
	// reject oversize files client-side instead of discovering the limit
	// after a full upload round-trip ends in a 413.
	UploadMaxBytes int64 `json:"upload_max_bytes"`
}

func (s *Server) serverConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	resp := serverConfigResponse{
		LockdownAvailable: s.cfg.LockdownAvailable(),
		LockdownOnly:      s.cfg.LockdownOnly,
		UploadMaxBytes:    s.cfg.UploadMaxBytes,
	}
	if resp.LockdownAvailable {
		resp.LockdownAllowedModels = append(resp.LockdownAllowedModels, s.cfg.LockdownAllowedModels...)
	}
	writeJSON(w, resp)
}

// ── /client-config ──────────────────────────────────────────────────────────

// clientConfigResponse is the white-label surface the web renders: branding
// strings + the chat empty-state catalog. Sourced from the loaded client
// bundle's manifest; neutral generic defaults when no bundle is wired.
type clientConfigResponse struct {
	Branding   clientConfigBranding   `json:"branding"`
	EmptyState clientConfigEmptyState `json:"empty_state"`
	// Models carries the workspace's effective model tiers (#1187) — the slug a
	// new conversation starts on and the escalation target — resolved from the
	// live agentcore holders the admin settings apply into. The web reads this
	// on every shell mount, which is what makes the admin setting live without
	// a rebuild: the compiled-in web constants remain only its fallback.
	Models clientConfigModels `json:"models"`
}

type clientConfigModels struct {
	DefaultModel  string `json:"default_model"`
	AdvancedModel string `json:"advanced_model"`
}

type clientConfigBranding struct {
	AppName          string `json:"app_name"`
	LoginTitle       string `json:"login_title"`
	LoginTagline     string `json:"login_tagline"`
	ShareTitle       string `json:"share_title"`
	ShareDescription string `json:"share_description"`
	// LogoURL is the web path the shell renders as the brand mark, or "" when
	// the bundle declares no logo (the web then uses fleet's own). A URL rather
	// than the manifest's bundle-relative path: the browser cannot read the
	// bundle, and the web is a separate process, so the only thing it can act on
	// is the proxied route. Omitted from JSON when empty so an older web build
	// sees exactly what it saw before.
	LogoURL string `json:"logo_url,omitempty"`
}

// brandLogoWebPath is the Next-proxied path the browser fetches the bundle mark
// from. It intentionally mirrors /api/theme: one public proxy per brand asset,
// so the login shell can render both before a session exists.
const brandLogoWebPath = "/api/brand/logo"

type clientConfigEmptyState struct {
	Cards         []map[string]any `json:"cards"`
	ProtocolPills []map[string]any `json:"protocol_pills"`
}

func (s *Server) clientConfigHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// No-bundle fallback branding flows through the SAME source of truth a sparse
	// manifest uses (clientconfig.applyBrandingDefaults), so the no-bundle and
	// sparse-bundle UIs cannot drift.
	d := clientconfig.DefaultBranding()
	resp := clientConfigResponse{
		Branding: clientConfigBranding{
			AppName:          d.AppName,
			LoginTitle:       d.LoginTitle,
			LoginTagline:     d.LoginTagline,
			ShareTitle:       d.ShareTitle,
			ShareDescription: d.ShareDescription,
		},
		EmptyState: clientConfigEmptyState{
			Cards:         []map[string]any{},
			ProtocolPills: []map[string]any{},
		},
		Models: clientConfigModels{
			DefaultModel:  agentcore.CurrentDefaultModel(),
			AdvancedModel: agentcore.CurrentAdvancedModel(),
		},
	}
	if s.clientConfig != nil {
		b := s.clientConfig
		resp.Branding = clientConfigBranding{
			AppName:          b.Branding.AppName,
			LoginTitle:       b.Branding.LoginTitle,
			LoginTagline:     b.Branding.LoginTagline,
			ShareTitle:       b.Branding.ShareTitle,
			ShareDescription: b.Branding.ShareDescription,
		}
		// Advertise the logo only when a file actually backed it at load, so the
		// web never renders an <img> at a route that 404s.
		if b.BrandLogoPath != "" {
			resp.Branding.LogoURL = brandLogoWebPath
		}
		if len(b.EmptyState.Cards) > 0 {
			resp.EmptyState.Cards = b.EmptyState.Cards
		}
		if len(b.EmptyState.ProtocolPills) > 0 {
			resp.EmptyState.ProtocolPills = b.EmptyState.ProtocolPills
		}
	}
	writeJSON(w, resp)
}

func (s *Server) listPersonas(w http.ResponseWriter, _ *http.Request) {
	names, err := s.agent.ListPersonas()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, personasResponse{
		Personas: names,
		Default:  s.cfg.PersonaDefault,
	})
}
