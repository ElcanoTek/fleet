// Approval-card describers: a readable card for a staged critical call.
//
// A bundle can map a critical tool to a read-only "describer" tool on the same
// server (agent_policy.critical_tool_card_describers). When such a call is
// staged, the stager calls the describer with the SAME arguments through the
// turn's own MCP scope, bounded and without retries, and stores the structured
// result with the approval. The web renders it as a plain-words card (what
// changes, on which record, with which flags) and keeps the raw arguments
// under "Details". Fleet knows nothing about what the card describes: it only
// enforces the schema below and treats every string as text.
//
// Any failure — no describer resolvable, a timeout, a tool error, output that
// is not exactly this schema, or a card the secret redaction would alter —
// leaves the card off and the generic arguments card renders, as before.
// Describing never blocks or fails staging. See docs/APPROVAL-CARD-DESCRIBERS.md.

package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/mcp"
	"github.com/ElcanoTek/fleet/internal/store"
	"github.com/ElcanoTek/fleet/internal/tools"
)

// approvalCardDescriberTimeout bounds one describer call, the whole of it:
// the call crosses the out-of-process broker with the same budget. Var, not
// const: tests shorten it.
var approvalCardDescriberTimeout = 5 * time.Second

// Card limits. A card is display data riding the approval event, every
// pending_approvals row and every resolved_approvals row, so it is bounded
// well below the frozen-args bound.
const (
	approvalCardMaxBytes      = 128 << 10 // the describer's raw output and the stored card
	approvalCardMaxItems      = 500
	approvalCardMaxRowsPerSet = 100 // changes, settings per item
	approvalCardMaxFlags      = 20  // per item
	approvalCardMaxRows       = 5000
	approvalCardMaxTitle      = 200 // runes: title, labels, flag labels
	approvalCardMaxText       = 2000
	approvalCardMaxLink       = 2048
)

// ApprovalCard is the schema a describer must return, exactly. Unknown keys,
// wrong types and trailing data are refused (strict decode), so a describer
// cannot smuggle fields the client might start reading later.
type ApprovalCard struct {
	Title    string             `json:"title"`
	Subtitle string             `json:"subtitle,omitempty"`
	Items    []ApprovalCardItem `json:"items"`
	Footer   string             `json:"footer,omitempty"`
}

// ApprovalCardItem is one record the call touches.
type ApprovalCardItem struct {
	Label    string                `json:"label"`
	ID       string                `json:"id,omitempty"`
	Link     string                `json:"link,omitempty"`
	Changes  []ApprovalCardChange  `json:"changes,omitempty"`
	Settings []ApprovalCardSetting `json:"settings,omitempty"`
	Flags    []ApprovalCardFlag    `json:"flags,omitempty"`
}

// ApprovalCardChange is one field the call changes: before → after. Before is
// a pointer so "no previous value" (absent) differs from an empty string.
type ApprovalCardChange struct {
	Label  string  `json:"label"`
	Before *string `json:"before,omitempty"`
	After  *string `json:"after"`
}

// ApprovalCardSetting is one value the call sets that is not a change (a new
// record's settings, for example).
type ApprovalCardSetting struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

// ApprovalCardFlag is a short warning shown as a badge, e.g. code
// "deal_active", label "Deal is Active".
type ApprovalCardFlag struct {
	Code  string `json:"code"`
	Label string `json:"label"`
}

var approvalCardFlagCode = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,63}$`)

// parseApprovalCard strictly decodes and validates a describer's output and
// returns the card re-marshaled in canonical form. Every refusal is an error
// naming the first problem (for the log; the user sees the generic card).
func parseApprovalCard(raw []byte) (*ApprovalCard, []byte, error) {
	if len(raw) == 0 {
		return nil, nil, errors.New("empty output")
	}
	if len(raw) > approvalCardMaxBytes {
		return nil, nil, fmt.Errorf("output is %d bytes, over the %d-byte card limit", len(raw), approvalCardMaxBytes)
	}
	if !utf8.Valid(raw) {
		return nil, nil, errors.New("output is not valid UTF-8")
	}
	// encoding/json decodes null into a string's or a slice's zero value, so
	// a null would pass as a blank field. Nothing in the schema is nullable:
	// refuse any null before the typed decode.
	if err := refuseJSONNulls(raw); err != nil {
		return nil, nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var card ApprovalCard
	if err := dec.Decode(&card); err != nil {
		return nil, nil, fmt.Errorf("not the card schema: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, nil, errors.New("trailing data after the card")
	}
	if err := validateApprovalCard(&card); err != nil {
		return nil, nil, err
	}
	canonical, err := json.Marshal(&card)
	if err != nil {
		return nil, nil, err
	}
	if len(canonical) > approvalCardMaxBytes {
		return nil, nil, fmt.Errorf("card is %d bytes, over the %d-byte limit", len(canonical), approvalCardMaxBytes)
	}
	return &card, canonical, nil
}

// refuseJSONNulls reports a null anywhere in raw. Malformed JSON passes
// through here and is refused, with its own error, by the typed decode.
func refuseJSONNulls(raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	decodeErr := dec.Decode(&v)
	var walk func(any) bool
	walk = func(x any) bool {
		switch t := x.(type) {
		case nil:
			return true
		case []any:
			for _, e := range t {
				if walk(e) {
					return true
				}
			}
		case map[string]any:
			for _, e := range t {
				if walk(e) {
					return true
				}
			}
		}
		return false
	}
	if decodeErr == nil && walk(v) {
		return errors.New("a field is null; no card field is nullable")
	}
	return nil
}

// redactedForLog passes text that may echo describer output (a validation
// error naming a field's value, a transport error) through the same secret
// redaction the loop applies, so a credential in a malformed card never
// reaches the server log.
func redactedForLog(err error) string {
	if err == nil {
		return ""
	}
	if tools.ShowUIRedactor == nil {
		return err.Error()
	}
	return tools.ShowUIRedactor(err.Error())
}

func validateApprovalCard(c *ApprovalCard) error {
	if err := cardText("title", c.Title, approvalCardMaxTitle, true); err != nil {
		return err
	}
	if err := cardText("subtitle", c.Subtitle, approvalCardMaxText, false); err != nil {
		return err
	}
	if err := cardText("footer", c.Footer, approvalCardMaxText, false); err != nil {
		return err
	}
	if c.Items == nil {
		return errors.New("items is required")
	}
	if len(c.Items) > approvalCardMaxItems {
		return fmt.Errorf("%d items, over the %d-item limit", len(c.Items), approvalCardMaxItems)
	}
	rows := 0
	for i := range c.Items {
		it := &c.Items[i]
		at := fmt.Sprintf("items[%d]", i)
		if err := cardText(at+".label", it.Label, approvalCardMaxTitle, true); err != nil {
			return err
		}
		if err := cardText(at+".id", it.ID, approvalCardMaxTitle, false); err != nil {
			return err
		}
		if it.Link != "" {
			if err := cardLink(at+".link", it.Link); err != nil {
				return err
			}
		}
		if len(it.Changes) > approvalCardMaxRowsPerSet || len(it.Settings) > approvalCardMaxRowsPerSet {
			return fmt.Errorf("%s has more than %d changes or settings", at, approvalCardMaxRowsPerSet)
		}
		if len(it.Flags) > approvalCardMaxFlags {
			return fmt.Errorf("%s has more than %d flags", at, approvalCardMaxFlags)
		}
		rows += len(it.Changes) + len(it.Settings) + len(it.Flags)
		for j, ch := range it.Changes {
			cat := fmt.Sprintf("%s.changes[%d]", at, j)
			if err := cardText(cat+".label", ch.Label, approvalCardMaxTitle, true); err != nil {
				return err
			}
			if ch.After == nil {
				return fmt.Errorf("%s.after is required", cat)
			}
			if err := cardText(cat+".after", *ch.After, approvalCardMaxText, false); err != nil {
				return err
			}
			if ch.Before != nil {
				if err := cardText(cat+".before", *ch.Before, approvalCardMaxText, false); err != nil {
					return err
				}
			}
		}
		for j, st := range it.Settings {
			sat := fmt.Sprintf("%s.settings[%d]", at, j)
			if err := cardText(sat+".label", st.Label, approvalCardMaxTitle, true); err != nil {
				return err
			}
			if err := cardText(sat+".value", st.Value, approvalCardMaxText, false); err != nil {
				return err
			}
		}
		for j, f := range it.Flags {
			fat := fmt.Sprintf("%s.flags[%d]", at, j)
			if !approvalCardFlagCode.MatchString(f.Code) {
				return fmt.Errorf("%s.code is not a short lowercase code", fat)
			}
			if err := cardText(fat+".label", f.Label, approvalCardMaxTitle, true); err != nil {
				return err
			}
		}
	}
	if rows > approvalCardMaxRows {
		return fmt.Errorf("%d rows across the card, over the %d-row limit", rows, approvalCardMaxRows)
	}
	return nil
}

// cardText checks one display string: required when asked, bounded in runes,
// and free of control and bidirectional-formatting characters (a card is
// plain text; such characters could reorder what the reviewer reads).
func cardText(at, s string, maxRunes int, required bool) error {
	if required && strings.TrimSpace(s) == "" {
		return fmt.Errorf("%s is required", at)
	}
	if n := utf8.RuneCountInString(s); n > maxRunes {
		return fmt.Errorf("%s is %d characters, over the %d limit", at, n, maxRunes)
	}
	for _, r := range s {
		if unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r) || r == ' ' || r == ' ' {
			return fmt.Errorf("%s contains a control or bidirectional-formatting character", at)
		}
	}
	return nil
}

// cardLink accepts an absolute https URL with a host and no credentials.
func cardLink(at, s string) error {
	if len(s) > approvalCardMaxLink {
		return fmt.Errorf("%s is longer than %d bytes", at, approvalCardMaxLink)
	}
	if err := cardText(at, s, approvalCardMaxLink, true); err != nil {
		return err
	}
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Opaque != "" {
		return fmt.Errorf("%s must be an absolute https URL without credentials", at)
	}
	return nil
}

// approvalCardFallback reasons, carried on the tool.approval_card_fallback
// event so an observing stream (and a test) can tell why a declared describer
// did not produce a card. The detail stays in the server log.
const (
	cardFallbackUnresolved = "describer_unavailable"
	cardFallbackTimeout    = "timeout"
	cardFallbackError      = "describer_error"
	cardFallbackInvalid    = "invalid_card"
	cardFallbackRedacted   = "redacted"
	cardFallbackStore      = "store_error"
)

// describeApproval runs the bundle-declared describer for a staged call and
// returns the canonical card JSON, or "" with a fallback reason. "" with no
// reason means no describer is declared for the tool (nothing to report).
func (a *approvalStager) describeApproval(toolName, rawInput string) (cardJSON, reason string) {
	suffix := agentcore.CardDescriberFor(toolName)
	if suffix == "" || !strings.HasPrefix(toolName, "mcp_") {
		return "", ""
	}
	broker, catalog, seats := a.mcpScope()
	if broker == nil {
		return "", cardFallbackUnresolved
	}
	server, _, err := resolveMCPTool(catalog, toolName)
	if err != nil {
		log.Printf("approval card: resolve %q: %s", toolName, redactedForLog(err))
		return "", cardFallbackUnresolved
	}
	describer, ok := resolveDescriber(catalog, server, suffix)
	if !ok {
		log.Printf("approval card: no single %q describer on server %q for %q", suffix, server, toolName)
		return "", cardFallbackUnresolved
	}
	// Re-check the describer's identity at call time, under the one policy
	// the loop uses: it runs with no approval, so it must never be a
	// critical tool, and it must be a declared read (parallel-safe) on the
	// registered server or, for a named-account seat, on its base server.
	full := "mcp_" + server + "_" + describer
	readOnly := agentcore.IsParallelSafeTool(full)
	if seat, ok := seats[server]; ok && seat.Server != "" && !readOnly {
		readOnly = agentcore.IsParallelSafeTool("mcp_" + seat.Server + "_" + describer)
	}
	if agentcore.IsCriticalTool(full) || !readOnly {
		log.Printf("approval card: describer %q is critical or not parallel-safe; not called", full)
		return "", cardFallbackUnresolved
	}
	var args map[string]any
	if err := decodeJSONNumbers([]byte(rawInput), &args); err != nil || args == nil {
		return "", cardFallbackInvalid
	}
	ctx, cancel := context.WithTimeout(mcp.WithCallTimeout(a.ctx, approvalCardDescriberTimeout), approvalCardDescriberTimeout)
	defer cancel()
	text, isErr, err := broker.CallMCP(ctx, server, describer, args)
	switch {
	case err != nil && (errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil):
		log.Printf("approval card: describer %q timed out after %s", full, approvalCardDescriberTimeout)
		return "", cardFallbackTimeout
	case err != nil:
		log.Printf("approval card: describer %q: %s", full, redactedForLog(err))
		return "", cardFallbackError
	case isErr:
		log.Printf("approval card: describer %q reported an error", full)
		return "", cardFallbackError
	}
	_, canonical, err := parseApprovalCard([]byte(strings.TrimSpace(text)))
	if err != nil {
		log.Printf("approval card: describer %q output refused: %s", full, redactedForLog(err))
		return "", cardFallbackInvalid
	}
	if tools.RedactionWouldAlter(canonical) {
		log.Printf("approval card: describer %q output carries a secret-like value; card withheld", full)
		return "", cardFallbackRedacted
	}
	return string(canonical), ""
}

// resolveDescriber finds the describer tool on the given registered server:
// the tool named exactly the suffix, else the single tool whose name ends in
// "_<suffix>". Zero or several candidates resolve to nothing — fleet does not
// guess which read to run.
func resolveDescriber(catalog []mcp.ServerTool, server, suffix string) (string, bool) {
	var candidates []string
	for _, st := range catalog {
		if st.ServerName != server {
			continue
		}
		if st.Tool.Name == suffix {
			return suffix, true
		}
		if strings.HasSuffix(st.Tool.Name, "_"+suffix) {
			candidates = append(candidates, st.Tool.Name)
		}
	}
	if len(candidates) == 1 {
		return candidates[0], true
	}
	return "", false
}

// approvalCardPayload is the card a client receives for an approval row, or
// nil. The stored card is re-validated on read, so a row written by another
// version (or edited by hand) can never put an off-schema card on the wire.
func approvalCardPayload(a *store.Approval) json.RawMessage {
	if a == nil || a.CardJSON == "" {
		return nil
	}
	_, canonical, err := parseApprovalCard([]byte(a.CardJSON))
	if err != nil || tools.RedactionWouldAlter(canonical) {
		return nil
	}
	return canonical
}

// withApprovalCard adds the row's card to a client payload when it has one.
func withApprovalCard(payload map[string]any, a *store.Approval) map[string]any {
	if card := approvalCardPayload(a); card != nil {
		payload["card"] = card
	}
	return payload
}
