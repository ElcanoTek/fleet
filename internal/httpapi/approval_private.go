package httpapi

import (
	"context"
	"errors"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/logging"
	"github.com/ElcanoTek/fleet/internal/mcp"
	"github.com/ElcanoTek/fleet/internal/safe"
	"github.com/ElcanoTek/fleet/internal/store"
	"github.com/ElcanoTek/fleet/internal/tools"
)

// An outbound email card's Private box: every email the bundle's sender sends
// is copied to the team Outbox, which the whole organization reads, and the
// person may keep one out of it before approving. The card offers the box
// (`private_option`) on outbound email tools only, ticked when the model
// passed private; the approve request carries the person's choice
// (approvalRequest.Private), which is written in the claim itself
// (store.ClaimApprovalPrivate) and sent with the call as the request `_meta`
// fleet/private_emails, so it wins over the model's argument. Absent (the
// CLI, an older client): the call runs with its own argument, as before.

// privateOptionTool reports whether toolName's card offers the Private box:
// a send_email card, the one email card that renders the box. A
// send_template_email card renders as a generic action card without it, so it
// never sends a choice (the model's own argument applies).
func privateOptionTool(toolName string) bool {
	return agentcore.IsOutboundEmailTool(toolName) && tools.IsEmailToolName(toolName) && toolName != "preview_email"
}

// checkRequestedPrivate refuses a Private choice the card could not have
// offered: only with an approval (a declined email sends nothing), only on an
// outbound email card, and never with a narrowing (an email card is never
// narrowable). nil when the request carries none.
func checkRequestedPrivate(a *store.Approval, req approvalRequest, n *approvalNarrowing) error {
	if req.Private == nil {
		return nil
	}
	if !req.Approved {
		return errors.New("private goes with an approval; to decline, send approved false without it")
	}
	if !privateOptionTool(a.ToolName) {
		return errors.New("private applies only to an outbound email card that offers the Private box")
	}
	if n != nil {
		return errors.New("private cannot be combined with a narrowed approval")
	}
	return nil
}

// claimApprovedPrivate claims an email card approved with the person's
// Private choice, writing it in the same UPDATE. On a won claim it sets
// a.PrivateChoice (execution reads it) and logs the audit line.
func (s *Server) claimApprovedPrivate(ctx context.Context, user, convID string, a *store.Approval, private bool) (bool, error) {
	claimed, err := s.store.ClaimApprovalPrivate(ctx, user, a.ID, approvalExecutingSentinel, private)
	if err == nil && claimed {
		a.PrivateChoice = &private
		// The choice is logged as one of two fixed words, never the request's
		// own value.
		choice := "outbox"
		if private {
			choice = "private"
		}
		logging.From(ctx).Info("audit: approval private choice", "user", safe.LogSafe(user), "choice", choice, "approval_id", safe.LogSafe(a.ID), "tool", safe.LogSafe(a.ToolName), "conv_id", safe.LogSafe(convID))
	}
	return claimed, err
}

// approvalPrivateRequestMeta is the `_meta` entry an email card approved with
// a Private choice sends with its call, or nil when no choice was recorded.
func approvalPrivateRequestMeta(a *store.Approval) map[string]any {
	if a == nil || a.PrivateChoice == nil || !privateOptionTool(a.ToolName) {
		return nil
	}
	return map[string]any{mcp.PrivateEmailsMetaKey: *a.PrivateChoice}
}

// withApprovalPrivateChoice adds a settled email card's `private_choice` (the
// box as the person approved it). Absent for every other row, so its payload
// is unchanged.
func withApprovalPrivateChoice(payload map[string]any, a *store.Approval) map[string]any {
	if a == nil || a.PrivateChoice == nil {
		return payload
	}
	payload["private_choice"] = *a.PrivateChoice
	return payload
}
