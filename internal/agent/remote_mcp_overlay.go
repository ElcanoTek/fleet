package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/mcp"
	"github.com/ElcanoTek/fleet/internal/mcpbroker"
	"github.com/ElcanoTek/fleet/internal/tools"
)

// Per-user remote (hosted) MCP overlay (#443), shared by BOTH the interactive
// and scheduled drivers. A user's OAuth-connected remote servers are wired into
// a run WITHOUT touching the long-lived shared MCP client (process-wide; mutating
// it would leak one user's bearer to another). The overlay may be an in-process
// compatibility client or a child-owned broker scope; a compositeBroker routes
// its server names to that per-run target while everything else falls through
// to the base broker.
// ApplyMCPOverlay sets the run's Deps to advertise the merged catalog and
// dispatch via that composite — the SAME governed loop, just a different broker
// seam impl. The overlay client is closed by the caller at run end.
//
// The agent package can't import internal/remotemcp (that package imports
// internal/store, which imports this package — a cycle), so the dependency is
// inverted: this file declares the small RemoteMCPResolver interface that
// remotemcp.Service satisfies, and cmd/fleet injects the concrete service.

// RemoteMCPConn is a connected remote MCP server, in the store-agnostic shape
// the overlay needs.
type RemoteMCPConn struct {
	ID   string
	Name string // connection name: the picker/opt-in key and the mcp_<name>_* prefix of the unlabeled seat
	URL  string
	// Account is this seat's public label under Name (#988); "" is the
	// unlabeled seat. The run registers the seat under
	// agentcore.RegisteredMCPName(Name, Account) — the bundle seat formula —
	// so a labeled seat's tools read mcp_<name>_<account>_*.
	Account string
	// Default marks the seat a run mounts for Name when nothing pins another.
	Default bool
	// Owner is the connection owner's email when this server was SHARED with
	// the running user (empty for the user's own servers). Used for audit
	// attribution: tool calls authenticate with the owner's token host-side.
	Owner string
	// AuthQuery is the query-parameter NAME the credential is sent under for
	// api_key connections that authenticate in the URL (Browserbase). The
	// transport attaches it per-request; the registered URL stays clean.
	AuthQuery string
	// AuthHeader is the header NAME the credential is sent under for api_key
	// connections (e.g. "X-API-Key"). Empty means the default OAuth/bearer
	// shape: "Authorization: Bearer <credential>".
	AuthHeader string
}

// RemoteMCPResolver supplies a user's connected remote servers and mints fresh
// bearer tokens for them. remotemcp.Service implements it.
type RemoteMCPResolver interface {
	// ConnectedServersForUser returns the user's servers currently in the
	// "connected" state (ready to use).
	ConnectedServersForUser(ctx context.Context, email string) ([]RemoteMCPConn, error)
	// AcquireTokenByID returns a valid bearer for the server, refreshing under a
	// lock if needed. A needs-reauth/expired connection returns an error so the
	// caller skips the server gracefully.
	AcquireTokenByID(ctx context.Context, email, serverID string) (string, error)
	// SafeHTTPClient is the SSRF-safe client used to dial user-supplied servers.
	SafeHTTPClient() *http.Client
}

// RemoteMCPStatusMarker is the optional resolver seam the overlay uses to
// record a credential rejection on the connection row. A server that answers
// the per-turn mount with HTTP 401 has refused the stored token, and without
// this the row kept reading `connected` until the token's natural expiry made
// a refresh fail — for GitHub after "Revoke all user tokens", eight hours in
// which Settings → Connections showed nothing wrong while every turn skipped
// the server (#1006). Optional (a type assertion) so test fakes and older
// resolvers keep compiling; remotemcp.Service implements it. The row is the
// OWNER's: for a shared connection the grantee's run marks the owner's row,
// because it is the owner's token that died — the same row the refresh path
// already marks on invalid_grant.
type RemoteMCPStatusMarker interface {
	MarkRemoteMCPUnauthorized(ctx context.Context, ownerEmail, serverID, detail string) error
}

// RemoteMCPSelection says which of a user's hosted connections a run mounts,
// and on which seat (#988). It carries public names and labels only.
type RemoteMCPSelection struct {
	// Filter restricts mounting to the names in Enabled (the interactive
	// per-conversation opt-in). False mounts every connected name — the
	// scheduled default — each on its default seat unless Accounts pins one.
	Filter  bool
	Enabled map[string]bool
	// Accounts pins a seat per connection name (name → label). An absent
	// key, or "" when !Exact, mounts the name's default seat.
	Accounts map[string]string
	// Exact makes every mounted name use Accounts[name] literally — "" is
	// then the UNLABELED seat, not the default. Approval re-execution uses it
	// to reopen the seat a card recorded even if the default has since moved.
	Exact bool
}

// wants reports whether sel mounts the connection name at all. The persisted
// opt-in list is canonically lowercase (the HTTP layer normalizes on write)
// while remote names keep the case the user typed, so check the lowercased
// form too — exact match first so any pre-existing list entry keeps working.
func (sel RemoteMCPSelection) wants(name string) bool {
	if !sel.Filter {
		return true
	}
	return sel.Enabled[name] || sel.Enabled[strings.ToLower(name)]
}

// pinned returns the seat label sel demands for name and whether it demands
// one at all (false = "the default seat").
func (sel RemoteMCPSelection) pinned(name string) (string, bool) {
	acct, ok := sel.Accounts[name]
	if !ok {
		acct, ok = sel.Accounts[strings.ToLower(name)]
	}
	if sel.Exact {
		return acct, true
	}
	if !ok || acct == "" {
		return "", false
	}
	return acct, true
}

// RemoteMCPAllConnected is the scheduled default: every connected name on its
// default seat.
var RemoteMCPAllConnected = RemoteMCPSelection{}

// RemoteMCPEnabledOnly builds the interactive selection: only the names the
// conversation opted into, on the seat accounts pins (or the default).
func RemoteMCPEnabledOnly(enabledNames []string, accounts map[string]string) RemoteMCPSelection {
	enabled := make(map[string]bool, len(enabledNames))
	for _, name := range enabledNames {
		if n := strings.TrimSpace(name); n != "" {
			enabled[n] = true
		}
	}
	return RemoteMCPSelection{Filter: true, Enabled: enabled, Accounts: accounts}
}

// selectRemoteSeats picks exactly one seat per wanted connection name. A
// pinned label that no connected seat carries yields the name in missing
// (public registered name) instead of falling back to another seat — a run
// must never silently transact as a different account. Order follows conns.
func selectRemoteSeats(conns []RemoteMCPConn, sel RemoteMCPSelection) (chosen []RemoteMCPConn, missing []string) {
	var order []string
	byName := map[string][]RemoteMCPConn{}
	for _, c := range conns {
		if _, seen := byName[c.Name]; !seen {
			order = append(order, c.Name)
		}
		byName[c.Name] = append(byName[c.Name], c)
	}
	for _, name := range order {
		if !sel.wants(name) {
			continue
		}
		seats := byName[name]
		if acct, pin := sel.pinned(name); pin {
			found := false
			for _, c := range seats {
				if c.Account == acct {
					chosen = append(chosen, c)
					found = true
					break
				}
			}
			if !found {
				missing = append(missing, agentcore.RegisteredMCPName(name, acct))
			}
			continue
		}
		chosen = append(chosen, defaultRemoteSeat(seats))
	}
	return chosen, missing
}

// defaultRemoteSeat is the seat a name mounts when nothing pins one: the one
// flagged default, else the unlabeled seat, else the first connected.
// (Legacy single-row connections are flagged default by the #988 migration,
// so they resolve exactly as before.)
func defaultRemoteSeat(seats []RemoteMCPConn) RemoteMCPConn {
	for _, c := range seats {
		if c.Default {
			return c
		}
	}
	for _, c := range seats {
		if c.Account == "" {
			return c
		}
	}
	return seats[0]
}

// RemoteMCPSeatGroup is one connection NAME with its seats, the shape the
// pickers (chat Tools picker, task modal) present: the user toggles/pins by
// name, then picks a seat (#988).
type RemoteMCPSeatGroup struct {
	Name string
	URL  string // the default seat's URL (display only)
	// Accounts lists the labeled seats (non-empty labels), sorted. The
	// unlabeled seat is not listed — it is reachable only as the default.
	Accounts []string
	// DefaultAccount is the label of the seat a run mounts when nothing pins
	// one ("" = the unlabeled seat).
	DefaultAccount string
	// Owner is non-empty when EVERY seat of this name was shared with the
	// user (display attribution); mixed groups report the user's own.
	Owner string
}

// GroupRemoteMCPSeats collapses per-seat connections into one group per name,
// preserving first-seen order. Callers that need per-seat rows (Settings →
// Connections) keep using the flat list.
func GroupRemoteMCPSeats(conns []RemoteMCPConn) []RemoteMCPSeatGroup {
	var order []string
	byName := map[string][]RemoteMCPConn{}
	for _, c := range conns {
		if _, seen := byName[c.Name]; !seen {
			order = append(order, c.Name)
		}
		byName[c.Name] = append(byName[c.Name], c)
	}
	out := make([]RemoteMCPSeatGroup, 0, len(order))
	for _, name := range order {
		seats := byName[name]
		def := defaultRemoteSeat(seats)
		g := RemoteMCPSeatGroup{Name: name, URL: def.URL, DefaultAccount: def.Account, Accounts: []string{}, Owner: def.Owner}
		for _, c := range seats {
			if c.Account != "" {
				g.Accounts = append(g.Accounts, c.Account)
			}
			if c.Owner == "" {
				g.Owner = ""
			}
		}
		sort.Strings(g.Accounts)
		out = append(out, g)
	}
	return out
}

// RemoteMCPOverlayOpener binds one user's public remote-server selection to a
// per-run broker overlay. Implementations may keep the historical in-process
// client or open a scope in a credential-owning subprocess; callers see only
// public tool metadata and the agentcore call seam.
type RemoteMCPOverlayOpener func(ctx context.Context, email string, shadowed map[string]bool, sel RemoteMCPSelection) (*RemoteMCPOverlay, error)

// maxOverlayServers caps how many remote servers one user can inject into a
// single run, a guard against blowing the 128-tool ceiling (and against a
// pathological number of per-turn handshakes). Servers the run named (a
// scheduled task's mcp_selection, a chat's seat pins) are mounted first, so
// the cap only ever cuts servers nobody asked for by name; every server it
// cuts is recorded as skipped with SkipReasonOverlayCap, so the prompt notice
// names it and a pinned name past the cap reads as "left out", never as
// "misspelled" (#1656).
const maxOverlayServers = 8

// MaxOverlayServers is maxOverlayServers for the notices other packages
// render (the scheduled runner's [notice] prefix), so the number a user is
// told is the one the overlay applies.
const MaxOverlayServers = maxOverlayServers

// MaxSkipNoticeNames bounds how many connector names one reason group of a
// skip notice spells out before it says "and N more". Every connection past
// the overlay cap is recorded as skipped, and a user can own or be shared any
// number of connections, so an unbounded list would let one account grow the
// system or task prompt without limit — the cost the cap exists to bound.
// The overlay's Skipped list stays complete: only the prompt text is capped,
// so a pinned name past the cap still resolves as "left out".
const MaxSkipNoticeNames = 10

// JoinSkipNoticeNames joins names for a skip notice, listing at most
// MaxSkipNoticeNames of them and counting the rest.
func JoinSkipNoticeNames(names []string) string {
	if len(names) <= MaxSkipNoticeNames {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s, and %d more", strings.Join(names[:MaxSkipNoticeNames], ", "), len(names)-MaxSkipNoticeNames)
}

// RemoteMCPOverlay is the per-run wiring for a user's remote servers. Client is
// retained for the in-process compatibility path; Broker and CloseScope allow
// the same overlay to be owned across a process boundary. The caller MUST close
// the overlay at run end.
type RemoteMCPOverlay struct {
	Client  *mcp.Client // per-run compatibility client
	Broker  agentcore.MCPBroker
	Catalog []mcp.ServerTool // the overlay servers' tools, merged into the run catalog
	Servers map[string]bool  // registration names handled by the overlay broker
	// Seats maps each registration name in Servers to the public
	// {connection name, account label} it mounted (#988), so approval staging
	// can record the exact seat a later approval must reopen.
	Seats map[string]agentcore.MCPChoice
	// CloseScope releases a broker-owned scope. It is called with a fresh,
	// bounded context so cancellation of the run cannot suppress cleanup.
	CloseScope func(context.Context) error
	// Skipped names servers that were selected but could not be wired this run.
	// Callers surface these to the owner (a needs-reauth server silently doing
	// nothing is a correctness trap, especially for headless runs). SkipReasons
	// says WHY, per name, because the right advice differs: a login that needs
	// re-authorizing is fixed by reconnecting, a vendor that did not respond is
	// not — telling the user to reconnect a working connection was F10 (#1006).
	Skipped []string
	// SkipReasons maps a Skipped name to one of the SkipReason* classes. Nil
	// when nothing was skipped; a name missing here reads as SkipReasonUnknown.
	SkipReasons map[string]string
	// SkippedSeats maps a Skipped registration name to the public
	// {connection name, account label} it stood for, the way Seats does for
	// mounted ones. A scheduled task pins a bare connection name, and when
	// that connection's default seat carries a label the registration name
	// it was skipped under is "name_label" — without this map the pin check
	// would read a skipped-but-known connection as one the owner never had
	// (#1656). Nil when nothing was skipped by connection.
	SkippedSeats map[string]agentcore.MCPChoice
}

// The classes a skipped hosted connection can fall into.
const (
	// SkipReasonNeedsReauth: the stored credential is gone or the vendor
	// refused it (HTTP 401 at mount, a terminal refresh failure). Reconnecting
	// fixes it.
	SkipReasonNeedsReauth = "needs_reauth"
	// SkipReasonUnreachable: the vendor did not answer, or answered with
	// something that is not a credential refusal (a 5xx, a timeout, a TLS or
	// DNS failure, a JSON-RPC error at initialize). Reconnecting changes
	// nothing; the user waits or the operator looks at the vendor.
	SkipReasonUnreachable = "unreachable"
	// SkipReasonSeatNotConnected: the run pinned an account that is not
	// connected under that name. The owner connects (or re-pins) it.
	SkipReasonSeatNotConnected = "seat_not_connected"
	// SkipReasonUnknown: fleet could not say — a credential that could not
	// be read from the store, an overlay built without reasons. The notice
	// then asserts nothing about the login or the vendor.
	SkipReasonUnknown = "unknown"
	// SkipReasonOverlayCap: the run had more connected servers than
	// maxOverlayServers and this one was past the cap. Nothing is wrong with
	// the connection; the user turns off connectors they do not need, or
	// names the ones they do (named servers mount first).
	SkipReasonOverlayCap = "overlay_cap"
)

// skipConn records a selected connection as skipped under its registration
// name, with its reason class and the seat it stood for.
func (o *RemoteMCPOverlay) skipConn(conn RemoteMCPConn, reason string) {
	regName := agentcore.RegisteredMCPName(conn.Name, conn.Account)
	o.skip(regName, reason)
	if o.SkippedSeats == nil {
		o.SkippedSeats = map[string]agentcore.MCPChoice{}
	}
	o.SkippedSeats[regName] = agentcore.MCPChoice{Server: conn.Name, Account: conn.Account}
}

// skip records a name in Skipped with its reason class.
func (o *RemoteMCPOverlay) skip(name, reason string) {
	o.Skipped = append(o.Skipped, name)
	if o.SkipReasons == nil {
		o.SkipReasons = map[string]string{}
	}
	o.SkipReasons[name] = reason
}

// skippedWithReasons renders "name (reason), …" for a log line, bounded like
// the prompt notices (MaxSkipNoticeNames, then "and N more"): every
// connection past the overlay cap is skipped, so an unbounded line would grow
// with the size of the account on every turn and run.
func skippedWithReasons(o *RemoteMCPOverlay) string {
	n := len(o.Skipped)
	if n > MaxSkipNoticeNames+1 {
		n = MaxSkipNoticeNames + 1 // one past the bound is enough for "and N more"
	}
	parts := make([]string, 0, n)
	for _, name := range o.Skipped[:n] {
		parts = append(parts, name+" ("+o.SkipReason(name)+")")
	}
	if len(o.Skipped) > MaxSkipNoticeNames {
		return fmt.Sprintf("%s, and %d more", strings.Join(parts[:MaxSkipNoticeNames], ", "), len(o.Skipped)-MaxSkipNoticeNames)
	}
	return strings.Join(parts, ", ")
}

// SkippedForLog is skippedWithReasons for callers outside this package (the
// scheduled-run notice logs the same list).
func SkippedForLog(o *RemoteMCPOverlay) string { return skippedWithReasons(o) }

// SkipReason returns the class recorded for a skipped name. A name without a
// recorded class — an overlay built without reasons, or a class this build
// does not know — is SkipReasonUnknown, never a guess: the notice must not
// tell the model "not the login" or "reconnect" on no evidence.
func (o *RemoteMCPOverlay) SkipReason(name string) string {
	if o == nil || o.SkipReasons == nil {
		return SkipReasonUnknown
	}
	switch r := o.SkipReasons[name]; r {
	case SkipReasonNeedsReauth, SkipReasonUnreachable, SkipReasonSeatNotConnected, SkipReasonOverlayCap:
		return r
	}
	return SkipReasonUnknown
}

// named reports whether the selection names the server at all — a scheduled
// task's mcp_selection entry or a chat seat override — regardless of which
// seat it asks for. pinned() is narrower (it answers "which seat"); this is
// the question the overlay cap needs: was this server asked for by name?
func (sel RemoteMCPSelection) named(name string) bool {
	if _, ok := sel.Accounts[name]; ok {
		return true
	}
	_, ok := sel.Accounts[strings.ToLower(name)]
	return ok
}

// namedFirst orders the chosen connections so the ones the selection names
// come first, in their original order, then the rest in theirs. With the
// overlay cap applied in this order a named server is cut only when more
// than maxOverlayServers are named, which is the one case the user must
// resolve by hand.
func namedFirst(chosen []RemoteMCPConn, sel RemoteMCPSelection) []RemoteMCPConn {
	if len(sel.Accounts) == 0 {
		return chosen
	}
	out := make([]RemoteMCPConn, 0, len(chosen))
	for _, c := range chosen {
		if sel.named(c.Name) {
			out = append(out, c)
		}
	}
	for _, c := range chosen {
		if !sel.named(c.Name) {
			out = append(out, c)
		}
	}
	return out
}

// connectSkipReason classifies a mount failure the way recordRefusedMount
// does: only a vendor refusing the credential is a re-auth matter.
func connectSkipReason(err error) string {
	var hs *mcp.HTTPStatusError
	if errors.As(err, &hs) && hs.Unauthorized() {
		return SkipReasonNeedsReauth
	}
	return SkipReasonUnreachable
}

// tokenSkipReason classifies a credential-acquisition failure: the store's
// needs-reauth sentinel (a terminal refresh failure, a signed-out row) is a
// re-auth matter. Anything else — a store read that failed, a refresh whose
// transport failed — says nothing either way, so it is unknown rather than
// "unreachable", which would tell the model the login is fine. The sentinel
// is recognised through its NeedsReauth method rather than by identity
// because this package cannot import internal/store (store imports it);
// store.ErrRemoteMCPNeedsReauth implements the method for exactly this.
func tokenSkipReason(err error) string {
	var nr interface{ NeedsReauth() bool }
	if errors.As(err, &nr) && nr.NeedsReauth() {
		return SkipReasonNeedsReauth
	}
	return SkipReasonUnknown
}

// recordRefusedMount flips the connection to needs_reauth when the mount
// failed because the server REFUSED the credential (HTTP 401) — and only
// then: a 5xx, a timeout or a TLS failure says nothing about the token, and
// marking those would send the user to re-authorize a connection that is
// fine. Best-effort: a store failure is logged, never fails the turn.
func recordRefusedMount(ctx context.Context, resolver RemoteMCPResolver, email, regName string, conn RemoteMCPConn, err error) {
	var hs *mcp.HTTPStatusError
	if !errors.As(err, &hs) || !hs.Unauthorized() {
		return
	}
	marker, ok := resolver.(RemoteMCPStatusMarker)
	if !ok {
		return
	}
	owner := conn.Owner
	if owner == "" {
		owner = email
	}
	detail := fmt.Sprintf("the server rejected the stored credential (HTTP %d) — reconnect, or update the key, to use", hs.StatusCode)
	if merr := marker.MarkRemoteMCPUnauthorized(ctx, owner, conn.ID, detail); merr != nil {
		log.Printf("remote-mcp: could not record the credential rejection for %q: %v", regName, merr)
	}
}

// connectFailureReason renders a hosted-server connect error for the skip
// log line. credential is the value that rode the failed request (the bearer,
// the api_key header value or the query-parameter key — the same string in
// every case; empty when the server took none). The error can quote a
// user-supplied server's response, which is untrusted input into an
// operator's terminal and into this process's memory, so in order it is:
//
//   - cut to a bounded window — a JSON-RPC error near the transport's 64 MiB
//     cap must not be tokenised, joined, scanned or REWRITTEN whole for a
//     240-byte log line (a one-byte key replaced by a ten-byte placeholder
//     across 64 MiB would be a 600 MiB allocation per mount attempt). The
//     window is the input bound plus the longest wire form of the credential,
//     so an occurrence that begins inside the bound is always inside the
//     window whole; see maskCredential for how the window's ragged end is
//     kept from leaking a prefix;
//   - stripped of the request's own credential FIRST, in every form a
//     transport or a vendor's encoder can have given it (maskCredential lists
//     them). This is a plain ReplaceAll per form — no patterns — and it is
//     what the two redactor passes below cannot be relied on for: the
//     redactors' literal floor (internal/redact minLiteralLen) ignores a key
//     under 8 bytes that validateAPIKeyAuth accepts, and they know only the
//     raw value, not its URL, JSON or header-trimmed spellings. Nothing else
//     secret is in this request for the vendor to echo;
//   - redacted by BOTH process-wide redactors, on the text AS SENT — before
//     any whitespace is touched, because a registered literal is matched
//     byte-for-byte and an API key may legitimately contain runs of spaces
//     (validateAPIKeyAuth admits any printable ASCII); collapsing first would
//     turn the echoed key into a string the redactor no longer recognises.
//     This code runs in the main process (agentcore's redactor holds its
//     literals) and, in production, in the credential-owning broker, where the
//     bearer that rode the failed request was registered with mcpbroker's
//     redactor when it was acquired (#1274, cmd/fleet/mcp_broker.go) —
//     agentcore's set in that process never sees it. Each redactor also
//     matches the canonical token shapes;
//   - stripped of control characters, so an ANSI sequence in the body cannot
//     repaint the terminal or forge a line;
//   - whitespace-collapsed, then redacted once more, so a canonical token
//     shape that only lines up after the normalisation is caught too;
//   - bounded on a rune boundary, so the line names the failure class without
//     becoming a transcript of the vendor's response body.
//
// The input bound is applied before the redactor passes, so a REGISTERED
// literal other than this request's credential could survive only if it began
// inside the first 240 bytes and ran past the 8 KiB cut — and no such literal
// is in this request for the vendor to echo in the first place.
func connectFailureReason(credential string, err error) string {
	if err == nil {
		return ""
	}
	const (
		maxInput  = 8 << 10 // bytes of the raw error considered at all
		maxReason = 240     // bytes of the rendered reason
	)
	raw := maskCredential(err.Error(), credential, maxInput)
	raw = redactBoth(raw) // on the bytes as sent: literals match verbatim only
	raw = strings.Map(func(r rune) rune {
		if unicode.IsPrint(r) {
			return r
		}
		return ' ' // controls, escapes and invalid bytes become separators
	}, raw)
	s := redactBoth(strings.Join(strings.Fields(raw), " "))
	if len(s) > maxReason {
		s = truncateAtRune(s, maxReason) + "…"
	}
	return s
}

// maskCredential returns at most bound bytes of text with every occurrence
// of the credential that rode the failed request replaced, whatever its
// length, in each form the wire can have given it:
//
//   - as sent (Authorization / api_key header value; query parameter value);
//   - header-trimmed: Go's HTTP transport strips leading and trailing
//     whitespace from a header value when it writes it, and
//     validateAPIKeyAuth admits a key with either, so the vendor saw and
//     echoes the trimmed spelling;
//   - url.QueryEscape'd (mcp.WithQueryParam builds the query with
//     url.Values.Encode, so a space becomes `+`), the `%20` spelling of that,
//     and url.PathEscape'd — how a vendor that echoes its request URI shows
//     the key;
//   - JSON-string-escaped, with and without HTML escaping: the transport's
//     parseJSONResponse embeds the vendor's raw error object in the error
//     text, where a key containing `"` or `\` appears as `\"` / `\\`.
//
// The credential is masked before the input is bounded, but on a WINDOW, not
// the whole text: bound plus the longest form, so an occurrence that begins
// inside the bound lies inside the window whole and is replaced, while a
// 64 MiB response is never rewritten end to end (a one-byte key would
// otherwise turn into 600 MiB of placeholders). What the window's far edge
// can cut is an occurrence that begins beyond the bound; its surviving part
// is a proper prefix of one form sitting at the very end of the text, so
// after the window is cut back to bound any trailing proper prefix of a form
// is dropped. That is at most a few bytes off the end of an 8 KiB window that
// renders as 240, and it is what makes "no partial credential" hold without
// scanning the whole response.
func maskCredential(text, credential string, bound int) string {
	if credential == "" {
		return truncateAtRune(text, bound)
	}
	forms := credentialWireForms(credential)
	longest := 0
	for _, f := range forms {
		longest = max(longest, len(f))
	}
	window := truncateAtRune(text, bound+longest)
	for _, form := range forms {
		window = strings.ReplaceAll(window, form, "[REDACTED]")
	}
	window = truncateAtRune(window, bound)
	// Drop a trailing proper prefix of any form (see above). Longest first,
	// so "abc" is removed as one piece rather than leaving "ab".
	for _, form := range forms {
		for k := min(len(form)-1, len(window)); k > 0; k-- {
			if strings.HasSuffix(window, form[:k]) {
				window = window[:len(window)-k]
				break
			}
		}
	}
	return window
}

// credentialWireForms lists the distinct spellings of credential that a
// transport or a vendor's encoder can put into an error message, longest
// first so a longer form is never left half-masked by a shorter one.
func credentialWireForms(credential string) []string {
	var forms []string
	seen := map[string]bool{}
	add := func(f string) {
		if f != "" && !seen[f] {
			seen[f] = true
			forms = append(forms, f)
		}
	}
	for _, base := range []string{credential, strings.TrimSpace(credential)} {
		if base == "" {
			continue
		}
		add(base)
		query := url.QueryEscape(base)
		add(query)
		add(strings.ReplaceAll(query, "+", "%20"))
		add(url.PathEscape(base))
		if b, err := json.Marshal(base); err == nil && len(b) >= 2 {
			add(string(b[1 : len(b)-1]))
		}
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(base); err == nil {
			if q := strings.TrimSuffix(buf.String(), "\n"); len(q) >= 2 {
				add(q[1 : len(q)-1])
			}
		}
	}
	sort.SliceStable(forms, func(i, j int) bool { return len(forms[i]) > len(forms[j]) })
	return forms
}

// redactBoth runs text through the main process's redactor and the broker's;
// whichever process this runs in, the one holding the acquired credential's
// literal is among them.
func redactBoth(text string) string {
	return mcpbroker.RedactSecrets(agentcore.RedactSecrets(text))
}

// truncateAtRune cuts s to at most n bytes without splitting a multibyte rune.
func truncateAtRune(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// skippedNames is a nil-safe read of Skipped for error text.
func (o *RemoteMCPOverlay) skippedNames() []string {
	if o == nil {
		return nil
	}
	return o.Skipped
}

// Active reports whether the overlay actually registered any servers.
func (o *RemoteMCPOverlay) Active() bool {
	return o != nil && (o.Broker != nil || o.Client != nil) && len(o.Servers) > 0
}

// hostedMCPRoster is what the system prompt builder needs to know about this
// turn's per-user hosted (remote) MCP overlay that the tool roster cannot tell
// it: the registration names of the selected connections whose token could not
// be acquired, that failed to connect, or that fell past maxOverlayServers
// (Skipped, each with its reason class). The tools the overlay DID mount are
// not carried here any more: the "MCP Tools (live registry)" section is
// appended by agentcore.Run from the roster it actually registered
// (agentcore/live_registry.go), which is the only source that also knows
// whether that roster was deferred behind the disclosure bridges (#1006).
type hostedMCPRoster struct {
	// skipped are the registration names of selected connections that could
	// not be mounted, sorted, each with its SkipReason* class, so the model can
	// tell the user the right thing: reconnect for a dead login, wait or retry
	// for a vendor that did not answer — never "reconnect" for the latter (F10).
	skipped []skippedConnector
}

// skippedConnector is one entry of hostedMCPRoster.skipped.
type skippedConnector struct {
	name   string
	reason string
}

// hostedRosterFromOverlay derives the prompt notice from an opened overlay.
// Nil-safe; an overlay with nothing skipped yields the zero value.
//
// Every name is passed through promptSafeName. A hosted connection's name is
// user-authored (the add path trims it and requires it non-empty, nothing
// more) and a connection can be shared to other users, so a name is a channel
// by which one user could place text into another user's SYSTEM prompt. The
// tool-name grammar the model APIs enforce is the natural bound: a legitimate
// name is unchanged, anything else is reduced to that grammar.
func hostedRosterFromOverlay(o *RemoteMCPOverlay) hostedMCPRoster {
	if o == nil {
		return hostedMCPRoster{}
	}
	var r hostedMCPRoster
	for _, name := range o.Skipped {
		r.skipped = append(r.skipped, skippedConnector{name: promptSafeName(name), reason: o.SkipReason(name)})
	}
	// Stable, and keyed on the reason too: two raw names that reduce to the
	// same prompt-safe name must not swap between builds (prompt-cache bytes).
	sort.SliceStable(r.skipped, func(i, j int) bool {
		if r.skipped[i].name != r.skipped[j].name {
			return r.skipped[i].name < r.skipped[j].name
		}
		return r.skipped[i].reason < r.skipped[j].reason
	})
	return r
}

// promptSafeName reduces a hosted registration or tool name to the identifier
// grammar model APIs accept for tool names — ASCII letters, digits, `_`, `-`
// and `.`, at most 64 bytes — replacing anything else with `_`. A name that
// already fits comes back unchanged; one that does not could never have been
// a callable tool anyway, and must not reach the system prompt as written.
func promptSafeName(name string) string { return agentcore.PromptSafeName(name) }

// Validate checks the ownership contract an injected opener must satisfy. A
// broker-backed overlay always represents a per-run scope and therefore needs
// an explicit release function; selected routing names need a call target.
func (o *RemoteMCPOverlay) Validate() error {
	if o == nil {
		return nil
	}
	if o.Broker != nil && o.CloseScope == nil {
		return errors.New("remote MCP broker overlay has no close function")
	}
	if len(o.Servers) > 0 && o.Broker == nil && o.Client == nil {
		return errors.New("remote MCP overlay has routing names but no call broker")
	}
	return nil
}

const remoteMCPOverlayCloseTimeout = 5 * time.Second

// Close tears down the overlay's per-run client or broker scope (nil-safe). It
// deliberately uses a fresh bounded context: callers commonly defer it from a
// run whose context may already be cancelled.
func (o *RemoteMCPOverlay) Close() {
	if o == nil {
		return
	}
	if o.CloseScope != nil {
		ctx, cancel := context.WithTimeout(context.Background(), remoteMCPOverlayCloseTimeout)
		defer cancel()
		if err := o.CloseScope(ctx); err != nil {
			log.Printf("remote-mcp: close overlay scope: %v", err)
		}
	}
	if o.Client != nil {
		_ = o.Client.Close()
	}
}

// BuildRemoteMCPOverlay registers a user's connected remote servers onto a fresh
// per-run client. shadowed is the set of server names already provided by the
// base catalog — an overlay server colliding with one is skipped so a user can
// never shadow a built-in tool. sel says which connection names mount (the
// conversation's opt-in set, or every connected name for a scheduled run) and
// which seat each one uses (#988): exactly ONE seat per name is mounted,
// registered under agentcore.RegisteredMCPName(name, account), and a pinned
// seat that is not connected is reported in Skipped rather than replaced by
// another account. A server that fails to mint a token (needs re-auth) or
// connect is likewise recorded in Skipped (graceful degradation), never
// fatal. The returned overlay is non-nil whenever there are connected servers
// (so the caller can read Skipped even when none registered); its Active()
// reports whether any server is actually wired. The caller MUST Close it.
func BuildRemoteMCPOverlay(ctx context.Context, resolver RemoteMCPResolver, email string, shadowed map[string]bool, sel RemoteMCPSelection) (*RemoteMCPOverlay, error) {
	if resolver == nil || email == "" {
		return nil, nil
	}
	conns, err := resolver.ConnectedServersForUser(ctx, email)
	if err != nil {
		return nil, err
	}
	if len(conns) == 0 {
		return nil, nil
	}

	client := mcp.NewClient()
	httpClient := resolver.SafeHTTPClient()
	overlay := &RemoteMCPOverlay{Client: client, Servers: map[string]bool{}, Seats: map[string]agentcore.MCPChoice{}}
	chosen, missing := selectRemoteSeats(conns, sel)
	for _, name := range missing {
		// A pinned seat that is not connected: never fall back to another
		// account under the same name — surface it so the owner can connect
		// (or re-pin) it.
		log.Printf("remote-mcp: skipping %q for %s — the pinned seat is not connected", name, email)
		overlay.skip(name, SkipReasonSeatNotConnected)
	}
	registered := 0
	capLogged := false
	for _, conn := range namedFirst(chosen, sel) {
		regName := agentcore.RegisteredMCPName(conn.Name, conn.Account)
		if shadowed[conn.Name] || shadowed[regName] {
			// A collision never takes a slot, so it is checked before the cap:
			// past the cap it would otherwise be reported as a connector the
			// user could free a slot for, which it can never use.
			log.Printf("remote-mcp: skipping remote server %q — name collides with a built-in server", regName)
			continue
		}
		if registered >= maxOverlayServers {
			// Past the cap: record every one (the notice names them, and a
			// pinned name here is "left out", not "unknown"), log once.
			if !capLogged {
				log.Printf("remote-mcp: overlay cap %d reached for %s — skipping %q and every further server", maxOverlayServers, email, regName)
				capLogged = true
			}
			overlay.skipConn(conn, SkipReasonOverlayCap)
			continue
		}
		if conn.Owner != "" {
			// Attribution for shared connections: the run belongs to email, but
			// tool calls authenticate with the OWNER's token host-side.
			log.Printf("remote-mcp: run for %s uses shared server %q owned by %s", email, regName, conn.Owner)
		}
		bearer, terr := resolver.AcquireTokenByID(ctx, email, conn.ID)
		if terr != nil {
			// needs-reauth / refresh failure: skip this server, keep the rest, and
			// record it so the caller can tell the owner.
			log.Printf("remote-mcp: skipping server %q for %s — token unavailable", regName, email)
			overlay.skipConn(conn, tokenSkipReason(terr))
			continue
		}
		opts := mcp.HTTPServerOptions{HTTPClient: httpClient}
		if bearer != "" {
			switch {
			case conn.AuthQuery != "":
				// Query-authenticated vendor: attach the key in the transport so
				// the registered URL (and every log/error that embeds it) stays
				// credential-free.
				opts.HTTPClient = mcp.WithQueryParam(httpClient, conn.AuthQuery, bearer)
			case conn.AuthHeader != "":
				// api_key connection with a vendor-specific header: the raw key,
				// no Bearer scheme.
				opts.Headers = map[string]string{conn.AuthHeader: bearer}
			default:
				opts.Headers = map[string]string{"Authorization": "Bearer " + bearer}
			}
		}
		if aerr := client.AddHTTPServerWithOptions(ctx, regName, conn.URL, opts); aerr != nil {
			// The reason matters: a 401 from the vendor (dead token, revoked
			// grant, org approval pending), a TLS or DNS failure and a handshake
			// timeout each want a different operator action, and a value-free
			// "failed to connect" left the GitHub verification in #1006 blind
			// to which one it was. The wire to the parent still carries only the
			// public name — this stays a host-side log line.
			log.Printf("remote-mcp: skipping server %q for %s — failed to connect: %s", regName, email, connectFailureReason(bearer, aerr))
			overlay.skipConn(conn, connectSkipReason(aerr))
			recordRefusedMount(ctx, resolver, email, regName, conn, aerr)
			continue
		}
		overlay.Servers[regName] = true
		overlay.Seats[regName] = agentcore.MCPChoice{Server: conn.Name, Account: conn.Account}
		registered++
	}

	overlay.Catalog = client.GetAllTools()
	return overlay, nil
}

// SeatSelection returns the public {connection name, account} of every seat
// the overlay mounted, ordered by registration name, for approval staging
// (#988): the stager keys seats by agentcore.RegisteredMCPName, which is
// exactly the name each seat registered under.
func (o *RemoteMCPOverlay) SeatSelection() agentcore.MCPSelection {
	if o == nil || len(o.Seats) == 0 {
		return nil
	}
	names := make([]string, 0, len(o.Seats))
	for name := range o.Seats {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make(agentcore.MCPSelection, 0, len(names))
	for _, name := range names {
		out = append(out, o.Seats[name])
	}
	return out
}

// ComposeWith returns the call seam + public catalog a run sees once this
// overlay is layered over a base broker/catalog: overlay names route to the
// overlay, everything else to the base. Shared by ApplyMCPOverlayWithBase and
// the approval stager rebind so staging resolves remote tools against the
// same composite the loop dispatches on. An inactive overlay returns the base
// unchanged.
func (o *RemoteMCPOverlay) ComposeWith(baseBroker agentcore.MCPBroker, baseCatalog []mcp.ServerTool) (agentcore.MCPBroker, []mcp.ServerTool) {
	if !o.Active() {
		return baseBroker, baseCatalog
	}
	merged := append([]mcp.ServerTool(nil), baseCatalog...)
	merged = append(merged, o.Catalog...)
	if baseBroker == nil {
		return o.CallBroker(), merged
	}
	return &compositeBroker{
		overlay:        o.callBroker(agentcore.DefaultRemediationHints),
		overlayServers: o.Servers,
		base:           baseBroker,
	}, merged
}

// CallBroker is the overlay's own call seam (a broker-owned scope, or the
// in-process compatibility client wrapped as a local broker).
func (o *RemoteMCPOverlay) CallBroker() agentcore.MCPBroker {
	return o.callBroker(agentcore.DefaultRemediationHints)
}

// ApplyMCPOverlayWithBase composes a per-user remote overlay with either the
// historical local base client or an injected out-of-process base broker and
// catalog. A remote server can still never shadow a base server; only the base
// call location changes.
func ApplyMCPOverlayWithBase(
	deps *agentcore.Deps,
	baseClient *mcp.Client,
	baseBroker agentcore.MCPBroker,
	baseCatalog []mcp.ServerTool,
	overlay *RemoteMCPOverlay,
) {
	if deps == nil || !overlay.Active() {
		return
	}
	hints := agentcore.DefaultRemediationHints
	if baseBroker == nil {
		if baseClient == nil {
			return
		}
		baseBroker = agentcore.NewLocalMCPBroker(baseClient, hints)
	}
	if baseCatalog == nil && baseClient != nil {
		baseCatalog = baseClient.GetAllTools()
	}
	deps.MCPBroker = &compositeBroker{
		overlay:        overlay.callBroker(hints),
		overlayServers: overlay.Servers,
		base:           baseBroker,
	}
	merged := append([]mcp.ServerTool(nil), baseCatalog...)
	merged = append(merged, overlay.Catalog...)
	deps.MCPCatalog = merged
}

func (o *RemoteMCPOverlay) callBroker(hints agentcore.RemediationHints) agentcore.MCPBroker {
	if o.Broker != nil {
		return o.Broker
	}
	return agentcore.NewLocalMCPBroker(o.Client, hints)
}

// compositeBroker routes an MCP call to the per-user overlay broker when the
// server name belongs to the overlay, and to the base (shared) broker otherwise.
// It implements agentcore.MCPBroker so it slots into Deps.MCPBroker without
// forking the governed loop.
type compositeBroker struct {
	overlay        agentcore.MCPBroker
	overlayServers map[string]bool
	base           agentcore.MCPBroker
}

func (b *compositeBroker) CallMCP(ctx context.Context, server, tool string, args map[string]any) (string, bool, error) {
	if b.overlayServers[server] {
		return b.overlay.CallMCP(ctx, server, tool, args)
	}
	return b.base.CallMCP(ctx, server, tool, args)
}

// browserbaseHost is the vendor host of the Browserbase hosted MCP endpoint.
// The connector is matched on its URL rather than its registration name because
// the name is whatever the user typed when they added it ("bb", "Browserbase",
// anything) — the URL is what actually identifies the vendor.
const browserbaseHost = "browserbase.com"

// browserbaseKeyFunc returns a resolver for THIS user's Browserbase connector
// credential, or nil when one is not genuinely reachable for this turn.
//
// Returning nil matters twice over, because a non-nil func is what registers
// browserbase_live_view:
//
//   - It keeps a permanently-failing tool away from the majority of users, who
//     have no Browserbase connection at all.
//   - It keeps the credential, and the session enumeration it enables, inside the
//     per-conversation connector gate. openRemoteOverlay restricts remote servers
//     to the conversation's opt-in set; a key resolver that ignored that set would
//     let a chat with Browserbase switched OFF still unseal the key and list every
//     running session in the account.
//
// The cost is one extra ConnectedServersForUser read per turn (the overlay does
// its own later). That is a store read, not a decrypt: the key itself is unsealed
// only if the tool is actually called.
func (m *Manager) browserbaseKeyFunc(ctx context.Context, email string, enabledOptional []string) tools.BrowserbaseKeyFunc {
	if m.remoteMCP == nil || strings.TrimSpace(email) == "" {
		return nil
	}
	conns, err := m.remoteMCP.ConnectedServersForUser(ctx, email)
	if err != nil {
		return nil
	}
	// Same opt-in semantics as openRemoteOverlay: nil and empty both mean "no
	// connectors on" (RunTurnInput.OptionalMCPServersEnabled documents exactly
	// that, and openRemoteOverlay builds its filter map unconditionally). This
	// function's only caller is the interactive turn — scheduled runs pass a nil
	// key func straight to NewTurnTools — so there is no "unfiltered" caller,
	// and treating nil as unfiltered would unseal the key in a conversation the
	// overlay wires no connectors into (e.g. one whose opt-in list was never
	// seeded).
	enabled := make(map[string]bool, len(enabledOptional))
	for _, n := range enabledOptional {
		if n = strings.TrimSpace(n); n != "" {
			enabled[n] = true
			enabled[strings.ToLower(n)] = true
		}
	}
	for _, c := range conns {
		if !isBrowserbaseURL(c.URL) {
			continue
		}
		if !enabled[c.Name] && !enabled[strings.ToLower(c.Name)] {
			continue // connector switched off for this conversation
		}
		id := c.ID
		return func(ctx context.Context) (string, error) {
			// Own connections and ones shared with this user both work: the
			// credential is the owner's, applied host-side, exactly as it is for
			// the connector's own tool calls.
			return m.remoteMCP.AcquireTokenByID(ctx, email, id)
		}
	}
	return nil
}

// isBrowserbaseURL reports whether a connection's URL points at Browserbase.
func isBrowserbaseURL(raw string) bool {
	host := strings.ToLower(strings.TrimSpace(raw))
	// Hostname(), not Host: an explicit port (":443") must not defeat the match.
	if u, err := url.Parse(raw); err == nil && u.Hostname() != "" {
		host = strings.ToLower(u.Hostname())
	}
	return host == browserbaseHost || strings.HasSuffix(host, "."+browserbaseHost)
}
