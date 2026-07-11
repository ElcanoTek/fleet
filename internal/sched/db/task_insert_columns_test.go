// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package db

import (
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// TestTaskInsertColumnsCount pins the three task-insert shapes to each other,
// DB-free, so a new task column can never desynchronize them again: #710 added
// file_names to taskInsertColumns + taskInsertArgs but not
// taskInsertColumnsCount, which broke every multi-row AddTaskBatch INSERT
// ("INSERT has more target columns than expressions") — and only the DB-gated
// suites could see it (#723). This test fails at plain `go test` speed instead.
func TestTaskInsertColumnsCount(t *testing.T) {
	var cols int
	for _, c := range strings.Split(taskInsertColumns, ",") {
		if strings.TrimSpace(c) != "" {
			cols++
		}
	}
	if cols != taskInsertColumnsCount {
		t.Errorf("taskInsertColumns has %d columns but taskInsertColumnsCount = %d — bump the const with the column list", cols, taskInsertColumnsCount)
	}
	if args := len(taskInsertArgs(&models.Task{})); args != taskInsertColumnsCount {
		t.Errorf("taskInsertArgs returns %d values but taskInsertColumnsCount = %d — keep the arg list in the exact column order", args, taskInsertColumnsCount)
	}
}
