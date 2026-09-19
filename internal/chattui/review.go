package chattui

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"

	tea "charm.land/bubbletea/v2"
)

type approvalsLoadedMsg struct {
	conversation string
	pending      []pendingApproval
	err          error
}

type approvalTickMsg struct{}

func (m *model) toolResultIndex(ev Event) int {
	if id := ev.Str("id"); id != "" {
		for i := len(m.toolIDs) - 1; i >= 0; i-- {
			if m.toolIDs[i] == id {
				return i
			}
		}
		return -1
	}
	return len(m.toolLines) - 1
}

func approvalTick() tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return approvalTickMsg{} })
}

func (m *model) expireApprovals(now time.Time) bool {
	kept := m.pending[:0]
	changed := false
	for _, a := range m.pending {
		if !a.executing && a.expiresAt > 0 && now.Unix() >= a.expiresAt {
			m.history = append(m.history, "Local deadline passed for "+a.tool+" · "+a.id+"; the server is authoritative. Refresh with /approvals reload only if the card may have changed.")
			changed = true
		} else {
			kept = append(kept, a)
		}
	}
	m.pending = kept
	if changed {
		m.refresh()
	}
	return changed
}

func (m *model) resetApprovalAutoBlocked() {
	for i := range m.pending {
		m.pending[i].autoBlocked = false
	}
}

// parsePatternArgs copies handler-only raw string fields. Non-strings are
// dropped. A missing payload is nil (old servers) so pattern scope is refused;
// a present map, even empty, means the server exposed pattern_args.
func parsePatternArgs(v any) map[string]string {
	out := map[string]string{}
	switch t := v.(type) {
	case map[string]string:
		if t == nil {
			return nil
		}
		for k, s := range t {
			out[k] = s
		}
	case map[string]any:
		if t == nil {
			return nil
		}
		for k, val := range t {
			s, ok := val.(string)
			if !ok {
				continue
			}
			out[k] = s
		}
	default:
		return nil
	}
	return out
}

func sortedPatternKeys(args map[string]string) []string {
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// effectivePatternArgs overlays local schedule_task edits onto the server's
// handler string fields, including optional fields absent on the original card.
func (a pendingApproval) effectivePatternArgs() map[string]string {
	if a.patternArgs == nil {
		return nil
	}
	out := make(map[string]string, len(a.patternArgs))
	for k, v := range a.patternArgs {
		out[k] = v
	}
	if a.edits == nil {
		return out
	}
	if a.edits.Name != nil {
		out["name"] = *a.edits.Name
	}
	if a.edits.Prompt != nil {
		out["prompt"] = *a.edits.Prompt
	}
	if a.edits.Cron != nil {
		out["cron"] = *a.edits.Cron
	}
	return out
}

func policyMatchesCard(d ApprovalDecision, a pendingApproval) bool {
	if d.Scope != "pattern" {
		return true
	}
	key, glob, ok := strings.Cut(d.Pattern, "=")
	if !ok || key == "" {
		return false
	}
	val, present := a.effectivePatternArgs()[key]
	if !present {
		return false
	}
	matched, err := filepath.Match(glob, val)
	return err == nil && matched
}

// matchCardPolicy returns the terminal session decision that covers this card.
// Deny wins, matching the server registry: scan denials first, then approvals.
func (m *model) matchCardPolicy(a pendingApproval) (ApprovalDecision, bool) {
	policies := m.cardPolicies[m.convID][a.tool]
	for _, want := range []bool{false, true} {
		for _, d := range policies {
			if d.Approved != want || !policyMatchesCard(d, a) {
				continue
			}
			return d, true
		}
	}
	return ApprovalDecision{}, false
}

// LoadApprovals rehydrates server-owned cards, including expiry, pattern_args,
// and the full review summary. No decisions are inferred from a previous
// terminal session.
func (c *Client) loadApprovals(ctx context.Context, conversation string) ([]pendingApproval, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.ServerURL+"/conversations/"+url.PathEscape(conversation)+"?omit_history=1", nil)
	if err != nil {
		return nil, err
	}
	c.setAuthHeaders(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("load conversation: HTTP %d", resp.StatusCode)
	}
	type approvalWire struct {
		ID          string         `json:"approval_id"`
		Tool        string         `json:"tool"`
		Summary     map[string]any `json:"summary"`
		PatternArgs map[string]any `json:"pattern_args"`
		ExpiresAt   int64          `json:"expires_at"`
		Executing   bool           `json:"executing"`
	}
	var body struct {
		Approvals []approvalWire `json:"pending_approvals"`
		Resolved  []approvalWire `json:"resolved_approvals"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&body); err != nil {
		return nil, err
	}
	var pending []pendingApproval
	for _, a := range body.Resolved {
		if a.Executing {
			body.Approvals = append(body.Approvals, a)
		}
	}
	for _, a := range body.Approvals {
		pending = append(pending, pendingApproval{
			id:          a.ID,
			tool:        a.Tool,
			summary:     approvalSummaryLine(a.Tool, a.Summary),
			details:     a.Summary,
			patternArgs: parsePatternArgs(a.PatternArgs),
			expiresAt:   a.ExpiresAt,
			executing:   a.Executing,
		})
	}
	return pending, nil
}

func (m *model) loadApprovals(conversation string) tea.Cmd {
	client := m.client
	m.reviewBusy = true
	return func() tea.Msg {
		pending, err := client.loadApprovals(context.Background(), conversation)
		return approvalsLoadedMsg{conversation: conversation, pending: pending, err: err}
	}
}

func (m *model) reviewNote(text string) tea.Cmd {
	m.history = append(m.history, text)
	m.refresh()
	m.vp.GotoBottom()
	return nil
}

func (m *model) showApprovals() tea.Cmd {
	if len(m.pending) == 0 {
		return m.reviewNote("No pending approvals. /approvals reload refreshes the current conversation.")
	}
	var b strings.Builder
	for _, a := range m.pending {
		fmt.Fprintf(&b, "Approval %s · %s\n%s\n", a.id, a.tool, a.summary)
		if a.executing {
			b.WriteString("Already approved; execution is running. /approve " + a.id + " retrieves the outcome without re-executing.\n")
		}
		if a.expiresAt > 0 {
			fmt.Fprintf(&b, "Expires: %s\n", time.Unix(a.expiresAt, 0).UTC().Format(time.RFC3339))
		}
		if !handlerOnlyCard(a.tool) {
			b.WriteString("Patterns match original string tool arguments on the server, not summary labels.\n")
		} else if a.patternArgs == nil {
			b.WriteString("No pattern keys; pattern scope cannot match this card (server did not send pattern_args).\n")
		} else if keys := sortedPatternKeys(a.patternArgs); len(keys) > 0 {
			fmt.Fprintf(&b, "Pattern keys: %s\n", strings.Join(keys, ", "))
		} else {
			b.WriteString("No pattern keys on this card.\n")
		}
		data, _ := json.MarshalIndent(a.details, "", "  ")
		b.Write(data)
		if a.edits != nil {
			data, _ = json.MarshalIndent(a.edits, "", "  ")
			fmt.Fprintf(&b, "\nStaged edits: %s", data)
		}
		b.WriteString("\n/approve [id] [session|pattern arg=glob] · /deny [id]\n")
	}
	return m.reviewNote(b.String())
}

// scheduleCardKind reports whether the frozen schedule_task card is recurring
// (cron), one-time (run_at), or immediate. Cron edits are only valid on
// recurring cards: overlaying cron onto run_at fails validation after the
// approval is claimed, and clearing cron on a recurring card silently turns
// it into an immediate run.
func scheduleCardKind(a pendingApproval) string {
	if a.details != nil {
		if rec, ok := a.details["recurring"].(bool); ok && rec {
			return "recurring"
		}
		if strField(a.details, "cron") != "" {
			return "recurring"
		}
		if strField(a.details, "run_at") != "" {
			return "one-time"
		}
		if imm, ok := a.details["run_immediately"].(bool); ok && imm {
			return "immediate"
		}
	}
	if a.patternArgs != nil {
		if strings.TrimSpace(a.patternArgs["cron"]) != "" {
			return "recurring"
		}
		if strings.TrimSpace(a.patternArgs["run_at"]) != "" {
			return "one-time"
		}
	}
	return ""
}

func (m *model) editApproval(text string) tea.Cmd {
	_, payload, ok := strings.Cut(text, " ")
	if !ok || len(m.pending) == 0 {
		return m.reviewNote(`usage: /edit {"name":"...","prompt":"...","cron":"..."} (oldest schedule_task card)`)
	}
	if m.pending[0].tool != "schedule_task" {
		return m.reviewNote("The oldest card is not schedule_task; approve or deny it first.")
	}
	if m.pending[0].executing {
		return m.reviewNote("This action is already executing; its arguments cannot be edited.")
	}
	var edits ScheduleEdits
	d := json.NewDecoder(strings.NewReader(payload))
	d.DisallowUnknownFields()
	if err := d.Decode(&edits); err != nil {
		return m.reviewNote("Invalid edits: " + err.Error())
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return m.reviewNote("Edits must be one JSON object.")
	}
	if edits.Name == nil && edits.Prompt == nil && edits.Cron == nil {
		return m.reviewNote("Supply name, prompt, or cron.")
	}
	if edits.Prompt != nil && strings.TrimSpace(*edits.Prompt) == "" {
		return m.reviewNote("Prompt cannot be empty.")
	}
	if edits.Cron != nil {
		kind := scheduleCardKind(m.pending[0])
		if kind == "one-time" || kind == "immediate" {
			return m.reviewNote("Cron edits apply only to recurring schedule_task cards.")
		}
		if kind == "recurring" && strings.TrimSpace(*edits.Cron) == "" {
			return m.reviewNote("Clearing cron would convert this recurring task into an immediate run.")
		}
	}
	if m.pending[0].edits == nil {
		m.pending[0].edits = &ScheduleEdits{}
	}
	e := m.pending[0].edits
	if edits.Name != nil {
		e.Name = edits.Name
	}
	if edits.Prompt != nil {
		e.Prompt = edits.Prompt
	}
	if edits.Cron != nil {
		e.Cron = edits.Cron
	}
	return m.showApprovals()
}

func (m *model) decideApproval(fields []string, approve bool) tea.Cmd {
	if len(m.pending) == 0 {
		return m.reviewNote("— no pending approvals —")
	}
	idx := 0
	args := fields[1:]
	if len(args) > 0 && args[0] != "session" && args[0] != "pattern" {
		id := args[0]
		args = args[1:]
		idx = -1
		for i, a := range m.pending {
			if a.id == id {
				idx = i
				break
			}
		}
		if idx < 0 {
			return m.reviewNote("Unknown approval id; use /approvals to review pending cards.")
		}
	}
	d := ApprovalDecision{Approved: approve, Edits: m.pending[idx].edits}
	if len(args) > 0 {
		switch {
		case len(args) == 1 && args[0] == "session":
			d.Scope = "session"
		case len(args) >= 2 && args[0] == "pattern":
			d.Scope = "pattern"
			// The slash parser preserves the raw remainder after pattern, so
			// literal whitespace inside a glob has the same meaning server-side.
			d.Pattern = strings.Join(args[1:], " ")
			key, glob, ok := strings.Cut(d.Pattern, "=")
			if !ok || key == "" {
				return m.reviewNote("Invalid arg=glob pattern.")
			}
			if _, err := filepath.Match(glob, ""); err != nil {
				return m.reviewNote("Invalid arg=glob pattern.")
			}
			if handlerOnlyCard(m.pending[idx].tool) && m.pending[idx].patternArgs == nil {
				return m.reviewNote(patternKeyUnavailable(key))
			}
		default:
			return m.reviewNote("usage: /approve|/deny [id] [session|pattern arg=glob]")
		}
	}
	a := m.pending[idx]
	if approve && !a.executing && emailSummaryOverflow(a.details) {
		return m.reviewNote("Refusing to approve: the frozen email body exceeded the 1 MiB review cap. Deny, or wait for a smaller restage.")
	}
	if a.executing {
		if !approve {
			return m.reviewNote("This action is already executing; /approve " + a.id + " retrieves its outcome.")
		}
		d.Scope, d.Pattern, d.Edits = "", "", nil
	}
	if !a.executing && a.expiresAt > 0 && time.Now().Unix() >= a.expiresAt {
		return m.reviewNote("Local deadline passed; the server is authoritative. Refresh with /approvals reload only if this card may have changed. Nothing executed.")
	}
	m.pending = append(m.pending[:idx], m.pending[idx+1:]...)
	m.reviewBusy = true
	client, conv := m.client, m.convID
	m.reviewNote("Resolving " + a.tool + " (" + orDefault(d.Scope, "once") + ")…")
	return func() tea.Msg {
		wire := d
		// These tools execute only inside the approval handler. A server-side
		// preapproval sentinel cannot call their deliberately inert Run methods.
		if handlerOnlyCard(a.tool) {
			wire.Scope = ""
			wire.Pattern = ""
		}
		status, result, err := client.ResolveApprovalWithOptions(context.Background(), conv, a.id, wire)
		return approvalResolvedMsg{tool: a.tool, approved: approve, status: status, resultText: result, err: err, card: a, decision: d, conversation: conv}
	}
}

func approvalCommandFields(text string) []string {
	var fields []string
	for text = strings.TrimSpace(text); text != ""; text = strings.TrimLeftFunc(text, unicode.IsSpace) {
		end := strings.IndexFunc(text, unicode.IsSpace)
		if end < 0 {
			return append(fields, text)
		}
		word := text[:end]
		fields = append(fields, word)
		text = strings.TrimLeftFunc(text[end:], unicode.IsSpace)
		if word == "pattern" {
			if text != "" {
				fields = append(fields, text)
			}
			return fields
		}
	}
	return fields
}

func patternKeyUnavailable(key string) string {
	return "Pattern key " + key + " is unavailable; this card has no handler pattern_args. Older servers cannot match patterns. Available keys: none."
}

func handlerOnlyCard(tool string) bool {
	return tool == "schedule_task" || tool == "manage_tasks" || tool == "preview_email" || tool == "suggest_advanced_model"
}

// Handler-only actions continue to create real server approval rows. A terminal
// session decision resolves matching rows through that same idempotent endpoint.
func (m *model) autoResolveCard() tea.Cmd {
	if m.streaming || m.reviewBusy {
		return nil
	}
	now := time.Now().Unix()
	for _, a := range m.pending {
		if a.autoBlocked || a.executing {
			continue
		}
		if a.expiresAt > 0 && now >= a.expiresAt {
			continue
		}
		d, ok := m.matchCardPolicy(a)
		if !ok {
			continue
		}
		m.reviewNote("Applying terminal session decision to " + a.tool + " · " + a.id)
		return m.decideApproval([]string{"/approve", a.id}, d.Approved)
	}
	return nil
}
