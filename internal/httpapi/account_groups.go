package httpapi

import (
	"slices"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/clientconfig"
)

// Account groups in the chat Tools picker (docs/ACCOUNT-GROUPS.md): the
// bundle's named sets of connector accounts, resolved against what this box
// and this user can actually use.
//
// The catalog responses carry them (`account_groups`) beside `servers`, so the
// picker gets its rows and its chips from one read and they can never
// describe two different moments.

// Member statuses. A member that is not "ok" is reported, never replaced: a
// group whose client account is missing must not quietly run on the
// deployment's own account.
const (
	accountGroupMemberOK = "ok"
	// The connector is in the picker but this account of it is not
	// provisioned on the box.
	accountGroupMemberAccountMissing = "account_missing"
	// The connector is not in this user's picker: not set up on the box, or
	// turned off by the user under Settings → Connections.
	accountGroupMemberConnectorMissing = "connector_missing"
)

// accountGroupMemberView is one member as the picker reads it.
type accountGroupMemberView struct {
	Server  string `json:"server"`
	Account string `json:"account"`
	Status  string `json:"status"`
}

// accountGroupView is one group as the picker reads it.
type accountGroupView struct {
	ID       string                   `json:"id"`
	Name     string                   `json:"name"`
	Accounts []accountGroupMemberView `json:"accounts"`
}

// accountGroupsView is the `account_groups` value of a catalog response.
type accountGroupsView struct {
	// Label is the bundle's heading for the chips ("Clients"); "" lets the
	// picker use its own generic one.
	Label  string             `json:"label"`
	Groups []accountGroupView `json:"groups"`
}

// resolveAccountGroups checks each member of each group against the picker
// rows the same response carries: pickable maps a connector the user can
// switch to its provisioned account names. A group none of whose members can
// be used is still listed, so the user sees that the group exists and why it
// does nothing here.
func resolveAccountGroups(cfg clientconfig.AccountGroupsConfig, pickable map[string][]string) *accountGroupsView {
	if len(cfg.Groups) == 0 {
		return nil
	}
	view := &accountGroupsView{Label: cfg.Label, Groups: make([]accountGroupView, 0, len(cfg.Groups))}
	for _, g := range cfg.Groups {
		gv := accountGroupView{ID: g.ID, Name: g.Name, Accounts: make([]accountGroupMemberView, 0, len(g.Accounts))}
		for _, m := range g.Accounts {
			status := accountGroupMemberOK
			accounts, ok := pickable[m.Server]
			switch {
			case !ok:
				status = accountGroupMemberConnectorMissing
			case m.Account != "" && !slices.Contains(accounts, m.Account):
				status = accountGroupMemberAccountMissing
			}
			gv.Accounts = append(gv.Accounts, accountGroupMemberView{Server: m.Server, Account: m.Account, Status: status})
		}
		view.Groups = append(view.Groups, gv)
	}
	return view
}

// pickableBundledAccounts lists the bundled optional connectors a catalog
// response offers with a switch, each with its provisioned account names.
// rows are the response's own server rows, so a connector the response hides
// (the user turned it off, the persona cannot use it in chat) is not pickable
// here either.
//
// turnedOff names connectors the user turned off under Settings → Connections.
// A conversation that had one on still lists its row, so it can be switched
// off there, but no turn will run it (applyConnectorPrefs drops it): it is not
// pickable, and a group member on it is reported missing.
func pickableBundledAccounts(rows []map[string]any, catalog []agent.OptionalServerInfo, turnedOff map[string]bool) map[string][]string {
	accounts := make(map[string][]string, len(catalog))
	for _, info := range catalog {
		accounts[info.Name] = info.Accounts
	}
	out := make(map[string][]string, len(rows))
	for _, row := range rows {
		name, _ := row["name"].(string)
		if name == "" || turnedOff[name] {
			continue
		}
		if remote, _ := row["remote"].(bool); remote {
			continue
		}
		if alwaysOn, _ := row["always_on"].(bool); alwaysOn {
			continue
		}
		if status, _ := row["status"].(string); status != "" && status != connectorStatusAvailable {
			continue
		}
		list, known := accounts[name]
		if !known {
			continue
		}
		out[name] = list
	}
	return out
}

// withAccountGroups adds the bundle's account groups to a catalog response
// body. A bundle with none leaves the body as it was, key and all. turnedOff
// is pickableBundledAccounts'; nil when the response lists no such row.
func (s *Server) withAccountGroups(body map[string]any, rows []map[string]any, turnedOff map[string]bool) map[string]any {
	if s.clientConfig == nil || s.agent == nil {
		return body
	}
	if view := resolveAccountGroups(s.clientConfig.AccountGroups(), pickableBundledAccounts(rows, s.agent.MCPServerCatalog(), turnedOff)); view != nil {
		body["account_groups"] = view
	}
	return body
}
