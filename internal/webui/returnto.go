// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package webui

import (
	"encoding/base64"
	"net/http"
	"net/url"
	"strings"

	"github.com/ElcanoTek/fleet/internal/logging"
)

// Deep links survive sign-in (docs/DEEP-LINK-SIGN-IN.md). A signed-out visit
// to a page — `/chat?persona=<ref>&draft=<text>` (docs/USER-PERSONAS.md), `/chat?c=<id>`,
// a task notification's `/orchestrator/tasks/<id>` — parks that page's path
// and query in a short-lived cookie on its way to /login, and the first
// signed-in landing sends the visitor on to it. Every sign-in leg ends on `/`
// with a fresh session (the password form's 303, the OIDC callback's 303,
// central Auth's return_to for the Elcano email link), so that landing is the
// one place the link is redeemed, whichever leg was used. It is the team-link
// cookie's shape (gateway.go), generalised to any page and its whole query.
//
// A cookie, not a return URL threaded through the legs: the gate is the only
// place that sees the original URL, and carrying it from there would mean the
// login page, each leg, the OIDC state and central Auth's return_to all
// passing it on — and the draft text landing in the IdP's and Auth's request
// logs. The cookie never leaves this origin.
const (
	// returnToCookie is the cookie's name on a plain-http origin (local and
	// loopback installs). On https it is returnToHostCookie: a browser
	// accepts a __Host- cookie only host-only, Secure and on Path=/, so no
	// sibling subdomain can plant one for the whole domain — one this gate
	// could neither tell from its own nor delete.
	returnToCookie        = "fleet_return_to"
	returnToHostCookie    = "__Host-" + returnToCookie
	returnToMaxAgeSeconds = teamLinkMaxAgeSeconds
	// maxReturnToLen caps the parked path+query as the browser sent it
	// (percent-encoded). A draft of several hundred characters fits; base64
	// keeps the cookie under 2.8 KB, inside the 4 KB a browser keeps per
	// cookie. A longer link is not parked and sign-in lands on the start page.
	maxReturnToLen = 2048
	// returnToParam is the sign-in entries' own way in, for a link whose page
	// already loaded when the session turned out revoked (a sign-out the
	// gate's cookie check cannot see): the chat page, which strips its link's
	// parameters as it boots, sends the visitor to /login?return_to=<link>
	// (web/src/app/chat/ui/bootstrapFailure.ts), and the Operations Center's
	// sign-in card passes its page to /login or to its Elcano email hand-off
	// the same way (OrchestratorLogin.tsx).
	returnToParam = "return_to"
)

// returnRoute reports whether p, a decoded path, is a page a parked link may
// lead to: one the SPA renders behind sign-in, or a bookmark alias servePage
// redirects to one. /login and /no-access are sign-in's own pages and would
// loop; / is the start page itself.
func returnRoute(p string) bool {
	switch {
	case p == "/login" || p == "/no-access":
		return false
	case pagePaths[p] || adminAlias(p):
		return true
	case strings.HasPrefix(p, "/help/"):
		_, ok := helpGuides[strings.TrimPrefix(p, "/help/")]
		return ok
	case strings.HasPrefix(p, "/orchestrator/tasks/"):
		id := strings.TrimPrefix(p, "/orchestrator/tasks/")
		return id != "" && !strings.Contains(id, "/")
	}
	return false
}

// safeReturnPath reports whether s, a path and query, may be parked and later
// redirected to. It must be a path-absolute reference on this origin — no
// scheme, host, userinfo or fragment, no "//" or "/\" prefix, no backslash,
// space, control or non-ASCII byte — at most maxReturnToLen long, whose
// decoded path names a page (returnRoute). The decoded check is what refuses
// the encoded tricks: "/%2F%2Fevil.example", "/%5Cevil.example" and
// "/chat/%2E%2E/api/x" decode to paths no page has.
func safeReturnPath(s string) bool {
	if s == "" || len(s) > maxReturnToLen || s[0] != '/' {
		return false
	}
	if len(s) > 1 && (s[1] == '/' || s[1] == '\\') {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; c <= ' ' || c >= 0x7f || c == '\\' {
			return false
		}
	}
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil || u.Opaque != "" || u.Fragment != "" {
		return false
	}
	return returnRoute(u.Path)
}

// requestReturnPath is r's path and query as the browser sent them.
func requestReturnPath(r *http.Request) string {
	p := r.URL.EscapedPath()
	if r.URL.RawQuery != "" {
		p += "?" + r.URL.RawQuery
	}
	return p
}

// returnCookieName is the return cookie's name for r's origin (see
// returnToCookie).
func (g *Gateway) returnCookieName(r *http.Request) string {
	if g.secureRequest(r) {
		return returnToHostCookie
	}
	return returnToCookie
}

// parkReturnPath sets the return cookie to target. The value is base64url,
// so a query's commas, quotes and semicolons never meet cookie syntax. The
// caller has checked target with safeReturnPath.
func (g *Gateway) parkReturnPath(w http.ResponseWriter, r *http.Request, target string) {
	setCookie(w, g.returnCookieName(r), base64.RawURLEncoding.EncodeToString([]byte(target)), returnToMaxAgeSeconds, g.secureRequest(r), "")
}

func (g *Gateway) clearReturnPath(w http.ResponseWriter, r *http.Request) {
	setCookie(w, g.returnCookieName(r), "", 0, g.secureRequest(r), "")
}

// parkedReturnPath is the parked link, re-checked when read: the value is
// input like any other (on plain http a sibling subdomain can still set a
// cookie of this name for the whole domain). present reports whether the
// cookie was sent at all, so a caller can clear an unusable one.
func (g *Gateway) parkedReturnPath(r *http.Request) (target string, present bool) {
	c, err := r.Cookie(g.returnCookieName(r))
	if err != nil {
		return "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil || !safeReturnPath(string(raw)) {
		return "", true
	}
	return string(raw), true
}

// parkBouncedPage is called for a signed-out request the gate is about to
// send to /login. A GET of a page parks its link; a GET of a page whose link
// cannot be parked (the start page — `/`, or `/chat` with no query — or an
// over-long or unsafe link) drops a link parked by an earlier visit, so
// sign-in never lands on an older link than the one just opened. Anything
// that is not a page (a missing icon a browser asks for on its own, an API
// call) leaves the parked link alone.
//
// A team link parked by an earlier visit is dropped the same way, unless this
// visit is a team link itself (the gate parks that one again right after): the
// team cookie is redeemed on its own at a landing, so a stale one would
// otherwise take the visitor away from the newer link.
func (g *Gateway) parkBouncedPage(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	if r.Method != http.MethodGet || (p != "/" && !returnRoute(p)) {
		return
	}
	if !teamLinkRequest(r) {
		g.dropParkedTeamLink(w, r)
	}
	target := requestReturnPath(r)
	startPage := p == "/" || (p == "/chat" && r.URL.RawQuery == "")
	if !startPage && safeReturnPath(target) {
		g.parkReturnPath(w, r, target)
		return
	}
	if !startPage && len(target) > maxReturnToLen {
		// The length only: the link's query can carry a user's draft.
		logging.From(r.Context()).Debug("webui: link too long to keep through sign-in", "len", len(target))
	}
	if _, present := g.parkedReturnPath(r); present {
		g.clearReturnPath(w, r)
	}
}

// parkLoginReturnParam parks a sign-in entry's ?return_to=<link> (see
// returnToParam) for a signed-out visitor, and reports whether it did. The
// parameter names the newest link, so an unusable one (over-long, or unsafe)
// still drops the links parked earlier, return and team alike: sign-in then
// lands on the start page, as for any over-long link. Without the parameter
// nothing changes.
func (g *Gateway) parkLoginReturnParam(w http.ResponseWriter, r *http.Request) bool {
	q := r.URL.Query()
	if r.Method != http.MethodGet || !q.Has(returnToParam) {
		return false
	}
	if target := q.Get(returnToParam); safeReturnPath(target) {
		g.parkReturnPath(w, r, target)
		return true
	}
	g.dropParkedLinks(w, r)
	return false
}

// signedInLogin answers a signed-in visit to /login: on to the link that is
// waiting — /login's own ?return_to=, else the parked link, else the parked
// team link — or to /chat. A ?return_to= is the newest link even when it is
// unusable, so it is never passed over for an older parked one. A parked
// return cookie is cleared either way.
func (g *Gateway) signedInLogin(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	target, present := g.parkedReturnPath(r)
	if present {
		g.clearReturnPath(w, r)
	}
	explicit := q.Has(returnToParam)
	if explicit {
		target = q.Get(returnToParam)
		if !safeReturnPath(target) {
			target = ""
			g.dropParkedTeamLink(w, r)
		}
	}
	switch team := parkedTeamLink(r); {
	case target != "":
		g.redirectToReturnPath(w, r, target)
	case team != "" && !explicit:
		g.redirectToParkedTeamLink(w, r, team)
	default:
		temporaryRedirect(w, g.redirectURL(r, requestLanguagePath(r, "/chat")))
	}
}

// dropParkedLinks clears whichever of the return and team cookies r carries.
func (g *Gateway) dropParkedLinks(w http.ResponseWriter, r *http.Request) {
	if _, present := g.parkedReturnPath(r); present {
		g.clearReturnPath(w, r)
	}
	g.dropParkedTeamLink(w, r)
}

// dropParkedTeamLink clears the team cookie when r carries one.
func (g *Gateway) dropParkedTeamLink(w http.ResponseWriter, r *http.Request) {
	if _, err := r.Cookie(teamLinkCookie); err == nil {
		clearTeamLink(w, g.secureRequest(r))
	}
}

// redeemReturnPath handles the parked links on a signed-in page visit. A
// sign-in leg's bare landing (startPageLanding) is sent on to the parked
// return link; any other page visit is the visitor's own newer choice, so it
// only clears the return cookie, and — away from `/` and `/chat`, whose
// team-link rules follow in ServeHTTP — a parked team link too, so a later
// bare /chat does not jump back to an older link. Requests that are not page
// visits leave both alone. It reports whether it answered the request.
//
// Redeeming also drops a parked team link (redirectToReturnPath): the return
// cookie is never older than the team cookie, since every page visit the gate
// bounces re-parks or drops both.
func (g *Gateway) redeemReturnPath(w http.ResponseWriter, r *http.Request) bool {
	p := r.URL.Path
	if r.Method != http.MethodGet || (p != "/" && !returnRoute(p)) {
		return false
	}
	if p != "/" && p != "/chat" {
		g.dropParkedTeamLink(w, r)
	}
	target, present := g.parkedReturnPath(r)
	if !present {
		return false
	}
	g.clearReturnPath(w, r)
	if target == "" || !startPageLanding(r) {
		return false
	}
	g.redirectToReturnPath(w, r, target)
	return true
}

// redirectToReturnPath sends a signed-in visitor on to a checked link, with
// the landing's language hint. It drops a parked team link on the way: the
// link being followed is the newer one (a team link's own ?team= rides in its
// query), and a team cookie left in place would redirect the visitor's next
// /chat away from it.
func (g *Gateway) redirectToReturnPath(w http.ResponseWriter, r *http.Request, target string) {
	g.dropParkedTeamLink(w, r)
	temporaryRedirect(w, g.redirectURL(r, withReturnLanguage(target, requestLanguage(r))))
}

// startPageLanding reports whether a signed-in request is a sign-in leg's
// landing: `/` or `/chat` carrying nothing but the optional lang hint. A
// request naming its own page (`/chat?c=…`) is the visitor's newer choice.
func startPageLanding(r *http.Request) bool {
	if r.URL.Path != "/" && r.URL.Path != "/chat" {
		return false
	}
	for k := range r.URL.Query() {
		if k != "lang" {
			return false
		}
	}
	return true
}

// withReturnLanguage carries the landing's language hint (see
// oidcLanguageCookie) onto the parked link, replacing a lang the link already
// had. Only the lang pairs are touched; every other byte of the link's query
// is kept as the browser sent it. language is a normalized tag (or "").
func withReturnLanguage(target, language string) string {
	if language == "" {
		return target
	}
	path, query, _ := strings.Cut(target, "?")
	var kept []string
	for _, pair := range strings.Split(query, "&") {
		key, _, _ := strings.Cut(pair, "=")
		if k, err := url.QueryUnescape(key); pair == "" || (err == nil && k == "lang") {
			continue
		}
		kept = append(kept, pair)
	}
	return path + "?" + strings.Join(append(kept, "lang="+language), "&")
}
