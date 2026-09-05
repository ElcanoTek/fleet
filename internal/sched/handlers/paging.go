// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package handlers

import (
	"fmt"
	"net/http"
	"strconv"
)

// Page-size bounds for the offset/limit list endpoints that share parseLimit.
// The upper bounds exist so one request cannot ask the database for an
// unbounded result set; the defaults match what each endpoint returned before
// paging was validated, so an unparameterized call is unchanged. (The upcoming
// feed's bounds live beside its projection constants in upcoming.go.)
const (
	taskListDefaultLimit    = 50
	taskListLimitMax        = 500
	pausedDefaultLimit      = 100
	pausedLimitMax          = 1000
	datasetRowsDefaultLimit = 200
	datasetRowsLimitMax     = 1000
)

// parseLimit reads the optional ?limit= query parameter of a list endpoint.
//
// An absent parameter yields defaultLimit. Anything present must be an integer
// in [1, maxLimit]; otherwise a 400 with the same message GET /tasks has always
// used is written and ok is false, so the caller just returns. This is the one
// place the scheduler API decides what a bad page size means: before it, the
// sibling list endpoints each parsed the parameter by hand, and three of them
// (`/tasks/upcoming`, `/tasks/paused`, `/datasets/{id}/rows`) discarded the
// strconv error — so `?limit=abc` silently fell back to the default and the
// rows endpoint had no upper bound at all.
func parseLimit(w http.ResponseWriter, r *http.Request, defaultLimit, maxLimit int) (int, bool) {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return defaultLimit, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > maxLimit {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("Invalid limit parameter (must be 1-%d)", maxLimit))
		return 0, false
	}
	return n, true
}

// parseOffset reads the optional ?offset= query parameter of an
// offset-paged list endpoint: absent → 0, otherwise a non-negative integer.
// A negative offset is rejected here with a 400 rather than handed to
// Postgres, which refuses it ("OFFSET must not be negative") and used to
// surface as an opaque 500 from the dataset rows endpoint.
func parseOffset(w http.ResponseWriter, r *http.Request) (int, bool) {
	raw := r.URL.Query().Get("offset")
	if raw == "" {
		return 0, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		writeError(w, http.StatusBadRequest, "Invalid offset parameter (must be a non-negative integer)")
		return 0, false
	}
	return n, true
}
