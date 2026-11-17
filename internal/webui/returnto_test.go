// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package webui

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/websession"
)

// Deep links survive sign-in (returnto.go): a signed-out visit parks the
// page's path and query, and the landing every sign-in leg ends on sends the
// visitor on to it.

// manifestLink is the link Manifest's "Book in Fleet chat" button opens.
const manifestLink = "/chat?draft=Book%20Manifest%20intake%20ELC07431%20(intake%2042)"

// parkedValue is the fleet_return_to value res set, decoded; ok is false when
// res set none (a deletion counts as none).
func parkedValue(t *testing.T, res *response) (string, bool) {
	t.Helper()
	for _, c := range res.Cookies() {
		if c.Name != returnToCookie || c.MaxAge <= 0 {
			continue
		}
		raw, err := base64.RawURLEncoding.DecodeString(c.Value)
		if err != nil {
			t.Fatalf("parked value %q is not base64url: %v", c.Value, err)
		}
		return string(raw), true
	}
	return "", false
}

var returnCleared = regexp.MustCompile(`(?m)^fleet_return_to=;.*Max-Age=0`)

func withParked(r *http.Request, target string) *http.Request {
	return withCookie(r, returnToCookie, base64.RawURLEncoding.EncodeToString([]byte(target)))
}

// bounce is a signed-out visit to link; it returns the value parked.
func bounce(t *testing.T, e *testEnv, link string) string {
	t.Helper()
	res := e.do(req("GET", link, ""))
	if res.StatusCode != http.StatusTemporaryRedirect || !strings.HasPrefix(res.Header.Get("Location"), testOrigin+"/login") {
		t.Fatalf("%s: %d %q", link, res.StatusCode, res.Header.Get("Location"))
	}
	parked, ok := parkedValue(t, res)
	if !ok {
		t.Fatalf("%s: nothing parked: %q", link, setCookies(res))
	}
	return parked
}

func TestDeepLinkParkedOnTheWayToLogin(t *testing.T) {
	e := newTestEnv(t, withOrigin(testOrigin))
	for _, link := range []string{
		manifestLink,
		"/chat?persona=user%3Amy-twin&draft=Help%20me%20build%20my%20twin",
		"/chat?c=conv-123",
		"/chat?draft=a,b;c%3Bd\"e",
		"/orchestrator/tasks/task-9",
		"/orchestrator?task=task-9",
		"/settings/connections?connected=gmail",
		"/settings/personas",
		"/help/chat",
	} {
		res := e.do(req("GET", link, ""))
		// The redirect itself is unchanged: plain /login, nothing in its URL.
		if res.StatusCode != http.StatusTemporaryRedirect || res.Header.Get("Location") != testOrigin+"/login" {
			t.Errorf("%s: %d %q", link, res.StatusCode, res.Header.Get("Location"))
		}
		if got, ok := parkedValue(t, res); !ok || got != link {
			t.Errorf("%s: parked %q (%v)", link, got, ok)
		}
		c := setCookies(res)
		for _, want := range []string{"fleet_return_to=", "HttpOnly", "SameSite=Lax", "Path=/", "Max-Age=1800", "Secure"} {
			if !strings.Contains(c, want) {
				t.Errorf("%s: cookie %q lacks %q", link, c, want)
			}
		}
	}
}

// The no-query case is unchanged: `/` and bare `/chat` are where sign-in lands
// anyway, so nothing is parked and the response is what it always was. A link
// parked by an earlier visit is dropped, so sign-in does not land on it.
func TestDeepLinkNoQueryUnchanged(t *testing.T) {
	e := newTestEnv(t, withOrigin(testOrigin))
	for _, p := range []string{"/", "/chat"} {
		res := e.do(req("GET", p, ""))
		if res.StatusCode != http.StatusTemporaryRedirect || res.Header.Get("Location") != testOrigin+"/login" || len(res.Header.Values("Set-Cookie")) != 0 {
			t.Errorf("%s: %d %q %q", p, res.StatusCode, res.Header.Get("Location"), setCookies(res))
		}
		res = e.do(withParked(req("GET", p, ""), manifestLink))
		if res.Header.Get("Location") != testOrigin+"/login" || !returnCleared.MatchString(setCookies(res)) {
			t.Errorf("%s with an older link parked: %q %q", p, res.Header.Get("Location"), setCookies(res))
		}
	}
	// Signed in with nothing parked: / still goes to /chat, untouched.
	res := e.do(signedIn(t, req("GET", "/", "")))
	if res.Header.Get("Location") != testOrigin+"/chat" || strings.Contains(setCookies(res), returnToCookie) {
		t.Fatalf("%q %q", res.Header.Get("Location"), setCookies(res))
	}
	res = e.do(signedIn(t, req("GET", "/login", "")))
	if res.Header.Get("Location") != testOrigin+"/chat" || strings.Contains(setCookies(res), returnToCookie) {
		t.Fatalf("%q %q", res.Header.Get("Location"), setCookies(res))
	}
}

// Requests that are not a page visit never touch the parked link: a browser
// asking for an icon on its own while the login page loads must not replace
// the link being signed in to.
func TestDeepLinkUntouchedByNonPages(t *testing.T) {
	e := newTestEnv(t, withOrigin(testOrigin))
	for _, r := range []*http.Request{
		req("GET", "/apple-touch-icon-precomposed.png", ""),
		req("GET", "/wp-login.php?x=1", ""),
		req("GET", "/api/conversations?draft=x", ""),
		req("HEAD", "/chat?draft=x", ""),
		req("POST", "/chat?draft=x", ""),
	} {
		res := e.do(withParked(r, manifestLink))
		if strings.Contains(setCookies(res), returnToCookie) {
			t.Errorf("%s %s touched the parked link: %q", r.Method, r.URL, setCookies(res))
		}
	}
}

// sessionFrom is the session token a sign-in leg's response minted.
func sessionFrom(t *testing.T, res *response) string {
	t.Helper()
	for _, c := range res.Cookies() {
		if c.Name == websession.CookieName && c.Value != "" {
			return c.Value
		}
	}
	t.Fatalf("no session minted: %q", setCookies(res))
	return ""
}

// landAndFollow takes the landing a sign-in leg answered with, replays it with
// the session and the parked link, and checks it is sent on to want with the
// parked link cleared.
func landAndFollow(t *testing.T, e *testEnv, landing *http.Request, parked, want string) {
	t.Helper()
	res := e.do(withParked(landing, parked))
	if res.StatusCode != http.StatusTemporaryRedirect || res.Header.Get("Location") != testOrigin+want {
		t.Fatalf("landing %s: %d %q, want %q", landing.URL, res.StatusCode, res.Header.Get("Location"), testOrigin+want)
	}
	if !returnCleared.MatchString(setCookies(res)) {
		t.Fatalf("parked link not cleared: %q", setCookies(res))
	}
	if res.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("security headers missing on the redeem redirect")
	}
}

func TestDeepLinkSurvivesPasswordLogin(t *testing.T) {
	e := newTestEnv(t, withOrigin(testOrigin))
	parked := bounce(t, e, manifestLink)
	e.chat.respond = loginBackend(200, true, "epoch-7")
	login := e.do(withParked(loginReq("email=bob%40x.com&password=hunter2"), parked))
	// The leg itself is unchanged: 303 home.
	if login.StatusCode != http.StatusSeeOther || login.Header.Get("Location") != testOrigin+"/" {
		t.Fatalf("%d %q", login.StatusCode, login.Header.Get("Location"))
	}
	landing := withCookie(req("GET", "/", ""), websession.CookieName, sessionFrom(t, login))
	landAndFollow(t, e, landing, parked, manifestLink)

	// The link itself then opens as any signed-in visit does.
	res := e.do(withCookie(req("GET", manifestLink, ""), websession.CookieName, sessionFrom(t, login)))
	if res.StatusCode != http.StatusOK || res.Header.Get("Location") != "" {
		t.Fatalf("%d %q", res.StatusCode, res.Header.Get("Location"))
	}
}

func TestDeepLinkSurvivesOIDCLogin(t *testing.T) {
	idp := newFakeIDP(t)
	e := oidcEnv(t, idp)
	parked := bounce(t, e, "/chat?persona=user%3Amy-twin&draft=Hello%20there")
	idp.idClaims = map[string]any{"iss": idp.srv.URL, "aud": "client-123", "exp": float64(time.Now().Add(time.Hour).Unix()), "nonce": "n1", "email": "dave@x.com", "sub": "acct-1"}
	e.chat.respond = func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"session_epoch": "ext-1"})
	}
	cb := withCookie(withParked(callbackReq("code=c1&state=s1", "s1", "n1", "v1"), parked), oidcLanguageCookie, "es")
	res := e.do(cb)
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != testOrigin+"/?lang=es" {
		t.Fatalf("%d %q", res.StatusCode, res.Header.Get("Location"))
	}
	// The draft never went to the IdP: the callback's URL carried none of it.
	landing := withCookie(req("GET", "/?lang=es", ""), websession.CookieName, sessionFrom(t, res))
	landAndFollow(t, e, landing, parked, "/chat?persona=user%3Amy-twin&draft=Hello%20there&lang=es")

	// SSO-only: /login goes straight to the IdP, and the parked link is
	// still there for the callback's landing.
	e.g.cfg.PasswordLoginDisabled = true
	e.g.cfg.ElcanoPubKeyB64 = ""
	start := e.do(withParked(req("GET", "/login", ""), parked))
	if !strings.HasPrefix(start.Header.Get("Location"), testOrigin+"/api/auth/oidc/start") || strings.Contains(setCookies(start), returnToCookie) {
		t.Fatalf("%q %q", start.Header.Get("Location"), setCookies(start))
	}
	authorize := e.do(withParked(req("GET", "/api/auth/oidc/start", ""), parked))
	if loc := authorize.Header.Get("Location"); strings.Contains(loc, "draft") || strings.Contains(loc, "Hello") || strings.Contains(setCookies(authorize), returnToCookie) {
		t.Fatalf("the link leaked into the IdP hop: %q %q", loc, setCookies(authorize))
	}
}

func TestDeepLinkSurvivesElcanoEmailLogin(t *testing.T) {
	e := newTestEnv(t, withOrigin(testOrigin))
	parked := bounce(t, e, manifestLink)
	// The hand-off to central Auth is unchanged: return_to is the start page,
	// so the draft never reaches Auth's URL or logs.
	res := e.do(withParked(req("GET", "/api/auth/elcano-login", ""), parked))
	want := "https://auth.elcanotek.com/?return_to=" + strings.ReplaceAll(url.QueryEscape(testOrigin+"/"), "+", "%20")
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != want {
		t.Fatalf("%d %q", res.StatusCode, res.Header.Get("Location"))
	}
	// Auth sends the browser back to / with its elcano_auth cookie.
	landing := withCookie(req("GET", "/", ""), "elcano_auth", e.mintElcano(t, "erin@example.com"))
	landAndFollow(t, e, landing, parked, manifestLink)
}

// Already signed in through the shared elcano_auth cookie: the link opens
// directly, with nothing parked.
func TestDeepLinkWithCentralAuthCookieOpensDirectly(t *testing.T) {
	e := newTestEnv(t, withOrigin(testOrigin))
	res := e.do(withCookie(req("GET", manifestLink, ""), "elcano_auth", e.mintElcano(t, "erin@example.com")))
	if res.StatusCode != http.StatusOK || res.Header.Get("Location") != "" || strings.Contains(setCookies(res), returnToCookie) {
		t.Fatalf("%d %q %q", res.StatusCode, res.Header.Get("Location"), setCookies(res))
	}
}

// A team link's other parameters survive with it: the whole query is parked,
// redeemed first, and the team cookie is then dropped by the link itself.
func TestDeepLinkKeepsTeamLinkQuery(t *testing.T) {
	e := newTestEnv(t, withOrigin(testOrigin))
	link := "/chat?team=" + testTeamLinkID + "&draft=Look%20at%20this"
	res := e.do(req("GET", link, ""))
	if res.Header.Get("Location") != testOrigin+"/login?team_link=1" || !strings.Contains(setCookies(res), "fleet_team_link="+testTeamLinkID) {
		t.Fatalf("%q %q", res.Header.Get("Location"), setCookies(res))
	}
	parked, ok := parkedValue(t, res)
	if !ok || parked != link {
		t.Fatalf("parked %q", parked)
	}
	landing := withCookie(signedIn(t, req("GET", "/", "")), teamLinkCookie, testTeamLinkID)
	landAndFollow(t, e, landing, parked, link)
	res = e.do(withCookie(signedIn(t, req("GET", link, "")), teamLinkCookie, testTeamLinkID))
	if res.StatusCode != http.StatusOK || !regexp.MustCompile(`fleet_team_link=;.*Max-Age=0`).MatchString(setCookies(res)) {
		t.Fatalf("%d %q", res.StatusCode, setCookies(res))
	}
}

var teamCleared = regexp.MustCompile(`(?m)^fleet_team_link=;.*Max-Age=0`)

// A newer link supersedes a team link parked by an earlier visit: the team
// cookie is redeemed on its own at a landing, so a stale one would take the
// visitor away from the link they opened last.
func TestNewerLinkSupersedesParkedTeamLink(t *testing.T) {
	e := newTestEnv(t, withOrigin(testOrigin))
	team := "/chat?team=" + testTeamLinkID
	withTeam := func(r *http.Request) *http.Request { return withCookie(r, teamLinkCookie, testTeamLinkID) }

	// A newer deep link, or the bare start page, drops the parked team link.
	for _, link := range []string{manifestLink, "/chat", "/", "/settings/personas"} {
		res := e.do(withTeam(withParked(req("GET", link, ""), team)))
		if !teamCleared.MatchString(setCookies(res)) {
			t.Errorf("%s: team link not dropped: %q", link, setCookies(res))
		}
	}
	// The team link itself parks it again, and nothing drops it first.
	res := e.do(withTeam(req("GET", team+"&draft=x", "")))
	if c := setCookies(res); teamCleared.MatchString(c) || !strings.Contains(c, "fleet_team_link="+testTeamLinkID) {
		t.Fatalf("team link re-park: %q", c)
	}
	// A page opened signed in drops it too; requests that are not page visits
	// leave it alone, signed in or out.
	res = e.do(withTeam(signedIn(t, req("GET", "/settings/personas", ""))))
	if !teamCleared.MatchString(setCookies(res)) {
		t.Fatalf("signed-in page visit kept the team link: %q", setCookies(res))
	}
	for _, r := range []*http.Request{req("GET", "/apple-touch-icon.png", ""), req("GET", "/api/conversations", ""), req("POST", "/chat", ""), signedIn(t, req("GET", "/api/session", ""))} {
		if c := setCookies(e.do(withTeam(r))); strings.Contains(c, teamLinkCookie) {
			t.Errorf("%s %s touched the team link: %q", r.Method, r.URL, c)
		}
	}

	// Redeeming a parked link drops a team cookie that is still there, so the
	// redeemed link is not redirected away on its next request.
	landing := withTeam(signedIn(t, req("GET", "/", "")))
	landAndFollow(t, e, landing, manifestLink, manifestLink)
	if c := setCookies(e.do(withTeam(withParked(signedIn(t, req("GET", "/", "")), manifestLink)))); !teamCleared.MatchString(c) {
		t.Errorf("redeem left the team link parked: %q", c)
	}
	res = e.do(signedIn(t, req("GET", manifestLink, "")))
	if res.StatusCode != http.StatusOK || res.Header.Get("Location") != "" {
		t.Fatalf("%d %q", res.StatusCode, res.Header.Get("Location"))
	}
	// /login with a session follows its return_to, or the parked link, and
	// drops the team link the same way.
	for _, r := range []*http.Request{
		signedIn(t, req("GET", "/login?"+returnToParam+"="+url.QueryEscape(manifestLink), "")),
		withParked(signedIn(t, req("GET", "/login", "")), manifestLink),
	} {
		res = e.do(withTeam(r))
		if res.Header.Get("Location") != testOrigin+manifestLink || !teamCleared.MatchString(setCookies(res)) {
			t.Errorf("%s: %q %q", r.URL, res.Header.Get("Location"), setCookies(res))
		}
	}
	// An unusable parked link is not redeemed, so the team link still is.
	res = e.do(withTeam(withCookie(signedIn(t, req("GET", "/", "")), returnToCookie, "garbage!")))
	if res.Header.Get("Location") != testOrigin+team {
		t.Fatalf("%q", res.Header.Get("Location"))
	}
}

// The bookmark aliases servePage redirects (/admin, the retired
// notifications page) survive sign-in too and then take their own redirect.
func TestDeepLinkKeepsAdminAliases(t *testing.T) {
	e := newTestEnv(t, withOrigin(testOrigin))
	for _, link := range []string{"/admin", "/settings/admin/notifications"} {
		if parked := bounce(t, e, link); parked != link {
			t.Fatalf("%s: parked %q", link, parked)
		}
		landAndFollow(t, e, signedIn(t, req("GET", "/", "")), link, link)
		res := e.do(signedIn(t, req("GET", link, "")))
		if res.StatusCode != http.StatusPermanentRedirect || res.Header.Get("Location") != testOrigin+"/settings/admin" {
			t.Fatalf("%s: %d %q", link, res.StatusCode, res.Header.Get("Location"))
		}
	}
}

func TestDeepLinkRedeemedOnlyOnASignInLanding(t *testing.T) {
	e := newTestEnv(t, withOrigin(testOrigin))
	for _, p := range []string{"/", "/chat", "/login", "/?lang=", "/login?e=invalid"} {
		landAndFollow(t, e, signedIn(t, req("GET", p, "")), manifestLink, manifestLink)
	}
	// A page of the visitor's own choosing wins and drops the parked link.
	for _, p := range []string{"/chat?c=conv-1", "/?x=1"} {
		res := e.do(withParked(signedIn(t, req("GET", p, "")), manifestLink))
		if res.Header.Get("Location") == testOrigin+manifestLink || !returnCleared.MatchString(setCookies(res)) {
			t.Errorf("%s: %q %q", p, res.Header.Get("Location"), setCookies(res))
		}
	}
	// Any other signed-in page visit is the visitor's newer choice too: it
	// opens as usual and drops the parked link, so a later bare /chat does
	// not jump back to it.
	for _, p := range []string{"/orchestrator?task=t1", "/settings/personas", "/help/chat"} {
		res := e.do(withParked(signedIn(t, req("GET", p, "")), manifestLink))
		if res.StatusCode != http.StatusOK || !returnCleared.MatchString(setCookies(res)) {
			t.Errorf("%s: %d %q", p, res.StatusCode, setCookies(res))
		}
	}
	// Requests that are not page visits leave it alone.
	for _, r := range []*http.Request{
		signedIn(t, req("GET", "/api/session", "")),
		signedIn(t, req("GET", "/apple-touch-icon.png", "")),
		signedIn(t, req("HEAD", "/orchestrator", "")),
	} {
		if c := setCookies(e.do(withParked(r, manifestLink))); strings.Contains(c, returnToCookie) {
			t.Errorf("%s %s touched the parked link: %q", r.Method, r.URL, c)
		}
	}
	res := e.do(signedIn(t, req("GET", "/orchestrator", "")))
	if strings.Contains(setCookies(res), returnToCookie) {
		t.Fatalf("no parked link, yet %q", setCookies(res))
	}
	// The landing's language choice replaces the link's own.
	landAndFollow(t, e, signedIn(t, req("GET", "/?lang=pt", "")), "/chat?lang=en&draft=x%20y", "/chat?draft=x%20y&lang=pt")
}

// /login?return_to= is the chat page's way back in when its session turns
// out revoked after the page loaded (bootstrapFailure.ts).
func TestLoginReturnToParam(t *testing.T) {
	e := newTestEnv(t, withOrigin(testOrigin))
	q := "/login?" + returnToParam + "=" + url.QueryEscape(manifestLink)
	res := e.do(req("GET", q, ""))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("%d", res.StatusCode)
	}
	if got, ok := parkedValue(t, res); !ok || got != manifestLink {
		t.Fatalf("parked %q (%v)", got, ok)
	}
	// Signed in already (another tab, or the shared cookie): straight there.
	res = e.do(signedIn(t, req("GET", q, "")))
	if res.StatusCode != http.StatusTemporaryRedirect || res.Header.Get("Location") != testOrigin+manifestLink {
		t.Fatalf("%d %q", res.StatusCode, res.Header.Get("Location"))
	}
	// An unusable return_to is ignored, signed in or out.
	for _, bad := range []string{"//evil.example/chat", "https://evil.example/chat", "/api/conversations", "/%2F%2Fevil.example"} {
		q := "/login?" + returnToParam + "=" + url.QueryEscape(bad)
		res := e.do(req("GET", q, ""))
		if res.StatusCode != http.StatusOK || strings.Contains(setCookies(res), returnToCookie) {
			t.Errorf("%s signed out: %d %q", bad, res.StatusCode, setCookies(res))
		}
		res = e.do(signedIn(t, req("GET", q, "")))
		if res.Header.Get("Location") != testOrigin+"/chat" {
			t.Errorf("%s signed in: %q", bad, res.Header.Get("Location"))
		}
	}
}

func TestReturnPathRefusals(t *testing.T) {
	for _, s := range []string{
		"",
		"chat?draft=x",
		"https://evil.example/chat",
		"javascript:alert(1)",
		"//evil.example/chat",
		"///evil.example/chat",
		"/\\evil.example/chat",
		"/chat\\..\\api",
		"/%2F%2Fevil.example",
		"/%2fevil.example",
		"/%5Cevil.example",
		"/%5cevil.example",
		"/chat/../api/conversations",
		"/chat/%2E%2E/api/conversations",
		"/%2E%2E/evil",
		"/chat%2F..%2Fapi",
		"/%252F%252Fevil.example",
		"/api/conversations",
		"/api/auth/logout",
		"/login",
		"/login?return_to=/chat",
		"/no-access",
		"/shared/token",
		"/unknown-page",
		"/chat#fragment",
		"/chat?draft=a b",
		"/chat?draft=a\tb",
		"/chat?draft=a\x00b",
		"/chat?draft=\x7f",
		"/chat?draft=caf\xc3\xa9",
		"/\t/evil.example",
		"/ /evil.example",
		"/chat?draft=" + strings.Repeat("a", maxReturnToLen),
		"/orchestrator/tasks/",
		"/orchestrator/tasks/a/b",
		"/help/missing",
	} {
		if safeReturnPath(s) {
			t.Errorf("accepted %q", s)
		}
	}
	for _, s := range []string{
		manifestLink,
		"/chat?persona=user%3Amy-twin&draft=hi%20there",
		"/chat?c=conv-1&team=" + testTeamLinkID,
		"/chat",
		"/orchestrator/tasks/task-9",
		"/settings/admin/users",
		"/admin",
		"/settings/admin/notifications",
		"/help",
		"/help/operations-center",
		"/chat?draft=" + strings.Repeat("a", maxReturnToLen-len("/chat?draft=")),
	} {
		if !safeReturnPath(s) {
			t.Errorf("refused %q", s)
		}
	}
}

// An over-long link is not parked (the cookie would not fit): the visit still
// goes to /login, and sign-in lands on the start page as before.
func TestDeepLinkOverLongNotParked(t *testing.T) {
	e := newTestEnv(t, withOrigin(testOrigin))
	res := e.do(req("GET", "/chat?draft="+strings.Repeat("x", maxReturnToLen), ""))
	if res.Header.Get("Location") != testOrigin+"/login" {
		t.Fatalf("%q", res.Header.Get("Location"))
	}
	if _, ok := parkedValue(t, res); ok {
		t.Fatalf("parked an over-long link: %q", setCookies(res))
	}
}

// An over-long team link keeps what it always had: its own cookie opens the
// team chat after sign-in, without the rest of the link (the guide says so).
func TestOverLongTeamLinkFallsBackToTheTeamCookie(t *testing.T) {
	e := newTestEnv(t, withOrigin(testOrigin))
	res := e.do(req("GET", "/chat?team="+testTeamLinkID+"&draft="+strings.Repeat("x", maxReturnToLen), ""))
	if res.Header.Get("Location") != testOrigin+"/login?team_link=1" || !strings.Contains(setCookies(res), "fleet_team_link="+testTeamLinkID) {
		t.Fatalf("%q %q", res.Header.Get("Location"), setCookies(res))
	}
	if _, ok := parkedValue(t, res); ok {
		t.Fatalf("parked an over-long link: %q", setCookies(res))
	}
	res = e.do(withCookie(signedIn(t, req("GET", "/", "")), teamLinkCookie, testTeamLinkID))
	if res.Header.Get("Location") != testOrigin+"/chat?team="+testTeamLinkID {
		t.Fatalf("%q", res.Header.Get("Location"))
	}
}

// A parked value is re-checked when redeemed: a cookie set for the whole
// domain by a sibling subdomain is input like any other.
func TestParkedValueRecheckedOnRedeem(t *testing.T) {
	e := newTestEnv(t, withOrigin(testOrigin))
	for _, v := range []string{
		base64.RawURLEncoding.EncodeToString([]byte("//evil.example/chat")),
		base64.RawURLEncoding.EncodeToString([]byte("https://evil.example/")),
		base64.RawURLEncoding.EncodeToString([]byte("/%5Cevil.example")),
		base64.RawURLEncoding.EncodeToString([]byte("/api/auth/logout")),
		"/chat?draft=x",
		"not base64 !!",
	} {
		for _, p := range []string{"/", "/login"} {
			res := e.do(withCookie(signedIn(t, req("GET", p, "")), returnToCookie, v))
			if res.Header.Get("Location") != testOrigin+"/chat" || !returnCleared.MatchString(setCookies(res)) {
				t.Errorf("%s with %q: %q %q", p, v, res.Header.Get("Location"), setCookies(res))
			}
		}
	}
}

func TestWithReturnLanguage(t *testing.T) {
	for _, c := range []struct{ target, lang, want string }{
		{"/chat?draft=a%20b", "", "/chat?draft=a%20b"},
		{"/chat?draft=a%20b", "es", "/chat?draft=a%20b&lang=es"},
		{"/chat?lang=pt&draft=x;y", "es", "/chat?draft=x;y&lang=es"},
		{"/chat?la%6Eg=pt&draft=x", "en", "/chat?draft=x&lang=en"},
		{"/orchestrator/tasks/t1", "pt", "/orchestrator/tasks/t1?lang=pt"},
		{"/chat?draft=1&&lang=", "en", "/chat?draft=1&lang=en"},
	} {
		if got := withReturnLanguage(c.target, c.lang); got != c.want {
			t.Errorf("withReturnLanguage(%q, %q) = %q, want %q", c.target, c.lang, got, c.want)
		}
	}
}

// The parked link is never written to the log: the draft is the user's text.
func TestDeepLinkNotLogged(t *testing.T) {
	logs := captureLogs(t)
	e := newTestEnv(t, withOrigin(testOrigin))
	secret := "Book%20Manifest%20intake%20SECRET-DRAFT"
	e.do(req("GET", "/chat?draft="+secret, ""))
	e.do(req("GET", "/chat?draft="+secret+strings.Repeat("x", maxReturnToLen), ""))
	e.do(withParked(signedIn(t, req("GET", "/", "")), "/chat?draft="+secret))
	if out := logs(); strings.Contains(out, "SECRET-DRAFT") {
		t.Fatalf("draft text logged: %s", out)
	}
}

// captureLogs sends the default logger to a buffer at debug level for the
// rest of the test (webui tests do not run in parallel).
func captureLogs(t *testing.T) func() string {
	t.Helper()
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })
	return buf.String
}
