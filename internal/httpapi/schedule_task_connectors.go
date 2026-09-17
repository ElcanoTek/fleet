// Connector snapshot for chat-created scheduled tasks (ADR-0068).
//
// A scheduled run binds exactly the task's saved mcp_selection plus the
// bundle's always-on servers (ADR-0052). The chat-side scheduling contract
// (tools.ScheduleTaskParams) never carried connectors, so a task scheduled from
// chat — by the agent's schedule_task call or by promote-to-task — was saved
// with none and, in a bundle whose connectors are all optional, ran with no
// email, no mailbox and no data feeds. This file makes the conversation's own
// connector selection travel with the staged card: resolved when the card is
// staged, shown on the card, and written to the task on approval.

package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/ElcanoTek/fleet/internal/tools"
)

// TaskConnector is one connector a chat-created scheduled task keeps: the
// public {server, account} pair the conversation had enabled. Account "" is
// the default seat. It mirrors the sched MCPChoice at the JSON level without
// importing the sched model into the chat server.
type TaskConnector struct {
	Server  string `json:"server"`
	Account string `json:"account,omitempty"`
}

// Label renders a connector for cards and confirmations: "gamma" or
// "gamma (work)".
func (c TaskConnector) Label() string {
	if c.Account == "" {
		return c.Server
	}
	return c.Server + " (" + c.Account + ")"
}

// scheduleTaskStagedArgs is what the approvals row stores for a schedule_task
// card: the agent-facing params plus the connector snapshot the stager attached.
// The two connector fields are deliberately NOT on tools.ScheduleTaskParams, so
// they never appear in the tool schema the model sees — the conversation's own
// toggles, not the model, decide which connectors a scheduled task inherits.
// Older rows (staged before this snapshot existed) simply lack the keys.
type scheduleTaskStagedArgs struct {
	tools.ScheduleTaskParams
	// Connectors is the resolved selection; an explicit empty list means the
	// stager looked and the conversation had none enabled.
	Connectors []TaskConnector `json:"connectors,omitempty"`
	// AlwaysOnConnectors names the bundle's available non-optional servers at
	// staging time — display only, so the card can say whether "no connectors"
	// means "always-on only" or "no tools at all".
	AlwaysOnConnectors []string `json:"always_on_connectors,omitempty"`
	// ConnectorsResolved distinguishes a stager that attached a (possibly
	// empty) snapshot from a legacy row that never had one.
	ConnectorsResolved bool `json:"connectors_resolved,omitempty"`
}

// chatTaskConnectors is the conversation's live connector picture at staging
// time.
type chatTaskConnectors struct {
	Selection []TaskConnector
	AlwaysOn  []string
}

// chatTaskConnectorsFor returns the stager's connector resolver for one
// conversation. It re-reads the conversation at staging time (the card should
// reflect the toggles as they are when the agent asks, not when the turn
// began) and applies the same connections-page preferences the turn itself
// applies, so the task inherits exactly the connectors chat would have run
// with: the opted-in optional bundle servers with their seats, and the user's
// hosted connections.
func (s *Server) chatTaskConnectorsFor(user, convID string) func(context.Context) (chatTaskConnectors, error) {
	return func(ctx context.Context) (chatTaskConnectors, error) {
		conv, err := s.store.Get(ctx, user, convID)
		if err != nil {
			return chatTaskConnectors{}, err
		}
		out := chatTaskConnectors{AlwaysOn: s.alwaysOnMCPServerNames()}
		if conv == nil {
			return out, nil
		}
		enabled, accounts := s.applyConnectorPrefs(ctx, user, conv.OptionalMCPServersEnabled, conv.MCPAccounts)
		for _, name := range enabled {
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			out.Selection = append(out.Selection, TaskConnector{Server: name, Account: accounts[name]})
		}
		sort.Slice(out.Selection, func(i, j int) bool { return out.Selection[i].Server < out.Selection[j].Server })
		return out, nil
	}
}

// alwaysOnMCPServerNames lists the bundle's non-optional servers that live
// discovery found usable — the set every scheduled run binds regardless of its
// saved selection (ADR-0052). Empty when the engine exposes no such catalog or
// every connector in the bundle is optional.
func (s *Server) alwaysOnMCPServerNames() []string {
	provider, ok := s.agent.(alwaysOnMCPServerCatalogProvider)
	if !ok {
		return nil
	}
	var names []string
	for _, info := range provider.AlwaysOnMCPServerCatalog() {
		if info.Available && strings.TrimSpace(info.Name) != "" {
			names = append(names, info.Name)
		}
	}
	sort.Strings(names)
	return names
}

// attachTaskConnectors rewrites a staged schedule_task payload to carry the
// conversation's connector snapshot. Anything the model itself put under the
// snapshot keys is overwritten: the conversation's toggles are the authority.
// A stager without a resolver (tests, legacy construction) stages the payload
// untouched; a payload that is not JSON is also left alone so Validate keeps
// reporting the real parse error on approval.
func (a *approvalStager) attachTaskConnectors(rawInput string) (string, error) {
	if a.taskConnectors == nil {
		return rawInput, nil
	}
	var staged scheduleTaskStagedArgs
	if err := json.Unmarshal([]byte(rawInput), &staged); err != nil {
		return rawInput, nil //nolint:nilerr // unparseable args stage untouched so Validate reports the real parse error on approval
	}
	conns, err := a.taskConnectors(a.ctx)
	if err != nil {
		// Fail closed: staging a card that silently drops every connector is
		// the defect this snapshot exists to prevent. The agent sees the reason
		// and can retry.
		return "", fmt.Errorf("schedule_task: could not resolve this conversation's connectors: %w", err)
	}
	staged.Connectors = conns.Selection
	staged.AlwaysOnConnectors = conns.AlwaysOn
	staged.ConnectorsResolved = true
	out, err := json.Marshal(staged)
	if err != nil {
		return "", fmt.Errorf("schedule_task: encode staged args: %w", err)
	}
	return string(out), nil
}

// connectorLabels renders a selection for the card / confirmation.
func connectorLabels(connectors []TaskConnector) []string {
	labels := make([]string, 0, len(connectors))
	for _, c := range connectors {
		if strings.TrimSpace(c.Server) == "" {
			continue
		}
		labels = append(labels, c.Label())
	}
	return labels
}

// describeTaskConnectors is the one-line "Connectors:" statement the chat
// confirmation carries after a task is created, so the user learns what the
// task can reach — and where to change it — without opening the run log.
func describeTaskConnectors(connectors []TaskConnector, alwaysOn []string) string {
	labels := connectorLabels(connectors)
	switch {
	case len(labels) > 0:
		return "Connectors: " + strings.Join(labels, ", ")
	case len(alwaysOn) > 0:
		return "Connectors: none selected; the task runs with the always-on connectors (" + strings.Join(alwaysOn, ", ") + ")"
	default:
		return "Connectors: none — this task will run without any email, mailbox or data connectors. Add them to the task in the Operations Center if it needs them."
	}
}
