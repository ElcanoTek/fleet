package handlers

import (
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/identity"
	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// The authorization differential for the one identity table (#2049 M4).
//
// Before M4 a person could have a chat row and an Operations Center row, each
// with its own role and enabled flag. After it they have one row, produced by
// the merge's mapping (identity.MapLegacy). This test runs EVERY combination
// of the two old rows through both models and compares the verdicts the
// request path gives:
//
//   - chat membership and chat role (store.GetUser: an enabled row);
//   - Operations Center membership, outside the Prompt Library;
//   - the Operations Center user on a Prompt Library path (the old chat-role
//     fallback);
//   - each Operations Center permission (userHasPermission).
//
// "old" re-implements the pre-M4 decisions from the two rows; "new" is the
// production code (MapLegacy, then headerTrustMember and userHasPermission).
// The verdicts must agree everywhere except the documented changes, each of
// which is asserted explicitly below and listed in docs/IDENTITY.md.

type oldChat struct {
	role    string // admin | member | viewer
	enabled bool
}

type oldSched struct {
	role    string // admin | client | readonly | a junk value (no CHECK)
	enabled bool
}

type verdict struct {
	chatMember bool
	chatRole   string
	opsMember  bool
	prompts    string // the legacy role of the /prompts user, "" for not_a_member
	perms      string // the Operations Center permissions held, in order
}

var diffPermissions = []models.Permission{models.PermissionAdmin, models.PermissionCreateTask, models.PermissionViewTasks, models.PermissionViewLogs}

func permString(u *models.User) string {
	out := ""
	for _, p := range diffPermissions {
		if userHasPermission(u, p) {
			out += string(p) + " "
		}
	}
	return out
}

// oldVerdict is the pre-M4 request path over the two rows.
func oldVerdict(chat *oldChat, sched *oldSched) verdict {
	var v verdict
	if chat != nil && chat.enabled {
		v.chatMember, v.chatRole = true, chat.role
	}
	var ops *models.User
	if sched != nil && sched.enabled {
		ops = &models.User{Role: sched.role}
	}
	v.opsMember = ops != nil
	v.perms = permString(ops)
	// The /prompts fallback: the chat-role seam (enabled chat rows only).
	prompts := ops
	if prompts == nil || prompts.Role == "readonly" {
		var chatUser *models.User
		if chat != nil && chat.enabled {
			chatUser = &models.User{Role: map[string]string{"admin": "admin", "member": "client", "viewer": "readonly"}[chat.role]}
		}
		if prompts == nil || (chatUser != nil && chatUser.Role != "readonly") {
			prompts = chatUser
		}
	}
	if prompts != nil {
		v.prompts = prompts.Role
	}
	return v
}

// newVerdict is the production path over the merged row (nil: no row).
func newVerdict(account *identity.User) verdict {
	var v verdict
	if account != nil && account.Enabled {
		v.chatMember, v.chatRole = true, string(account.Role)
	}
	ops := headerTrustMember(account, false)
	v.opsMember = ops != nil
	v.perms = permString(ops)
	if p := headerTrustMember(account, true); p != nil {
		v.prompts = p.Role
	}
	return v
}

// merged applies the merge's mapping; a person with only a chat row keeps it
// with scope none (the migration's column default).
func merged(chat *oldChat, sched *oldSched) *identity.User {
	switch {
	case chat == nil && sched == nil:
		return nil
	case sched == nil:
		return &identity.User{ID: uuid.New(), Role: identity.Role(chat.role), OpsScope: identity.OpsNone, Enabled: chat.enabled}
	}
	var lc *identity.LegacyChat
	if chat != nil {
		lc = &identity.LegacyChat{Role: identity.Role(chat.role), Enabled: chat.enabled}
	}
	m := identity.MapLegacy(lc, identity.LegacySched{Role: sched.role, Enabled: sched.enabled})
	return &identity.User{ID: uuid.New(), Role: m.Role, OpsScope: m.OpsScope, Enabled: m.Enabled}
}

func TestIdentityAuthorizationDifferential(t *testing.T) {
	chats := make([]*oldChat, 0, 7)
	chats = append(chats, nil)
	for _, role := range []string{"admin", "member", "viewer"} {
		for _, enabled := range []bool{true, false} {
			chats = append(chats, &oldChat{role, enabled})
		}
	}
	scheds := make([]*oldSched, 0, 9)
	scheds = append(scheds, nil)
	for _, role := range []string{"admin", "client", "readonly", "superuser"} {
		for _, enabled := range []bool{true, false} {
			scheds = append(scheds, &oldSched{role, enabled})
		}
	}
	changed := map[string]int{}
	for _, chat := range chats {
		for _, sched := range scheds {
			name := fmt.Sprintf("chat=%+v sched=%+v", chat, sched)
			old, got := oldVerdict(chat, sched), newVerdict(merged(chat, sched))
			if why := documentedChange(chat, sched, old, got); why != "" {
				changed[why]++
				continue
			}
			if old != got {
				t.Errorf("%s: verdict changed without a documented reason\n  old %+v\n  new %+v", name, old, got)
			}
		}
	}
	// Every documented change must actually occur (the list is not stale).
	for _, why := range []string{chatAdmin, splitAdmin, opsOnlyAccount, disabledChat, unknownSchedRole} {
		if changed[why] == 0 {
			t.Errorf("documented change %q never occurred: the list is stale", why)
		}
	}
	t.Logf("documented changes: %v", changed)
}

const (
	chatAdmin        = "a chat admin has full Operations Center access, with or without an Operations Center row (admin is one role)"
	splitAdmin       = "an Operations Center admin without chat admin keeps the chat role with run (ADR-0074 fail closed)"
	opsOnlyAccount   = "an Operations-Center-only account becomes a chat viewer (accepted widening)"
	disabledChat     = "a disabled chat account no longer reaches the Operations Center"
	unknownSchedRole = "an unknown sched role grants nothing, not even membership"
)

// documentedChange returns why old and new may differ for this state, after
// asserting the new verdict is exactly the documented one; "" when the
// verdicts must agree.
func documentedChange(chat *oldChat, sched *oldSched, old, got verdict) string {
	if chat != nil && chat.enabled && chat.role == "admin" {
		// Admin is ONE role: full Operations Center access, whatever (if any)
		// Operations Center row the chat admin had.
		want := old
		want.opsMember, want.perms, want.prompts = true, permString(&models.User{Role: "admin"}), "admin"
		if got != want {
			panic(fmt.Sprintf("chat admin %+v/%+v: got %+v want %+v", chat, sched, got, want))
		}
		if old == want {
			return ""
		}
		return chatAdmin
	}
	if sched == nil {
		return ""
	}
	schedAdmin := sched.enabled && sched.role == "admin"
	switch {
	case chat == nil && sched.enabled && sched.role != "superuser":
		// Operations-Center-only: now a chat viewer; ops as before, except an
		// admin, who fails closed to run.
		want := old
		want.chatMember, want.chatRole = true, "viewer"
		if schedAdmin {
			want.perms = permString(&models.User{Role: "client"})
			want.prompts = "client"
		}
		if got != want {
			panic(fmt.Sprintf("ops-only %+v: got %+v want %+v", sched, got, want))
		}
		return opsOnlyAccount
	case chat != nil && schedAdmin && (chat.role != "admin" || !chat.enabled):
		want := old
		want.perms = permString(&models.User{Role: "client"})
		want.prompts = "client"
		if !chat.enabled {
			want.opsMember, want.perms, want.prompts = false, "", ""
		}
		if got != want {
			panic(fmt.Sprintf("split admin %+v/%+v: got %+v want %+v", chat, sched, got, want))
		}
		return splitAdmin
	case sched.enabled && sched.role == "superuser":
		// Old: a member of the Operations Center with no permission at all;
		// new: not a member. The chat side is unchanged (an ops-only one
		// still becomes a viewer, with no Operations Center access).
		want := old
		want.opsMember, want.perms = false, ""
		switch {
		case chat == nil:
			want.chatMember, want.chatRole, want.prompts = true, "viewer", "readonly"
		case chat.enabled:
			want.prompts = map[string]string{"admin": "admin", "member": "client", "viewer": "readonly"}[chat.role]
		default:
			want.prompts = ""
		}
		if got != want {
			panic(fmt.Sprintf("unknown role %+v/%+v: got %+v want %+v", chat, sched, got, want))
		}
		return unknownSchedRole
	case chat != nil && !chat.enabled && sched.enabled:
		// The one row is disabled: no access to either plane.
		want := verdict{}
		if got != want {
			panic(fmt.Sprintf("disabled chat %+v/%+v: got %+v want %+v", chat, sched, got, want))
		}
		return disabledChat
	}
	return ""
}
