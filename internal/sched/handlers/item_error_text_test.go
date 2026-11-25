// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package handlers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	wire "github.com/a2aproject/a2a-go/v2/a2a"
)

// A batch or import record's error is a create refusal's own text; a store
// failure is logged and reported as the fixed "internal error".
func TestItemErrorText(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	refusal := &createRefusalError{status: http.StatusBadRequest, detail: "prompt is required"}
	if got := itemErrorText(ctx, fmt.Errorf("task[0]: %w", refusal)); got != "prompt is required" {
		t.Errorf("refusal: %q", got)
	}
	if got := itemErrorText(ctx, taskNameTaken("nightly")); got != taskNameTaken("nightly").detail {
		t.Errorf("name taken: %q", got)
	}
	store := errors.New(`ERROR: relation "sched.tasks" does not exist (SQLSTATE 42P01)`)
	if got := itemErrorText(ctx, store); got != "internal error" {
		t.Errorf("store failure: %q", got)
	}
}

// An A2A error wrapping a wire sentinel keeps its text; any other error is
// answered with the fixed "internal error" (and logged).
func TestA2AErrorFromAnswersAnInternalErrorFixed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	notFound := a2aErrorFrom(ctx, []byte("1"), fmt.Errorf("%w: no task %q is visible to this credential", wire.ErrTaskNotFound, "t1"))
	if notFound.Error == nil || notFound.Error.Message != fmt.Sprintf("%s: no task %q is visible to this credential", wire.ErrTaskNotFound, "t1") {
		t.Fatalf("sentinel: %+v", notFound.Error)
	}
	internal := a2aErrorFrom(ctx, []byte("1"), errors.New(`ERROR: relation "sched.tasks" does not exist (SQLSTATE 42P01)`))
	if internal.Error == nil || internal.Error.Message != "internal error" {
		t.Fatalf("store failure: %+v", internal.Error)
	}
}
