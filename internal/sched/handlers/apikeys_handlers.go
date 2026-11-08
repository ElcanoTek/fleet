package handlers

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/ElcanoTek/fleet/internal/logging"
	"github.com/ElcanoTek/fleet/internal/safe"
	"github.com/ElcanoTek/fleet/internal/sched/apikeys"
	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/go-chi/chi/v5"
)

// API Key Management Endpoints

// CreateAPIKey handles POST /keys
func (h *Handlers) CreateAPIKey(w http.ResponseWriter, r *http.Request) {
	var keyCreate models.APIKeyCreate
	if err := readJSON(r, &keyCreate); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	// The per-key spending caps are retired: nothing ever fed their accumulator,
	// so a cap set here was silently unenforced. Refuse the field rather than
	// accept it — an operator who believes a key is capped at $50/day and is
	// wrong is worse off than one who gets an error naming the replacement.
	// Checked BEFORE creating the key so a rejected request mints nothing.
	if keyCreate.MaxCostPerDayUSD != nil || keyCreate.MaxCostPerMonthUSD != nil {
		writeError(w, http.StatusBadRequest,
			"max_cost_per_day_usd/max_cost_per_month_usd are no longer supported; create a rolling budget instead: POST /admin/budgets {\"scope\":\"key\",\"principal_id\":\"<key_id>\",\"window\":\"day|week|month\",\"hard_usd\":…}")
		return
	}

	// Validate the optional task-urgency ceiling (#230) BEFORE creating the key,
	// so a bad value never leaves a half-created (uncapped) key behind.
	if keyCreate.MaxPriority != nil && (*keyCreate.MaxPriority < models.PriorityMin || *keyCreate.MaxPriority > models.PriorityMax) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("max_priority must be between %d and %d", models.PriorityMin, models.PriorityMax))
		return
	}

	// An explicit permission set (#980) is the legacy path's escape hatch for a
	// grant no role expresses. Reject the ambiguous combinations up front rather
	// than picking a winner silently — CreateKey prefers role over permissions,
	// and a typed key ignores both, so a caller that sent two of them would get a
	// key with permissions it did not ask for.
	if len(keyCreate.Permissions) > 0 {
		switch {
		case keyCreate.Type != "":
			writeError(w, http.StatusBadRequest, "permissions cannot be combined with type; a typed key derives its permissions from its type")
			return
		case keyCreate.Role != nil:
			writeError(w, http.StatusBadRequest, "permissions cannot be combined with role; set one or the other")
			return
		}
		for _, perm := range keyCreate.Permissions {
			if !perm.Valid() {
				writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown permission %q", perm))
				return
			}
		}
	}

	var (
		key    *apikeys.APIKey
		rawKey string
		err    error
	)
	if keyCreate.Type != "" {
		// Typed-key path (#190): the type determines the permission set; reject an
		// unknown type, and require a webhook key to be scoped to ≥1 valid slug.
		kt := apikeys.KeyType(keyCreate.Type)
		if !kt.Valid() {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown key type %q (want admin|task|webhook|readonly)", keyCreate.Type))
			return
		}
		if kt == apikeys.KeyTypeWebhook {
			if len(keyCreate.AllowedTriggerSlugs) == 0 {
				writeError(w, http.StatusBadRequest, "webhook key requires at least one allowed_trigger_slugs entry")
				return
			}
			for _, s := range keyCreate.AllowedTriggerSlugs {
				if !triggerSlugShape.MatchString(s) {
					writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid trigger slug %q", s))
					return
				}
			}
		}
		key, rawKey, err = h.apiKeys.CreateTypedKey(
			keyCreate.Name,
			kt,
			keyCreate.AllowedTriggerSlugs,
			keyCreate.RateLimit,
			keyCreate.ExpiresInDays,
			keyCreate.Description,
		)
	} else {
		// Legacy role-based path: mints an untyped sk- key. Unchanged except that
		// an explicit (validated, role-free) permission set is now honored.
		key, rawKey, err = h.apiKeys.CreateKey(
			keyCreate.Name,
			keyCreate.Permissions,
			keyCreate.Role,
			keyCreate.RateLimit,
			keyCreate.ExpiresInDays,
			keyCreate.Description,
		)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to create API key")
		return
	}

	// Apply the optional task-urgency ceiling (#230), validated above.
	if keyCreate.MaxPriority != nil {
		if err := h.apiKeys.SetMaxPriority(key.KeyID, keyCreate.MaxPriority); err != nil {
			logging.From(r.Context()).Warn("Warning: failed to set max_priority on new key", "key_id", key.KeyID, "err", err)
		}
	}

	logging.From(r.Context()).Info("Created API key", "key_id", key.KeyID, "key_name", safe.LogSafe(key.Name))

	resp := key.ToResponse()
	writeJSON(w, r, http.StatusOK, models.APIKeyCreated{
		APIKeyResponse: resp,
		APIKey:         rawKey,
	})
}

// ListAPIKeys handles GET /keys
func (h *Handlers) ListAPIKeys(w http.ResponseWriter, r *http.Request) {
	keys := h.apiKeys.ListKeys()
	responses := make([]models.APIKeyResponse, len(keys))
	for i, key := range keys {
		responses[i] = key.ToResponse()
	}

	writeJSON(w, r, http.StatusOK, responses)
}

// GetAuditLog handles GET /keys/audit
func (h *Handlers) GetAuditLog(w http.ResponseWriter, r *http.Request) {
	keyID := r.URL.Query().Get("key_id")
	action := r.URL.Query().Get("action")
	hours := 24
	if hr := r.URL.Query().Get("hours"); hr != "" {
		parsed, err := strconv.Atoi(hr)
		if err != nil || parsed < 1 || parsed > 24*365 {
			writeError(w, http.StatusBadRequest, "Invalid hours parameter (must be 1-8760)")
			return
		}
		hours = parsed
	}
	limit := 100
	if l := r.URL.Query().Get("limit"); l != "" {
		parsed, err := strconv.Atoi(l)
		if err != nil || parsed < 1 || parsed > 1000 {
			writeError(w, http.StatusBadRequest, "Invalid limit parameter (must be 1-1000)")
			return
		}
		limit = parsed
	}

	var since *time.Time
	if hours > 0 {
		t := time.Now().UTC().Add(-time.Duration(hours) * time.Hour)
		since = &t
	}

	var keyIDPtr, actionPtr *string
	if keyID != "" {
		keyIDPtr = &keyID
	}
	if action != "" {
		actionPtr = &action
	}

	entries := h.apiKeys.GetAuditLog(keyIDPtr, actionPtr, since, limit)
	writeJSON(w, r, http.StatusOK, models.AuditLogResponse{
		Entries: entries,
		Total:   len(entries),
	})
}

// GetAPIKey handles GET /keys/{key_id}
func (h *Handlers) GetAPIKey(w http.ResponseWriter, r *http.Request) {
	keyID := chi.URLParam(r, "key_id")
	key := h.apiKeys.GetKey(keyID)
	if key == nil {
		writeError(w, http.StatusNotFound, "API key not found")
		return
	}

	writeJSON(w, r, http.StatusOK, key.ToResponse())
}

// maxRotateGraceHours bounds POST /keys/{id}/rotate's ?grace_period_hours=:
// one week is ample for any client cut-over, and past that a "rotation" that
// leaves the old key valid is indistinguishable from not rotating.
const maxRotateGraceHours = 168

// RotateAPIKey handles POST /keys/{key_id}/rotate
func (h *Handlers) RotateAPIKey(w http.ResponseWriter, r *http.Request) {
	keyID := chi.URLParam(r, "key_id")
	// The grace window keeps the OLD key valid after rotation, so an unbounded
	// (or silently ignored) value defeats the rotation: ?grace_period_hours=87600
	// used to keep a retired key alive for ten years, and a typo fell back to
	// the default without a word. Bounded like GetAuditLog's ?hours=: 0 = revoke
	// immediately, at most one week of overlap.
	gracePeriodHours, ok := parseBoundedInt(w, r, "grace_period_hours", 24, 0, maxRotateGraceHours)
	if !ok {
		return
	}

	key, rawKey, err := h.apiKeys.RotateKey(keyID, gracePeriodHours)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	logging.From(r.Context()).Info("Rotated API key", "key_id", safe.LogSafe(keyID))

	resp := key.ToResponse()
	writeJSON(w, r, http.StatusOK, models.APIKeyRotated{
		APIKeyResponse:   resp,
		APIKey:           rawKey,
		GracePeriodHours: gracePeriodHours,
	})
}

// RevokeAPIKey handles POST /keys/{key_id}/revoke
func (h *Handlers) RevokeAPIKey(w http.ResponseWriter, r *http.Request) {
	keyID := chi.URLParam(r, "key_id")
	if err := h.apiKeys.RevokeKey(keyID); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	logging.From(r.Context()).Info("Revoked API key", "key_id", safe.LogSafe(keyID))

	key := h.apiKeys.GetKey(keyID)
	if key == nil {
		writeError(w, http.StatusNotFound, "API key not found")
		return
	}

	writeJSON(w, r, http.StatusOK, key.ToResponse())
}

// DeleteAPIKey handles DELETE /keys/{key_id}
func (h *Handlers) DeleteAPIKey(w http.ResponseWriter, r *http.Request) {
	keyID := chi.URLParam(r, "key_id")
	if err := h.apiKeys.DeleteKey(keyID); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	logging.From(r.Context()).Info("Deleted API key", "key_id", safe.LogSafe(keyID))
	writeJSON(w, r, http.StatusOK, models.DeleteKeyResponse{
		Deleted: true,
		KeyID:   keyID,
	})
}
