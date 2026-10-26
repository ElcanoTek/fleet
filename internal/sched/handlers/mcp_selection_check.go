// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package handlers

import (
	"context"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// SetMCPSelectionCheck wires the create-time mcp_selection check. cmd/fleet
// passes the scheduled runner's CheckTaskMCPSelection, so a create is refused
// for exactly the selections a run of the task would refuse — a server that is
// neither in the active bundle inventory nor among the task owner's hosted
// connections — and with the run's own message. Before this, such a task was
// accepted and every occurrence dead-lettered at dispatch. nil → no check.
func (h *Handlers) SetMCPSelectionCheck(check func(ctx context.Context, task *models.Task) error) {
	h.mcpSelectionCheck = check
}

// checkTaskMCPSelection runs the wired check on a task about to be written.
// task.CreatedBy must already be the owner the run will resolve hosted
// connections for; the check reads MCPSelection and CredentialAllowlist too.
// A non-nil error is a refusal for the client (400), never an infrastructure
// failure: the runner's check swallows those, as a run would.
func (h *Handlers) checkTaskMCPSelection(ctx context.Context, task *models.Task) error {
	if h.mcpSelectionCheck == nil || task == nil || len(task.MCPSelection) == 0 {
		return nil
	}
	return h.mcpSelectionCheck(ctx, task)
}

// checkMCPSelectionForOwner checks a selection that is about to be written onto
// an EXISTING row (an edit, an import replace), whose owner the write keeps.
func (h *Handlers) checkMCPSelectionForOwner(ctx context.Context, owner *uuid.UUID, selection models.MCPSelection, allowlist models.CredentialAllowlist) error {
	return h.checkTaskMCPSelection(ctx, &models.Task{
		CreatedBy:           owner,
		MCPSelection:        selection,
		CredentialAllowlist: allowlist,
	})
}
