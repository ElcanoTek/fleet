package webui

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// identityRefusals are the characters an identity value is refused for: C0
// controls and DEL (U+0000 among them), a format character, and characters
// that lowercase or fold onto an ASCII letter.
var identityRefusals = []string{"\x00", "\x01", "\x1f", "\x7f", "\u200b", "\u212a", "\u017f"}

// A password sign-in whose email holds such a character gets the answer a wrong
// password gets, without the backend being asked.
func TestLoginRefusesControlCharacterEmail(t *testing.T) {
	for _, ctl := range identityRefusals {
		e := newTestEnv(t, withOrigin(testOrigin))
		e.chat.respond = loginBackend(200, true, "e")
		res := e.do(loginReq("email=" + url.QueryEscape("a@x.com"+ctl) + "&password=p"))
		if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != testOrigin+"/login?e=invalid" {
			t.Errorf("email with %q: %d %q, want the invalid-credentials redirect", ctl, res.StatusCode, res.Header.Get("Location"))
		}
		if e.chat.count() != 0 {
			t.Errorf("email with %q reached the backend", ctl)
		}
		if strings.Contains(setCookies(res), "elcano_session=") {
			t.Errorf("email with %q: minted a cookie", ctl)
		}
	}
}

// A signed account event from central Auth whose email or subject holds such
// a character is refused like any other malformed event.
func TestBackchannelRefusesControlCharacterIdentity(t *testing.T) {
	k := newSigningKey(t)
	issuer := "https://auth.example.com"
	e := bcEnv(t, issuer, k.b64)
	now := time.Now()
	for _, ctl := range identityRefusals {
		for _, claim := range []string{"email", "sub"} {
			c := logoutClaims(issuer, now)
			c[claim] = c[claim].(string) + ctl
			if _, err := e.g.verifyLogoutToken(k.sign(t, "logout+jwt", c), issuer, bcAudience, now); err == nil {
				t.Errorf("%s with %q: accepted", claim, ctl)
			}
		}
	}
	if _, err := e.g.verifyLogoutToken(k.sign(t, "logout+jwt", logoutClaims(issuer, now)), issuer, bcAudience, now); err != nil {
		t.Fatalf("clean event refused: %v", err)
	}
}

// A backslash is a rule for multiplayer list entries only: an account whose
// email holds one still signs in (the backend is asked, as for any address).
func TestLoginAcceptsBackslashEmail(t *testing.T) {
	e := newTestEnv(t, withOrigin(testOrigin))
	e.chat.respond = loginBackend(200, true, "e")
	res := e.do(loginReq("email=" + url.QueryEscape(`a\b@x.com`) + "&password=p"))
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != testOrigin+"/" || e.chat.count() == 0 {
		t.Fatalf("login with a backslash email: %d %q (backend calls %d), want signed in", res.StatusCode, res.Header.Get("Location"), e.chat.count())
	}
}
