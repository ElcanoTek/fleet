package httpapi

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"unicode"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/playwrightrunner"
	"github.com/ElcanoTek/fleet/internal/tools"
)

// Browser approval cards (connector_studio's browser actions and
// playwright_browser) used the generic card, which lists the call's arguments
// verbatim: a JSON array of steps, a bare session id, a password_ref as given.
// The summary below keeps the generic card's shape (key/value rows, so the web
// and terminal clients render it unchanged) but says in words what the call
// will do, step by step, names where a credential comes from instead of
// showing it, and for a live session says which site the session started on
// and where its page is now.

type browserApprovalArgs struct {
	Action         string                  `json:"action"`
	URL            string                  `json:"url"`
	SessionID      string                  `json:"session_id"`
	Steps          []playwrightrunner.Step `json:"steps"`
	UsernameRef    string                  `json:"username_ref"`
	PasswordRef    string                  `json:"password_ref"`
	SecretRef      string                  `json:"secret_ref"`
	InspectDOM     bool                    `json:"inspect_dom"`
	CaptureNetwork bool                    `json:"capture_network"`
}

// browserCall decodes rawInput and classifies it the way the approval gate
// does (agentcore.ClassifyBrowserCall); kind is "" for anything else.
func browserCall(toolName, rawInput string) (browserApprovalArgs, agentcore.BrowserCallKind) {
	if toolName != tools.ConnectorStudioToolName && toolName != tools.PlaywrightBrowserToolName {
		return browserApprovalArgs{}, ""
	}
	var args browserApprovalArgs
	if err := json.Unmarshal([]byte(rawInput), &args); err != nil {
		return browserApprovalArgs{}, ""
	}
	return args, agentcore.ClassifyBrowserCall(toolName, args.Action, args.SessionID)
}

// summarizeBrowserInput builds the card rows for a browser call, or returns
// nil when the input is not one (the caller then uses the generic summary).
func summarizeBrowserInput(toolName, rawInput string) map[string]any {
	args, kind := browserCall(toolName, rawInput)
	if kind == "" {
		return nil
	}
	var rows []map[string]string
	add := func(k, v string) {
		rows = append(rows, map[string]string{"key": k, "value": displayArgValue(v)})
	}
	target := strings.TrimSpace(args.URL)
	switch kind {
	case agentcore.BrowserCallStartSession:
		add("What", "Open a live browser session you can watch and use")
		if target != "" {
			add("Opens", target)
		}
	case agentcore.BrowserCallSessionSteps:
		add("What", "Run these steps in your live browser session")
		if id := strings.TrimSpace(args.SessionID); id != "" {
			add("Live session", id)
		} else {
			add("Live session", "none given: this call will fail")
		}
		// connector_studio goes to its url first unless the first step is a
		// goto (connectorStudioProbeBrowser); playwright_browser's steps
		// action only uses url to resolve credentials.
		if target != "" && toolName == tools.ConnectorStudioToolName && (len(args.Steps) == 0 || args.Steps[0].Action != "goto") {
			add("Goes to", target)
		}
	default:
		add("What", "Run these steps in a new headless browser")
		if target != "" {
			add("Opens", target)
		}
	}
	hidden := plainCredentialValues(args)
	for i := range rows {
		if rows[i]["key"] == "Opens" || rows[i]["key"] == "Goes to" {
			rows[i]["value"] = hideCredentialInURL(rows[i]["value"], hidden)
		}
	}
	for i, st := range args.Steps {
		add(fmt.Sprintf("Step %d", i+1), describeBrowserStep(st, hidden))
	}
	if v := describeCredentialRef(args.UsernameRef, false); v != "" {
		add("Username", v)
	}
	if v := describeCredentialRef(args.PasswordRef, true); v != "" {
		add("Password", v)
	}
	if v := describeCredentialRef(args.SecretRef, true); v != "" {
		add("Secret", v)
	}
	if also := browserCallExtras(toolName, kind, args); also != "" {
		add("Also", also)
	}
	return map[string]any{"tool": toolName, "args": rows}
}

// browserCallExtras says what else the call reports, as the handlers decide
// it: connector_studio's probe turns both on when neither is asked for, a live
// session's steps never capture network calls, and a session start always
// inspects (connector_studio) or as asked (playwright_browser).
func browserCallExtras(toolName string, kind agentcore.BrowserCallKind, args browserApprovalArgs) string {
	inspect, capture := args.InspectDOM, args.CaptureNetwork
	if toolName == tools.ConnectorStudioToolName && kind != agentcore.BrowserCallStartSession && !inspect && !capture {
		inspect, capture = true, true
	}
	if kind != agentcore.BrowserCallOneShot {
		capture = false
	}
	if toolName == tools.ConnectorStudioToolName && kind == agentcore.BrowserCallStartSession {
		inspect = true
	}
	var also []string
	if inspect {
		also = append(also, "lists the page's fields, buttons and links")
	}
	if capture {
		also = append(also, "records the data requests the page makes")
	}
	return strings.Join(also, "; ")
}

// plainCredentialValues returns the credentials a call carries as plain
// values (not "$secret:" references), which a step must not print.
func plainCredentialValues(args browserApprovalArgs) []string {
	var out []string
	for _, ref := range []string{args.PasswordRef, args.SecretRef} {
		if v := strings.TrimSpace(ref); v != "" && !strings.HasPrefix(v, "$secret:") {
			out = append(out, v)
		}
	}
	return out
}

// hideCredentialInURL hides an address that carries one of the call's plain
// credentials (in a query, say).
func hideCredentialInURL(u string, hidden []string) string {
	for _, h := range hidden {
		if strings.Contains(u, h) {
			return "an address hidden here (it contains a credential)"
		}
	}
	return u
}

// secretFieldWords and secretFieldPairs name the words a selector for a field
// that takes a secret is made of; a literal typed into such a field is hidden
// even when it is not one of the call's credentials. Selectors are split into
// whole words (on punctuation and camelCase), so #apiKey and #pin_code match
// and #slotPicker or #secretary do not.
var (
	secretFieldWords = map[string]bool{
		"password": true, "passwd": true, "pass": true, "passcode": true, "passphrase": true, "pwd": true,
		"secret": true, "token": true, "otp": true, "totp": true, "2fa": true, "mfa": true,
		"pin": true, "pincode": true, "apikey": true, "cvv": true, "cvc": true, "csc": true, "pw": true,
	}
	secretFieldPairs = map[string]bool{
		"api key": true, "access key": true, "secret key": true, "security code": true, "verification code": true,
		"pin code": true, "pass code": true, "one time": true, "auth code": true,
	}
)

// selectorWords splits a selector into lower-case words on punctuation and
// camelCase: "input[name=apiKey]" -> input, name, api, key.
func selectorWords(sel string) []string {
	var b strings.Builder
	prevLower := false
	for _, r := range sel {
		switch {
		case unicode.IsUpper(r):
			if prevLower {
				b.WriteByte(' ')
			}
			b.WriteRune(unicode.ToLower(r))
			prevLower = false
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
			prevLower = unicode.IsLower(r) || unicode.IsDigit(r)
		default:
			b.WriteByte(' ')
			prevLower = false
		}
	}
	return strings.Fields(b.String())
}

func selectorLooksSecret(sel string) bool {
	words := selectorWords(sel)
	for i, w := range words {
		if secretFieldWords[w] {
			return true
		}
		if i+1 < len(words) && secretFieldPairs[w+" "+words[i+1]] {
			return true
		}
	}
	return false
}

const hiddenValue = "a value hidden here"

// describeBrowserStep renders one declarative step in words. Credential
// placeholders are named, never resolved; a literal value that is one of the
// call's plain credentials, or goes into a password-like field, is hidden.
func describeBrowserStep(st playwrightrunner.Step, hidden []string) string {
	sel := strings.TrimSpace(st.Selector)
	optional := ""
	if st.Optional {
		optional = " (if it is there)"
	}
	value := func() string {
		v := st.Value
		for _, h := range hidden {
			if strings.Contains(v, h) {
				return hiddenValue
			}
		}
		if selectorLooksSecret(sel) && !strings.Contains(v, "{{") {
			return hiddenValue
		}
		return describeStepValue(v)
	}
	switch strings.ToLower(strings.TrimSpace(st.Action)) {
	case "goto":
		target := strings.TrimSpace(st.Value)
		if target == "" {
			target = sel
		}
		return "Go to " + hideCredentialInURL(target, hidden)
	case "fill":
		return fmt.Sprintf("Type %s into %s%s", value(), sel, optional)
	case "click":
		return fmt.Sprintf("Click %s%s", sel, optional)
	case "press":
		return fmt.Sprintf("Press %s in %s%s", strings.TrimSpace(st.Value), sel, optional)
	case "select":
		return fmt.Sprintf("Choose %s in %s%s", value(), sel, optional)
	case "wait_for":
		switch v := strings.TrimSpace(st.Value); {
		case sel != "":
			return fmt.Sprintf("Wait for %s%s", sel, optional)
		case v == "networkidle":
			return "Wait until the page stops loading"
		case v != "":
			return "Wait until the address is " + hideCredentialInURL(v, hidden)
		}
		return fmt.Sprintf("Wait %d ms", browserWaitMs(st.TimeoutMs))
	case "sleep":
		return fmt.Sprintf("Wait %d ms", browserWaitMs(st.TimeoutMs))
	case "extract":
		return fmt.Sprintf("Read %s%s", sel, optional)
	}
	// An action the browser does not know fails when it runs; name it without
	// its value.
	return fmt.Sprintf("%q on %s (not a known step)", strings.TrimSpace(st.Action), sel)
}

// browserWaitMs is how long the browser workers actually pause for a sleep
// (or a wait_for with nothing to wait for): timeout_ms clamped to 500..30000,
// an omitted or zero value taking the step default (12 s in a live session,
// half the run's 45 s budget up to 15 s in a one-shot run), and the pause
// itself capped at 10 s.
func browserWaitMs(timeoutMs int) int {
	if timeoutMs <= 0 {
		return 10000
	}
	return min(max(timeoutMs, 500), 10000)
}

// describeStepValue names a step value: a credential placeholder by what it
// stands for, anything else quoted.
func describeStepValue(v string) string {
	switch strings.TrimSpace(v) {
	case "{{PASSWORD}}":
		return "the password"
	case "{{USERNAME}}":
		return "the username"
	case "{{SECRET}}":
		return "the secret"
	}
	r := strings.NewReplacer("{{PASSWORD}}", "<password>", "{{USERNAME}}", "<username>", "{{SECRET}}", "<secret>")
	return fmt.Sprintf("%q", r.Replace(v))
}

// describeCredentialRef says where a credential comes from without showing
// it: a vaulted reference by its form field, a value given in chat as such.
// A username is not secret and is shown as given.
func describeCredentialRef(ref string, secret bool) string {
	ref = strings.TrimSpace(ref)
	switch {
	case ref == "":
		return ""
	case strings.HasPrefix(ref, "$secret:"):
		return fmt.Sprintf("from the %q form field", strings.TrimSpace(strings.TrimPrefix(ref, "$secret:")))
	case secret:
		return "given in the chat (not shown here)"
	}
	return ref
}

// browserSessionOrigins looks up a live session's start and current URLs for
// its owner without touching it (playwrightrunner.PeekSession). A package
// variable so tests can stand in for live sessions.
var browserSessionOrigins = func(sessionID, owner string) (startURL, currentURL string, ok bool) {
	return playwrightrunner.DefaultSessionManager().PeekSession(sessionID, owner)
}

// withBrowserSessionSite adds, to the card summary of a call that runs steps
// in a live session, the site the session started on and the site its page
// is on now (origins only), and a note when they differ: a step that types a
// credential runs only on the session's own site. owner is the approval's
// user, the session's owner. Anything else is returned unchanged.
func withBrowserSessionSite(summary map[string]any, toolName, rawInput, owner string) map[string]any {
	rows, ok := summary["args"].([]map[string]string)
	if !ok || len(rows) == 0 {
		return summary
	}
	args, kind := browserCall(toolName, rawInput)
	if kind != agentcore.BrowserCallSessionSteps {
		return summary
	}
	var site []map[string]string
	startURL, currentURL, found := browserSessionOrigins(strings.TrimSpace(args.SessionID), owner)
	if !found {
		site = append(site, map[string]string{"key": "Session status", "value": "ended or expired: approving will fail; ask for a new session"})
	} else {
		start, now := originOf(startURL), originOf(currentURL)
		site = append(site, map[string]string{"key": "Session started on", "value": start})
		if now != "" {
			site = append(site, map[string]string{"key": "Page is now on", "value": now})
		}
		if now != "" && !strings.EqualFold(now, start) {
			site = append(site, map[string]string{"key": "Note", "value": "The page has left the site the session started on. A step that types a credential runs only while the page is on that site."})
		}
	}
	// Keep "What" first, then the session's site, then the rest.
	out := append([]map[string]string{rows[0]}, site...)
	out = append(out, rows[1:]...)
	summary["args"] = out
	return summary
}

// originOf renders rawURL as scheme://host, or "" when it is not an absolute
// URL. A page's path and query can carry tokens, so cards show the origin only.
func originOf(rawURL string) string {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host)
}
