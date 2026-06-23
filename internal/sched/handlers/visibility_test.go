// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package handlers

import (
	"testing"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// TestTaskVisibleToUser covers the post-node-routing visibility contract: tasks
// no longer carry a node target (the per-task mcp_selection replaced routing),
// so a scoped user sees every task plus their own, an unscoped user sees
// everything, and a nil user sees nothing. The out-of-scope-target cases the
// old node-routing model relied on no longer exist.
func TestTaskVisibleToUser(t *testing.T) {
	userID := uuid.New()
	otherID := uuid.New()

	scopedUser := &models.User{ID: userID, Scopes: []string{"elcano-cutlass-*"}}
	unscopedUser := &models.User{ID: otherID}

	tests := []struct {
		name string
		task *models.Task
		user *models.User
		want bool
	}{
		{
			name: "nil user sees nothing",
			task: &models.Task{},
			user: nil,
			want: false,
		},
		{
			name: "unscoped user sees any task",
			task: &models.Task{},
			user: unscopedUser,
			want: true,
		},
		{
			name: "scoped user sees untargeted task",
			task: &models.Task{},
			user: scopedUser,
			want: true,
		},
		{
			name: "scoped user sees own task",
			task: &models.Task{CreatedBy: &userID},
			user: scopedUser,
			want: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := taskVisibleToUser(tc.task, tc.user); got != tc.want {
				t.Errorf("taskVisibleToUser() = %v, want %v", got, tc.want)
			}
		})
	}
}
