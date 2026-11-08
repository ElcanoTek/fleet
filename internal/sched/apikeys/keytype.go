package apikeys

import (
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// KeyType is the access class encoded in a typed API key's prefix
// (fleet_{type}_{base58}). It is legible from the token itself, so middleware
// can enforce route scope before — and independently of — the per-key
// permission set. The type is the authoritative scope: because the WHOLE raw
// key (type segment included) is what gets hashed, a key's wire type can never
// diverge from its stored Type once the hash matches.
type KeyType string

const (
	// KeyTypeAdmin grants full orchestrator access (carries PermissionAdmin).
	KeyTypeAdmin KeyType = "admin"
	// KeyTypeTask submits/manages tasks and reads task logs.
	KeyTypeTask KeyType = "task"
	// KeyTypeWebhook may ONLY hit webhook trigger endpoints, scoped to the
	// specific slugs in the key's AllowedTriggerSlugs.
	KeyTypeWebhook KeyType = "webhook"
	// KeyTypeReadonly is GET-only on tasks/stats/usage endpoints.
	KeyTypeReadonly KeyType = "readonly"
	// KeyTypeUser acts as one person (APIKey.UserEmail) on the chat API —
	// `fleet chat`, `fleet acp`, fleet-bench (ADR-0108). It carries no
	// Operations Center permission and reaches no scoped-key route: what it
	// may do is its user's account, resolved on every request (disable the
	// account and the key stops working).
	KeyTypeUser KeyType = "user"

	// KeyTypeLegacy is the zero value: an untyped key minted before typed keys
	// existed (the "sk-" format). Legacy keys are NOT subject to the type-scope
	// gate — they fall back to their stored permission set exactly as before, so
	// adopting typed keys never changes the behavior of a deployed sk- key.
	KeyTypeLegacy KeyType = ""
)

const (
	// typedKeyPrefix is the wire prefix for a typed key: fleet_{type}_{base58}.
	typedKeyPrefix = "fleet_"
	// legacyKeyPrefix is the wire prefix for the pre-typed (untyped) key format.
	legacyKeyPrefix = "sk-"
	// keySuffixLen is the number of base58 characters in a typed key's random
	// suffix. 32 base58 chars carry ~187 bits of entropy — comfortably above the
	// 128-bit floor for an unguessable bearer token.
	keySuffixLen = 32
)

// Valid reports whether kt is one of the real key types (i.e. not the
// legacy zero value and not an unknown string).
func (kt KeyType) Valid() bool {
	switch kt {
	case KeyTypeAdmin, KeyTypeTask, KeyTypeWebhook, KeyTypeReadonly, KeyTypeUser:
		return true
	default:
		return false
	}
}

// Permissions returns the permission set a freshly minted key of this type
// carries. The type IS the role for typed keys; deriving perms here keeps the
// existing per-handler permission checks working unchanged.
//   - admin    → full access (PermissionAdmin)
//   - task     → create/view tasks + read logs (mirrors the legacy "client" role)
//   - readonly → view tasks + read logs (mirrors the legacy "readonly" role)
//   - webhook  → no orchestrator task permissions; access is the trigger
//     endpoints only, gated by slug, not by these permissions.
func (kt KeyType) Permissions() []models.Permission {
	switch kt {
	case KeyTypeAdmin:
		return []models.Permission{models.PermissionAdmin}
	case KeyTypeTask:
		return []models.Permission{models.PermissionCreateTask, models.PermissionViewTasks, models.PermissionViewLogs}
	case KeyTypeReadonly:
		return []models.Permission{models.PermissionViewTasks, models.PermissionViewLogs}
	case KeyTypeWebhook, KeyTypeUser:
		return []models.Permission{}
	default:
		return []models.Permission{}
	}
}

// Surface is an HTTP surface that authenticates API keys. Every edge names
// its surface when it validates a key (Manager.ValidateKeyOn,
// LookupKeyTypeOn, LookupKeyMetaOn), and KeyType.AllowsSurface — this one
// table — decides which key types it recognises, so no edge has to remember
// which types to turn away (ADR-0108).
type Surface string

const (
	// SurfaceOrchestrator is the orchestrator HTTP API: /v1 and the bare
	// routes, the admin group, task create and upload. A recognised key's
	// route scope is then decided by TypeAllowsMethod and its permissions.
	SurfaceOrchestrator Surface = "orchestrator"
	// SurfaceA2A is the A2A JSON-RPC endpoint (/a2a); each method then checks
	// its permission.
	SurfaceA2A Surface = "a2a"
	// SurfaceTrigger is POST /triggers/{slug}, where a key is an alternative to
	// the per-trigger HMAC signature, scoped by AllowedTriggerSlugs.
	SurfaceTrigger Surface = "trigger"
	// SurfaceChat is the chat listener's own port, where a key acts as the
	// person it names (`fleet chat`, `fleet acp`, fleet-bench).
	SurfaceChat Surface = "chat"
)

// Surfaces lists every surface, for tests that walk the table.
var Surfaces = []Surface{SurfaceOrchestrator, SurfaceA2A, SurfaceTrigger, SurfaceChat}

// AllowsSurface reports whether a key of this type authenticates on s at
// all. It only RECOGNISES the key: an orchestrator route still applies
// TypeAllowsMethod (a webhook key there is a definitive 403), A2A each
// method's permission, a trigger the key's slugs.
//   - user          → chat only: it acts as a person and carries no
//     Operations Center scope
//   - webhook       → triggers; recognised on the orchestrator and A2A only to
//     be refused by their scope checks, as before
//   - admin / task / readonly / legacy → the orchestrator and A2A
func (kt KeyType) AllowsSurface(s Surface) bool {
	switch kt {
	case KeyTypeUser:
		return s == SurfaceChat
	case KeyTypeWebhook:
		return s == SurfaceTrigger || s == SurfaceOrchestrator || s == SurfaceA2A
	case KeyTypeAdmin, KeyTypeTask, KeyTypeReadonly, KeyTypeLegacy:
		return s == SurfaceOrchestrator || s == SurfaceA2A
	default:
		return false
	}
}

// TypeAllowsMethod reports whether a typed key of this type may use the given
// HTTP method on the admin-or-user route group (the reads + task mutations
// guarded by AdminOrUserAuthMiddleware). It is the route-scope lattice the
// middleware enforces:
//   - admin / task → any method (task is the task-management class)
//   - readonly     → safe methods only (GET/HEAD/OPTIONS)
//   - webhook      → none (webhook keys belong on /triggers/{slug} only)
//
// It is only consulted for typed keys; legacy keys bypass it entirely.
func TypeAllowsMethod(kt KeyType, method string) bool {
	switch kt {
	case KeyTypeAdmin, KeyTypeTask:
		return true
	case KeyTypeReadonly:
		return method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions
	default: // webhook, user, legacy, unknown
		return false
	}
}

// ParseAPIKey classifies a raw key from its prefix WITHOUT a store lookup, so
// the wire format can be rejected (or a legacy key recognized) before any
// hashing/validation. It returns:
//   - a typed key (fleet_{type}_{base58}) → (its type, the base58 suffix, nil)
//   - a legacy key (sk-...)               → (KeyTypeLegacy, "", nil)
//   - anything else / malformed           → ("", "", error)
//
// The returned suffix is validated to be non-empty base58 so a structurally
// broken fleet_ key is rejected here rather than silently failing the later
// hash lookup.
func ParseAPIKey(raw string) (KeyType, string, error) {
	if strings.HasPrefix(raw, legacyKeyPrefix) {
		return KeyTypeLegacy, "", nil
	}
	if !strings.HasPrefix(raw, typedKeyPrefix) {
		return "", "", errors.New("invalid key prefix: expected fleet_ or sk-")
	}
	parts := strings.SplitN(raw[len(typedKeyPrefix):], "_", 2)
	if len(parts) != 2 {
		return "", "", errors.New("invalid key format: missing type segment")
	}
	kt := KeyType(parts[0])
	if !kt.Valid() {
		return "", "", fmt.Errorf("unknown key type: %q", parts[0])
	}
	suffix := parts[1]
	if suffix == "" || !isBase58(suffix) {
		return "", "", errors.New("invalid key suffix: expected non-empty base58")
	}
	return kt, suffix, nil
}

// base58Alphabet is the Bitcoin base58 alphabet: omits 0/O/I/l (visually
// ambiguous) and +/ (URL-encoding ambiguous), so a key is safe to paste into a
// URL or read aloud without transcription errors.
const base58Alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

// base58Set indexes base58Alphabet for O(1) membership checks.
var base58Set = func() map[byte]struct{} {
	m := make(map[byte]struct{}, len(base58Alphabet))
	for i := 0; i < len(base58Alphabet); i++ {
		m[base58Alphabet[i]] = struct{}{}
	}
	return m
}()

// isBase58 reports whether s is non-empty and consists solely of base58
// characters.
func isBase58(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if _, ok := base58Set[s[i]]; !ok {
			return false
		}
	}
	return true
}

// randomBase58 returns n cryptographically-random base58 characters. It uses
// rejection sampling (discarding byte values ≥ 232 = 58×4) so each character is
// drawn uniformly from the 58-symbol alphabet with no modulo bias.
func randomBase58(n int) (string, error) {
	out := make([]byte, n)
	buf := make([]byte, 1)
	for i := 0; i < n; {
		if _, err := rand.Read(buf); err != nil {
			return "", fmt.Errorf("failed to generate random bytes for API key: %w", err)
		}
		if buf[0] >= 232 { // reject the biased tail [232,256)
			continue
		}
		out[i] = base58Alphabet[buf[0]%58]
		i++
	}
	return string(out), nil
}
