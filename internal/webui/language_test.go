package webui

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/websession"
)

func TestRequestLanguage(t *testing.T) {
	for _, tc := range []struct{ query, cookie, want string }{
		{"es-MX", "pt", "es"}, {"", "pt-BR", "pt"}, {"fr", "es", "es"},
		{"", "", ""}, {"-", "_", ""}, {" ", "", ""}, {"en", "es", "en"},
	} {
		r := httptest.NewRequest(http.MethodGet, "/?lang="+url.QueryEscape(tc.query), nil)
		if tc.cookie != "" {
			r.AddCookie(&http.Cookie{Name: "elcano_language", Value: tc.cookie})
		}
		if got := requestLanguage(r); got != tc.want {
			t.Errorf("query=%q cookie=%q: got %q, want %q", tc.query, tc.cookie, got, tc.want)
		}
	}
}

func TestOIDCStartPassesLanguageWithoutChangingRedirect(t *testing.T) {
	idp := newFakeIDP(t)
	e := oidcEnv(t, idp)
	res := e.do(req("GET", "/api/auth/oidc/start?lang=pt-BR", ""))
	loc, err := url.Parse(res.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if loc.Query().Get("ui_locales") != "pt" || loc.Query().Get("redirect_uri") != testOrigin+"/api/auth/oidc/callback" {
		t.Fatalf("unexpected authorization parameters: %v", loc.Query())
	}
	if loc.Query().Get("state") == "" || loc.Query().Get("nonce") == "" || loc.Query().Get("code_challenge") == "" {
		t.Fatal("OIDC protections missing")
	}
}

func TestPageLanguageMatchesBrowserAndExplicitPreference(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/help", nil)
	r.Header.Set("Accept-Language", "pt-BR,pt;q=0.9,en;q=0.8")
	if got := pageLanguage(r); got != "pt" {
		t.Fatalf("browser language = %q", got)
	}
	r.AddCookie(&http.Cookie{Name: "elcano_language", Value: "es"})
	if got := pageLanguage(r); got != "es" {
		t.Fatalf("saved language = %q", got)
	}
	r.URL.RawQuery = "lang=en"
	if got := pageLanguage(r); got != "en" {
		t.Fatalf("explicit language = %q", got)
	}
}

func TestPublicMetadataLocalizationDoesNotMutateCachedBranding(t *testing.T) {
	b := Branding{AppName: "Example", ShareTitle: "Welcome", ShareDescription: "About", Localizations: map[string]map[string]string{
		"es": {"Welcome": "Bienvenido", "About": "Información"},
		"pt": {"Welcome": "Boas-vindas", "About": "Informações"},
	}}
	es := localizedBranding(b, "es")
	pt := localizedBranding(b, "pt")
	if es.ShareTitle != "Bienvenido" || pt.ShareDescription != "Informações" || b.ShareTitle != "Welcome" {
		t.Fatal("public metadata language crossed requests")
	}
	if localizedPageTitle("Chat guide", "es") != "Guía del chat" || localizedPageTitle("Customer title", "pt") != "Customer title" {
		t.Fatal("owned titles must translate without changing arbitrary titles")
	}
}

func TestSilentLoginRetainsExplicitLanguage(t *testing.T) {
	e := newTestEnv(t, withOIDC(&OIDCConfig{Issuer: "https://idp.example", ClientID: "fleet", ClientSecret: "s", AutoStart: true}))
	res := e.do(req("GET", "/login?lang=pt", ""))
	loc, err := url.Parse(res.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if loc.Path != "/api/auth/oidc/start" || loc.Query().Get("silent") != "1" || loc.Query().Get("lang") != "pt" {
		t.Fatalf("language lost during silent sign-in: %s", loc)
	}
}

func TestAutomaticLoginRetainsChoiceAcrossEveryCallback(t *testing.T) {
	for _, outcome := range []string{"success", "silent", "denied", "invalid-state"} {
		t.Run(outcome, func(t *testing.T) {
			idp := newFakeIDP(t)
			e := oidcEnv(t, idp)
			e.g.cfg.OIDC.AutoStart = true
			if err := e.g.spa.setIndex(strings.Replace(testIndexHTML, "<html>", `<html lang="en">`, 1)); err != nil {
				t.Fatal(err)
			}
			e.chat.respond = func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{"session_epoch":"ext-1"}`) }
			// The explicit URL must win over an existing English preference, even
			// though automatic SSO leaves before any client-side code can run.
			oldChoice := func(r *http.Request) *http.Request { return withCookie(r, "elcano_language", "en") }
			first := e.do(oldChoice(req("GET", "/?lang=pt-BR", "")))
			if first.Header.Get("Location") != testOrigin+"/login?lang=pt" {
				t.Fatal(first.Header.Get("Location"))
			}
			login := e.do(oldChoice(req("GET", first.Header.Get("Location"), "")))
			start := e.do(oldChoice(req("GET", login.Header.Get("Location"), "")))
			authorize, err := url.Parse(start.Header.Get("Location"))
			if err != nil {
				t.Fatal(err)
			}
			q := authorize.Query()
			if q.Get("ui_locales") != "pt" || q.Get("redirect_uri") != testOrigin+"/api/auth/oidc/callback" || q.Get("prompt") != "none" {
				t.Fatal(q)
			}
			callbackQuery := url.Values{"code": {"locale-code"}, "state": {q.Get("state")}}
			wantPath, wantCode := "/", ""
			switch outcome {
			case "silent":
				callbackQuery = url.Values{"error": {"login_required"}}
				wantPath, wantCode = "/login", "sso"
			case "denied":
				callbackQuery = url.Values{"error": {"access_denied"}}
				wantPath, wantCode = "/login", "e"
			case "invalid-state":
				callbackQuery.Set("state", "mismatched")
				wantPath, wantCode = "/login", "e"
			}
			callback := oldChoice(req("GET", "/api/auth/oidc/callback?"+callbackQuery.Encode(), ""))
			cookies := forwardLocaleTransaction(t, start, callback, q)
			idp.idClaims = map[string]any{"iss": idp.srv.URL, "aud": "client-123", "exp": float64(time.Now().Add(time.Hour).Unix()), "nonce": q.Get("nonce"), "email": "person@example.com", "sub": "account-123"}
			result := e.do(callback)
			landing, err := url.Parse(result.Header.Get("Location"))
			if err != nil {
				t.Fatal(err)
			}
			if result.StatusCode != http.StatusSeeOther || landing.Path != wantPath || landing.Query().Get("lang") != "pt" || (wantCode != "" && landing.Query().Get(wantCode) == "") {
				t.Fatal(landing, result.StatusCode)
			}
			cleared := false
			session := ""
			for _, c := range result.Cookies() {
				if c.Name == oidcLanguageCookie {
					cleared = c.Value == "" && c.MaxAge < 0
				}
				if c.Name == websession.CookieName {
					session = c.Value
				}
			}
			if !cleared {
				t.Fatal("temporary language was not cleared")
			}
			if outcome != "success" {
				if session != "" {
					t.Fatal("failed authentication minted a session")
				}
				page := e.do(oldChoice(req("GET", landing.String(), "")))
				if !strings.Contains(readBody(t, page), `<html lang="pt"`) {
					t.Fatal("failed sign-in page lost the explicit language")
				}
				return
			}
			if session == "" {
				t.Fatal("successful authentication missing session")
			}
			if idp.tokenForm.Get("code_verifier") != cookies[oidcVerifierCookie] || idp.tokenForm.Get("redirect_uri") != testOrigin+"/api/auth/oidc/callback" {
				t.Fatal("PKCE/callback changed")
			}
			home := e.do(withCookie(oldChoice(req("GET", landing.String(), "")), websession.CookieName, session))
			chat, err := url.Parse(home.Header.Get("Location"))
			if err != nil {
				t.Fatal(err)
			}
			if chat.Path != "/chat" || chat.Query().Get("lang") != "pt" {
				t.Fatal(chat)
			}
			page := e.do(withCookie(oldChoice(req("GET", chat.String(), "")), websession.CookieName, session))
			if !strings.Contains(readBody(t, page), `<html lang="pt"`) {
				t.Fatal("final page lost explicit language")
			}
		})
	}
}

func TestInvalidLanguageCannotChangeNavigation(t *testing.T) {
	for _, raw := range []string{"//evil.example", "javascript:alert(1)", "es?next=//evil.example", "../pt", "pt\r\nLocation: https://evil.example"} {
		if got := withLanguage("/login?e=oidc_error", raw); got != "/login?e=oidc_error" {
			t.Fatalf("%q changed path: %q", raw, got)
		}
		e := newTestEnv(t, withOrigin(testOrigin))
		response := e.do(withCookie(req("GET", "/api/auth/oidc/callback?error=access_denied", ""), oidcLanguageCookie, raw))
		target, _ := url.Parse(response.Header.Get("Location"))
		if target.Host != "fleet.example.com" || target.Path != "/login" || target.Query().Get("lang") != "" {
			t.Fatal(target)
		}
	}
}

func forwardLocaleTransaction(t *testing.T, start *response, callback *http.Request, q url.Values) map[string]string {
	t.Helper()

	cookies := map[string]string{}
	for _, c := range start.Cookies() {
		callback.AddCookie(c)
		cookies[c.Name] = c.Value
		if c.Name == oidcLanguageCookie && (!c.Secure || !c.HttpOnly || c.Domain != "" || c.Path != "/" || c.MaxAge != oidcTempCookieMaxAge || c.Value != "pt") {
			t.Fatalf("language cookie %+v", c)
		}
	}
	if cookies[oidcLanguageCookie] != "pt" || cookies[oidcStateCookie] != q.Get("state") || cookies[oidcNonceCookie] != q.Get("nonce") || pkceChallenge(cookies[oidcVerifierCookie]) != q.Get("code_challenge") {
		t.Fatal("authorization transaction changed")
	}
	return cookies
}
