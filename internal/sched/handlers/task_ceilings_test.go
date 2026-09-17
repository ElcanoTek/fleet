// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package handlers

import (
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

func fptr(v float64) *float64 { return &v }
func iptr(v int) *int         { return &v }

// Anyone may lower a task's own ceiling below the deployment's; raising it
// above needs admin — the same authority as editing the deployment ceiling.
// An unlimited deployment ceiling (0) bounds nothing (#1533).
func TestRequireAdminForCeilingsAboveDeployment(t *testing.T) {
	h := &Handlers{config: Config{MaxCostUSD: 10, LiveMaxTotalTokens: func() int { return 1_000_000 }}}

	if msg := h.requireAdminForCeilingsAboveDeployment(false, &models.TaskCreate{}); msg != "" {
		t.Fatalf("no per-task ceiling must pass: %q", msg)
	}
	if msg := h.requireAdminForCeilingsAboveDeployment(false, &models.TaskCreate{MaxCostUSD: fptr(4), MaxTotalTokens: iptr(500_000)}); msg != "" {
		t.Fatalf("lowering must pass for a non-admin: %q", msg)
	}
	if msg := h.requireAdminForCeilingsAboveDeployment(false, &models.TaskCreate{MaxCostUSD: fptr(12)}); !strings.Contains(msg, "max_cost_usd") || !strings.Contains(msg, "only an admin") {
		t.Fatalf("raising cost above the deployment ceiling must be refused for a non-admin, got %q", msg)
	}
	if msg := h.requireAdminForCeilingsAboveDeployment(false, &models.TaskCreate{MaxTotalTokens: iptr(2_000_000)}); !strings.Contains(msg, "max_total_tokens") {
		t.Fatalf("raising tokens above the deployment ceiling must be refused for a non-admin, got %q", msg)
	}
	if msg := h.requireAdminForCeilingsAboveDeployment(true, &models.TaskCreate{MaxCostUSD: fptr(12), MaxTotalTokens: iptr(2_000_000)}); msg != "" {
		t.Fatalf("an admin may raise a task above the deployment ceiling: %q", msg)
	}

	// The live reader wins over the boot value, exactly as the estimate does.
	live := &Handlers{config: Config{MaxCostUSD: 10, LiveMaxCostUSD: func() float64 { return 50 }}}
	if msg := live.requireAdminForCeilingsAboveDeployment(false, &models.TaskCreate{MaxCostUSD: fptr(12)}); msg != "" {
		t.Fatalf("12 is below the live ceiling of 50: %q", msg)
	}
	// Unlimited deployment ceiling: every per-task value is a lowering.
	open := &Handlers{config: Config{MaxCostUSD: 0}}
	if msg := open.requireAdminForCeilingsAboveDeployment(false, &models.TaskCreate{MaxCostUSD: fptr(999), MaxTotalTokens: iptr(999_999_999)}); msg != "" {
		t.Fatalf("no deployment ceiling means nothing to exceed: %q", msg)
	}
}

// A per-task ceiling must be a real bound: 0 would read as "unlimited" in
// checkCeilings — the one escape the field must never offer.
func TestValidateTaskLimitsRejectsNonBoundingCeilings(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   models.TaskCreate
		want string
	}{
		{"zero cost", models.TaskCreate{MaxCostUSD: fptr(0)}, "max_cost_usd"},
		{"negative cost", models.TaskCreate{MaxCostUSD: fptr(-1)}, "max_cost_usd"},
		{"absurd cost", models.TaskCreate{MaxCostUSD: fptr(1e9)}, "max_cost_usd"},
		{"tiny tokens", models.TaskCreate{MaxTotalTokens: iptr(10)}, "max_total_tokens"},
		{"zero tokens", models.TaskCreate{MaxTotalTokens: iptr(0)}, "max_total_tokens"},
	} {
		if err := validateTaskLimits(&tc.in); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err=%v, want a %s error", tc.name, err, tc.want)
		}
	}
	ok := models.TaskCreate{MaxCostUSD: fptr(0.5), MaxTotalTokens: iptr(1000)}
	if err := validateTaskLimits(&ok); err != nil {
		t.Fatalf("small but real bounds must pass: %v", err)
	}
	if err := validateTaskLimits(&models.TaskCreate{}); err != nil {
		t.Fatalf("omitted ceilings must pass: %v", err)
	}
}
