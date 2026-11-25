package httpapi

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/store"
)

// Per-user connector availability preferences (unified connector UX). The
// connections page is the AVAILABILITY layer: one list of every connector —
// bundled (sandboxed, operator-shipped) and remote (own + shared) — each with
// an "enabled for me" toggle, and a default credential-account seat for
// bundled connectors. The chat Tools picker then narrows to the user's
// enabled set (per-conversation SELECTION, supervised), and chat turns use
// the user's default seat. Scheduled tasks are deliberately DIFFERENT: they
// pin an explicit per-task {server, account} selection (plus the Gate-3
// credential allowlist) because unsupervised automation must not drift with
// a user's later preference changes.
//
// Prefs are a preference, not an authority boundary — absence of a row means
// the operator default, so the feature is a no-op until a user opts.

// connectorPrefs handles /connector-prefs:
//
//	GET    → {"prefs": [ConnectorPref...]} — the user's explicit choices
//	PUT    → upsert one {kind, connector_id, enabled, default_account}
//	DELETE → ?kind=&id= — drop the explicit choice (revert to operator default)
func (s *Server) connectorPrefs(w http.ResponseWriter, r *http.Request) {
	user := userFromCtx(r.Context())
	switch r.Method {
	case http.MethodGet:
		prefs, err := s.store.ListConnectorPrefs(r.Context(), user)
		if err != nil {
			writeInternalError(w, r, err)
			return
		}
		out := make([]store.ConnectorPref, 0, len(prefs))
		for _, p := range prefs {
			out = append(out, p)
		}
		slices.SortFunc(out, func(a, b store.ConnectorPref) int {
			if a.Kind != b.Kind {
				if a.Kind < b.Kind {
					return -1
				}
				return 1
			}
			if a.ConnectorID < b.ConnectorID {
				return -1
			}
			if a.ConnectorID > b.ConnectorID {
				return 1
			}
			return 0
		})
		writeJSON(w, r, map[string]any{"prefs": out})
	case http.MethodPut:
		var p store.ConnectorPref
		if !decodeJSONBody(w, r, &p) {
			return
		}
		// A bundled seat must exist in the catalog right now; a stale or
		// mistyped seat fails loudly here rather than degrading silently later.
		if p.Kind == store.ConnectorKindBundled && p.DefaultAccount != "" && s.agent != nil {
			if !catalogSeatExists(s.agent.MCPServerCatalog(), p.ConnectorID, p.DefaultAccount) {
				http.Error(w, "unknown account seat for connector "+p.ConnectorID, http.StatusBadRequest)
				return
			}
		}
		if err := s.store.SetConnectorPref(r.Context(), user, p); err != nil {
			if writeAccountGone(w, err) {
				return
			}
			if errors.Is(err, store.ErrConnectorPrefInvalid) {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			writeInternalError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case http.MethodDelete:
		kind := r.URL.Query().Get("kind")
		id := r.URL.Query().Get("id")
		if kind == "" || id == "" {
			http.Error(w, "kind and id query params required", http.StatusBadRequest)
			return
		}
		if err := s.store.DeleteConnectorPref(r.Context(), user, kind, id); err != nil {
			writeInternalError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// catalogSeatExists reports whether the named credential-account seat is
// currently provisioned for the bundled connector.
func catalogSeatExists(catalog []agent.OptionalServerInfo, server, account string) bool {
	for _, info := range catalog {
		if info.Name == server {
			return slices.Contains(info.Accounts, account)
		}
	}
	return false
}

// bundledPrefFor resolves a user's effective state for one bundled connector.
// available: whether pickers offer it at all — true unless the user explicitly
// disabled it. defaultOn: whether new conversations start with it enabled —
// the explicit pref when one exists, else the operator's enabled_by_default.
// seat: the user's default credential-account, validated against the live
// catalog (a stale seat degrades to the default seat rather than failing the
// surface that reads it).
func bundledPrefFor(prefs map[string]store.ConnectorPref, info agent.OptionalServerInfo) (available, defaultOn bool, seat string) {
	p, ok := prefs[store.ConnectorPrefKey(store.ConnectorKindBundled, info.Name)]
	if !ok {
		return true, info.EnabledByDefault, ""
	}
	seat = p.DefaultAccount
	if seat != "" && !slices.Contains(info.Accounts, seat) {
		seat = ""
	}
	// Two-toggle semantics: availability and new-chat seeding are independent
	// intents. A row carries the user's complete explicit state, so defaultOn
	// is simply "available AND chosen always-on" — no fallback to the
	// operator's enabled_by_default once the user has expressed a choice.
	return p.Enabled, p.Enabled && p.AutoEnable, seat
}

// bundledChosenAccounts reads a bundled connector's stored account choice
// (#988, a single account or a list, docs/MULTI-ACCOUNT-CHATS.md) against the
// live catalog and the user's connections-page default, prefSeat. The result
// is the choice as it stands NOW, with "" still standing for "my default": it
// is what the picker shows and, with "" filled in, what the turn binds, so the
// two cannot disagree. Never empty, never a repeat.
//
//   - A named account that is no longer provisioned is left out, the same
//     posture as a stale pref.
//   - "" beside the account that IS the user's default names one account
//     twice; the name is kept and "" dropped.
//   - A choice with nothing left is the default account, so the turn still
//     runs.
//   - The list is cut to the cap, in case a row was written by something
//     other than cleanMCPAccounts.
func bundledChosenAccounts(info agent.OptionalServerInfo, override, prefSeat string) []string {
	override = strings.TrimSpace(override)
	// The whole value is one provisioned account: the shape every row had
	// before lists existed.
	if override != "" && slices.Contains(info.Accounts, override) {
		return []string{override}
	}
	stored := store.SplitMCPAccounts(override)
	defaultNamed := prefSeat != "" && slices.Contains(stored, prefSeat) && slices.Contains(info.Accounts, prefSeat)
	out := make([]string, 0, 1)
	seen := map[string]bool{}
	for _, a := range stored {
		switch {
		case a == "" && defaultNamed:
			continue
		case a != "" && !slices.Contains(info.Accounts, a):
			continue
		}
		if seen[a] || len(out) >= store.MaxMCPAccountsPerConnector {
			continue
		}
		seen[a] = true
		out = append(out, a)
	}
	if len(out) == 0 {
		return []string{""}
	}
	return out
}

// bundledTurnAccounts resolves the accounts a bundled connector runs as in one
// conversation: bundledChosenAccounts with "" filled in by prefSeat ("" again
// when the user has no default: the deployment's own account). Its first entry
// is the account every single-account path has always used.
func bundledTurnAccounts(info agent.OptionalServerInfo, override, prefSeat string) []string {
	chosen := bundledChosenAccounts(info, override, prefSeat)
	for i, a := range chosen {
		if a == "" {
			chosen[i] = prefSeat
		}
	}
	return chosen
}

// applyConnectorPrefs filters a conversation's opted-in bundled servers by the
// user's availability prefs and resolves each connector's credential-account
// seat for the turn: the conversation's own override (#988) when it names a
// seat that exists, else the user's connections-page default. Best-effort — a
// prefs read failure keeps operator defaults rather than failing the turn.
// Names not in the bundled catalog (remote-overlay entries) pass through
// untouched except for their override, which the overlay pins; a remote name
// without one mounts the seat its owner flagged default.
//
// A bundled connector the conversation runs as several accounts
// (docs/MULTI-ACCOUNT-CHATS.md) has its first account in the returned seat map,
// exactly where a single account goes, and the rest in extra, in order. extra
// is nil unless some connector has a second account, so a conversation that
// never asked for one takes the path it always took.
func (s *Server) applyConnectorPrefs(ctx context.Context, user string, enabled []string, overrides map[string]string) (kept []string, accountDefaults map[string]string, extra map[string][]string) {
	prefs, err := s.store.ListConnectorPrefs(ctx, user)
	if err != nil {
		prefs = nil
	}
	byName := map[string]agent.OptionalServerInfo{}
	for _, info := range s.agent.MCPServerCatalog() {
		byName[info.Name] = info
	}
	kept = make([]string, 0, len(enabled))
	set := func(name, seat string) {
		if seat == "" {
			return
		}
		if accountDefaults == nil {
			accountDefaults = map[string]string{}
		}
		accountDefaults[name] = seat
	}
	for _, name := range enabled {
		info, known := byName[name]
		if !known {
			kept = append(kept, name)
			// Remote connection: the override is a seat label the overlay
			// pins verbatim (it reports an unconnected pin as skipped rather
			// than substituting another account).
			set(name, overrides[name])
			continue
		}
		avail, _, seat := bundledPrefFor(prefs, info)
		if !avail {
			continue
		}
		kept = append(kept, name)
		// A stale override (seat since de-provisioned) degrades to the user's
		// default seat — same posture as a stale pref — so the turn still runs.
		accounts := bundledTurnAccounts(info, overrides[name], seat)
		set(name, accounts[0])
		if len(accounts) > 1 {
			if extra == nil {
				extra = map[string][]string{}
			}
			extra[name] = accounts[1:]
		}
	}
	return kept, accountDefaults, extra
}
