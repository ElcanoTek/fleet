package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/agent"
)

// User-authored personas (docs/USER-PERSONAS.md). DB-owned and per-user: a
// conversation selects one with the persona reference "user:<name>", and only
// the owner's conversations resolve it — they never enter other users' runs
// and never touch the operator's bundle. A user persona is a LAYER over a
// bundle persona (base_persona): the bundle persona's YAML and its manifest
// tool policy still apply, so a user persona can add instructions but cannot
// widen the tools the operator granted.

// UserPersonaRefPrefix marks a conversation persona that names a user persona
// rather than a bundle persona file: "user:<name>". The colon cannot occur in
// a bundle persona basename the composer offers, so the two never collide.
const UserPersonaRefPrefix = agent.UserPersonaRefPrefix

// User-persona statuses. A PROPOSED persona was drafted by the persona_studio
// tool and is inert (not selectable, never resolved for a turn) until its owner
// approves it — the same trust posture as skill and memory proposals: the agent
// suggests, the human decides.
const (
	UserPersonaStatusActive   = "active"
	UserPersonaStatusProposed = "proposed"
)

// User-persona kinds. A twin speaks and prioritizes as its owner would; an
// assistant is a specialist the owner configured. The kind only changes the
// framing sentence the prompt layer opens with.
const (
	UserPersonaKindAssistant = "assistant"
	UserPersonaKindTwin      = "twin"
)

// User-persona autonomy. It shapes the prompt and the default for a patrol
// started from the persona page ("act" turns the chat's auto-approve on,
// "advise" leaves approval cards in place). It is guidance plus that one
// default, not an authorization boundary: critical tools are still governed
// by the conversation's approval setting.
const (
	UserPersonaAutonomyAdvise = "advise"
	UserPersonaAutonomyAct    = "act"
)

// Size caps: a persona is standing instructions, not a document store.
const (
	MaxUserPersonas                = 50
	maxUserPersonaNameLen          = 48
	maxUserPersonaDisplayName      = 80
	maxUserPersonaDescription      = 280
	maxUserPersonaInstructions     = 32 * 1024
	MaxUserPersonaPatrols          = 8
	maxUserPersonaPatrolTitle      = 120
	maxUserPersonaPatrolPrompt     = 4 * 1024
	userPersonaDraftSuffix         = "-draft"
	userPersonaDefaultPatrolZone   = "UTC"
	userPersonaBasePersonaMaxBytes = 128
)

// userPersonaNameShape: lowercase kebab, ≤48 chars, so the "-draft" staging
// name a proposal may need still fits comfortably.
var userPersonaNameShape = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,46}[a-z0-9])?$`)

// ErrUserPersonaInvalid is returned for a malformed persona write.
var ErrUserPersonaInvalid = errors.New("invalid persona")

// ErrUserPersonaNotFound is returned when an id/name isn't owned by the user.
var ErrUserPersonaNotFound = errors.New("persona not found")

// UserPersonaPatrol is one suggested in-chat patrol a persona carries. It is a
// template: nothing is scheduled until the owner starts it.
type UserPersonaPatrol struct {
	Title    string `json:"title"`
	Cron     string `json:"cron"`
	Timezone string `json:"timezone,omitempty"`
	Prompt   string `json:"prompt"`
}

// UserPersona is one user-authored persona.
type UserPersona struct {
	ID           string              `json:"id"`
	UserEmail    string              `json:"-"`
	Name         string              `json:"name"`
	Ref          string              `json:"ref"`
	DisplayName  string              `json:"display_name"`
	Description  string              `json:"description"`
	Instructions string              `json:"instructions"`
	BasePersona  string              `json:"base_persona"`
	Kind         string              `json:"kind"`
	Autonomy     string              `json:"autonomy"`
	Patrols      []UserPersonaPatrol `json:"patrols"`
	Status       string              `json:"status"`
	ReplacesName string              `json:"replaces_name,omitempty"`
	CreatedAt    int64               `json:"created_at"`
	UpdatedAt    int64               `json:"updated_at"`
}

// UserPersonaInput is the writable shape of a persona.
type UserPersonaInput struct {
	Name         string              `json:"name"`
	DisplayName  string              `json:"display_name"`
	Description  string              `json:"description"`
	Instructions string              `json:"instructions"`
	BasePersona  string              `json:"base_persona"`
	Kind         string              `json:"kind"`
	Autonomy     string              `json:"autonomy"`
	Patrols      []UserPersonaPatrol `json:"patrols"`
}

// UserPersonaRef returns the conversation persona reference for name.
func UserPersonaRef(name string) string { return UserPersonaRefPrefix + name }

// ParseUserPersonaRef reports whether persona is a user-persona reference and
// returns the referenced name.
func ParseUserPersonaRef(persona string) (string, bool) {
	p := strings.TrimSpace(persona)
	if !strings.HasPrefix(p, UserPersonaRefPrefix) {
		return "", false
	}
	return strings.TrimPrefix(p, UserPersonaRefPrefix), true
}

// ValidUserPersonaName reports whether name has the user-persona name shape.
func ValidUserPersonaName(name string) bool {
	return userPersonaNameShape.MatchString(name)
}

// normalizeUserPersonaInput trims and validates in, filling the defaults.
// name is validated separately by the callers that set it.
func normalizeUserPersonaInput(in UserPersonaInput) (UserPersonaInput, error) {
	in.Name = strings.TrimSpace(in.Name)
	in.DisplayName = strings.TrimSpace(in.DisplayName)
	in.Description = strings.TrimSpace(in.Description)
	in.Instructions = strings.TrimSpace(in.Instructions)
	in.BasePersona = strings.TrimSpace(in.BasePersona)
	in.Kind = strings.TrimSpace(in.Kind)
	in.Autonomy = strings.TrimSpace(in.Autonomy)
	if in.DisplayName == "" {
		in.DisplayName = in.Name
	}
	if in.Kind == "" {
		in.Kind = UserPersonaKindAssistant
	}
	if in.Autonomy == "" {
		in.Autonomy = UserPersonaAutonomyAdvise
	}
	switch {
	case in.DisplayName == "":
		return in, fmt.Errorf("%w: a display name is required", ErrUserPersonaInvalid)
	case utf8.RuneCountInString(in.DisplayName) > maxUserPersonaDisplayName:
		return in, fmt.Errorf("%w: display name exceeds %d characters", ErrUserPersonaInvalid, maxUserPersonaDisplayName)
	case strings.ContainsAny(in.DisplayName, "\r\n"):
		return in, fmt.Errorf("%w: display name must be a single line", ErrUserPersonaInvalid)
	case utf8.RuneCountInString(in.Description) > maxUserPersonaDescription:
		return in, fmt.Errorf("%w: description exceeds %d characters", ErrUserPersonaInvalid, maxUserPersonaDescription)
	case strings.ContainsAny(in.Description, "\r\n"):
		return in, fmt.Errorf("%w: description must be a single line", ErrUserPersonaInvalid)
	case in.Instructions == "":
		return in, fmt.Errorf("%w: instructions are required (they are what the persona does)", ErrUserPersonaInvalid)
	case len(in.Instructions) > maxUserPersonaInstructions:
		return in, fmt.Errorf("%w: instructions exceed %d bytes", ErrUserPersonaInvalid, maxUserPersonaInstructions)
	case len(in.BasePersona) > userPersonaBasePersonaMaxBytes || strings.ContainsAny(in.BasePersona, `/\:`) || strings.Contains(in.BasePersona, ".."):
		return in, fmt.Errorf("%w: base persona must be a workspace persona name", ErrUserPersonaInvalid)
	case in.Kind != UserPersonaKindAssistant && in.Kind != UserPersonaKindTwin:
		return in, fmt.Errorf("%w: kind must be %q or %q", ErrUserPersonaInvalid, UserPersonaKindAssistant, UserPersonaKindTwin)
	case in.Autonomy != UserPersonaAutonomyAdvise && in.Autonomy != UserPersonaAutonomyAct:
		return in, fmt.Errorf("%w: autonomy must be %q or %q", ErrUserPersonaInvalid, UserPersonaAutonomyAdvise, UserPersonaAutonomyAct)
	case len(in.Patrols) > MaxUserPersonaPatrols:
		return in, fmt.Errorf("%w: at most %d suggested patrols", ErrUserPersonaInvalid, MaxUserPersonaPatrols)
	}
	patrols := make([]UserPersonaPatrol, 0, len(in.Patrols))
	for i, p := range in.Patrols {
		p.Title = strings.TrimSpace(p.Title)
		p.Prompt = strings.TrimSpace(p.Prompt)
		p.Timezone = strings.TrimSpace(p.Timezone)
		if p.Timezone == "" {
			p.Timezone = userPersonaDefaultPatrolZone
		}
		switch {
		case p.Title == "":
			return in, fmt.Errorf("%w: patrol %d needs a title", ErrUserPersonaInvalid, i+1)
		case utf8.RuneCountInString(p.Title) > maxUserPersonaPatrolTitle || strings.ContainsAny(p.Title, "\r\n"):
			return in, fmt.Errorf("%w: patrol %d title must be one line of at most %d characters", ErrUserPersonaInvalid, i+1, maxUserPersonaPatrolTitle)
		case p.Prompt == "":
			return in, fmt.Errorf("%w: patrol %q needs instructions", ErrUserPersonaInvalid, p.Title)
		case len(p.Prompt) > maxUserPersonaPatrolPrompt:
			return in, fmt.Errorf("%w: patrol %q instructions exceed %d bytes", ErrUserPersonaInvalid, p.Title, maxUserPersonaPatrolPrompt)
		}
		// The same cron contract a started patrol must meet (5-field, valid
		// zone, at least MinChatPatrolCronInterval apart), checked now so a
		// suggestion can never be saved that would fail when started.
		spec, tz, _, err := validateAndComputeNextCron(p.Cron, p.Timezone, time.Now())
		if err != nil {
			return in, fmt.Errorf("%w: patrol %q: %s", ErrUserPersonaInvalid, p.Title, strings.TrimPrefix(err.Error(), ErrInvalidChatSchedule.Error()+": "))
		}
		p.Cron, p.Timezone = spec, tz
		patrols = append(patrols, p)
	}
	in.Patrols = patrols
	return in, nil
}

const userPersonaColumns = `id, user_email, name, display_name, description, instructions, base_persona, kind, autonomy, patrols, status, replaces_name, created_at, updated_at`

func scanUserPersona(sc interface{ Scan(...any) error }) (*UserPersona, error) {
	var p UserPersona
	var patrols []byte
	if err := sc.Scan(&p.ID, &p.UserEmail, &p.Name, &p.DisplayName, &p.Description, &p.Instructions, &p.BasePersona, &p.Kind, &p.Autonomy, &patrols, &p.Status, &p.ReplacesName, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return nil, err
	}
	if len(patrols) > 0 {
		if err := json.Unmarshal(patrols, &p.Patrols); err != nil {
			return nil, fmt.Errorf("decode persona patrols: %w", err)
		}
	}
	if p.Patrols == nil {
		p.Patrols = []UserPersonaPatrol{}
	}
	p.Ref = UserPersonaRef(p.Name)
	return &p, nil
}

func marshalUserPersonaPatrols(patrols []UserPersonaPatrol) (string, error) {
	if patrols == nil {
		patrols = []UserPersonaPatrol{}
	}
	b, err := json.Marshal(patrols)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// ListUserPersonas returns the user's personas (active and proposed), by
// display name.
func (s *Store) ListUserPersonas(ctx context.Context, userEmail string) ([]UserPersona, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+userPersonaColumns+`
		FROM user_personas WHERE user_email=$1
		ORDER BY lower(display_name), name`,
		normalizeEmail(userEmail))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []UserPersona{}
	for rows.Next() {
		p, err := scanUserPersona(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// GetUserPersona fetches one owned persona by id.
func (s *Store) GetUserPersona(ctx context.Context, userEmail, id string) (*UserPersona, error) {
	p, err := scanUserPersona(s.db.QueryRowContext(ctx, `
		SELECT `+userPersonaColumns+` FROM user_personas WHERE user_email=$1 AND id=$2`,
		normalizeEmail(userEmail), id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrUserPersonaNotFound
	}
	return p, err
}

// GetUserPersonaByName fetches one owned persona by name (any status; callers
// that run a turn must check Status).
func (s *Store) GetUserPersonaByName(ctx context.Context, userEmail, name string) (*UserPersona, error) {
	p, err := scanUserPersona(s.db.QueryRowContext(ctx, `
		SELECT `+userPersonaColumns+` FROM user_personas WHERE user_email=$1 AND name=$2`,
		normalizeEmail(userEmail), strings.TrimSpace(name)))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrUserPersonaNotFound
	}
	return p, err
}

func (s *Store) checkUserPersonaCapacity(ctx context.Context, email string) error {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM user_personas WHERE user_email=$1`, email).Scan(&n); err != nil {
		return err
	}
	if n >= MaxUserPersonas {
		return fmt.Errorf("%w: you can keep at most %d personas; delete one first", ErrUserPersonaInvalid, MaxUserPersonas)
	}
	return nil
}

// CreateUserPersona inserts a new active persona.
func (s *Store) CreateUserPersona(ctx context.Context, userEmail string, in UserPersonaInput) (*UserPersona, error) {
	email := normalizeEmail(userEmail)
	in, err := normalizeUserPersonaInput(in)
	if err != nil {
		return nil, err
	}
	if !ValidUserPersonaName(in.Name) {
		return nil, fmt.Errorf("%w: name must be lowercase kebab-case (a-z, 0-9, hyphens), max %d chars", ErrUserPersonaInvalid, maxUserPersonaNameLen)
	}
	if err := s.checkUserPersonaCapacity(ctx, email); err != nil {
		return nil, err
	}
	patrols, err := marshalUserPersonaPatrols(in.Patrols)
	if err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	id := uuid.NewString()
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO user_personas (id, user_email, name, display_name, description, instructions, base_persona, kind, autonomy, patrols, status, replaces_name, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10::jsonb,$11,'',$12,$12)`,
		id, email, in.Name, in.DisplayName, in.Description, in.Instructions, in.BasePersona, in.Kind, in.Autonomy, patrols, UserPersonaStatusActive, now)
	if err != nil {
		if pgUniqueViolation(err) {
			return nil, fmt.Errorf("%w: you already have a persona named %q", ErrUserPersonaInvalid, in.Name)
		}
		return nil, err
	}
	return s.GetUserPersona(ctx, email, id)
}

// UpdateUserPersona rewrites an owned ACTIVE persona's fields (full replace).
// The name is fixed at creation — conversations reference the persona by it —
// so in.Name is ignored. A proposal is not edited here: approve it (or delete
// it) instead.
func (s *Store) UpdateUserPersona(ctx context.Context, userEmail, id string, in UserPersonaInput) (*UserPersona, error) {
	email := normalizeEmail(userEmail)
	in, err := normalizeUserPersonaInput(in)
	if err != nil {
		return nil, err
	}
	patrols, err := marshalUserPersonaPatrols(in.Patrols)
	if err != nil {
		return nil, err
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE user_personas
		SET display_name=$3, description=$4, instructions=$5, base_persona=$6, kind=$7, autonomy=$8, patrols=$9::jsonb, updated_at=$10
		WHERE user_email=$1 AND id=$2`,
		email, id, in.DisplayName, in.Description, in.Instructions, in.BasePersona, in.Kind, in.Autonomy, patrols, time.Now().Unix())
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrUserPersonaNotFound
	}
	return s.GetUserPersona(ctx, email, id)
}

// ProposeUserPersona stages an agent-drafted persona (or an agent-drafted
// edit to an existing one) for its owner's review. A new name is staged under
// that name; when the owner already has an ACTIVE persona with the name, the
// draft is staged as "<name>-draft" with replaces_name set, so the live
// persona keeps running unchanged until the owner approves. Re-proposing
// overwrites the pending draft rather than piling up rows.
func (s *Store) ProposeUserPersona(ctx context.Context, userEmail string, in UserPersonaInput) (*UserPersona, error) {
	email := normalizeEmail(userEmail)
	in, err := normalizeUserPersonaInput(in)
	if err != nil {
		return nil, err
	}
	if !ValidUserPersonaName(in.Name) {
		return nil, fmt.Errorf("%w: name must be lowercase kebab-case (a-z, 0-9, hyphens), max %d chars", ErrUserPersonaInvalid, maxUserPersonaNameLen)
	}
	stageName, replaces := in.Name, ""
	existing, err := s.GetUserPersonaByName(ctx, email, in.Name)
	switch {
	case err == nil && existing.Status == UserPersonaStatusActive:
		stageName, replaces = in.Name+userPersonaDraftSuffix, in.Name
	case err != nil && !errors.Is(err, ErrUserPersonaNotFound):
		return nil, err
	}
	if _, gerr := s.GetUserPersonaByName(ctx, email, stageName); errors.Is(gerr, ErrUserPersonaNotFound) {
		if err := s.checkUserPersonaCapacity(ctx, email); err != nil {
			return nil, err
		}
	}
	patrols, err := marshalUserPersonaPatrols(in.Patrols)
	if err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	var id string
	err = s.db.QueryRowContext(ctx, `
		INSERT INTO user_personas (id, user_email, name, display_name, description, instructions, base_persona, kind, autonomy, patrols, status, replaces_name, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10::jsonb,$11,$12,$13,$13)
		ON CONFLICT (user_email, name) DO UPDATE
		SET display_name=EXCLUDED.display_name, description=EXCLUDED.description,
		    instructions=EXCLUDED.instructions, base_persona=EXCLUDED.base_persona,
		    kind=EXCLUDED.kind, autonomy=EXCLUDED.autonomy, patrols=EXCLUDED.patrols,
		    replaces_name=EXCLUDED.replaces_name, updated_at=EXCLUDED.updated_at
		WHERE user_personas.status = $11
		RETURNING id`,
		uuid.NewString(), email, stageName, in.DisplayName, in.Description, in.Instructions, in.BasePersona, in.Kind, in.Autonomy, patrols, UserPersonaStatusProposed, replaces, now).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		// The staging name is held by an ACTIVE persona the user created by
		// hand (e.g. they really have a persona called "<name>-draft").
		return nil, fmt.Errorf("%w: a persona named %q already exists; pick another name", ErrUserPersonaInvalid, stageName)
	}
	if err != nil {
		return nil, err
	}
	return s.GetUserPersona(ctx, email, id)
}

// ApproveUserPersona turns an owned proposal into a usable persona. A draft
// that edits an existing persona is written onto that persona (keeping its
// name, so chats that reference it pick the edit up) and the draft row is
// removed, atomically. Returns the resulting active persona.
func (s *Store) ApproveUserPersona(ctx context.Context, userEmail, id string) (*UserPersona, error) {
	email := normalizeEmail(userEmail)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	draft, err := scanUserPersona(tx.QueryRowContext(ctx, `
		SELECT `+userPersonaColumns+` FROM user_personas WHERE user_email=$1 AND id=$2 FOR UPDATE`,
		email, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrUserPersonaNotFound
	}
	if err != nil {
		return nil, err
	}
	if draft.Status != UserPersonaStatusProposed {
		return nil, fmt.Errorf("%w: this persona is not awaiting approval", ErrUserPersonaInvalid)
	}
	now := time.Now().Unix()
	patrols, err := marshalUserPersonaPatrols(draft.Patrols)
	if err != nil {
		return nil, err
	}
	resultID := draft.ID
	if draft.ReplacesName != "" {
		var targetID string
		terr := tx.QueryRowContext(ctx, `
			SELECT id FROM user_personas WHERE user_email=$1 AND name=$2 AND id<>$3 FOR UPDATE`,
			email, draft.ReplacesName, draft.ID).Scan(&targetID)
		switch {
		case terr == nil:
			if _, err := tx.ExecContext(ctx, `
				UPDATE user_personas
				SET display_name=$3, description=$4, instructions=$5, base_persona=$6, kind=$7, autonomy=$8, patrols=$9::jsonb, status=$10, updated_at=$11
				WHERE user_email=$1 AND id=$2`,
				email, targetID, draft.DisplayName, draft.Description, draft.Instructions, draft.BasePersona, draft.Kind, draft.Autonomy, patrols, UserPersonaStatusActive, now); err != nil {
				return nil, err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM user_personas WHERE user_email=$1 AND id=$2`, email, draft.ID); err != nil {
				return nil, err
			}
			resultID = targetID
		case errors.Is(terr, sql.ErrNoRows):
			// The persona this draft edits was deleted meanwhile: the draft
			// becomes that persona under its original name.
			if _, err := tx.ExecContext(ctx, `
				UPDATE user_personas SET name=$3, status=$4, replaces_name='', updated_at=$5
				WHERE user_email=$1 AND id=$2`,
				email, draft.ID, draft.ReplacesName, UserPersonaStatusActive, now); err != nil {
				return nil, err
			}
		default:
			return nil, terr
		}
	} else if _, err := tx.ExecContext(ctx, `
		UPDATE user_personas SET status=$3, updated_at=$4 WHERE user_email=$1 AND id=$2`,
		email, draft.ID, UserPersonaStatusActive, now); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.GetUserPersona(ctx, email, resultID)
}

// DeleteUserPersona removes an owned persona. Conversations that referenced it
// keep their "user:<name>" persona and run on its base from then on.
func (s *Store) DeleteUserPersona(ctx context.Context, userEmail, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM user_personas WHERE user_email=$1 AND id=$2`, normalizeEmail(userEmail), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrUserPersonaNotFound
	}
	return nil
}

// ChatDigestConversation is one conversation in a UserChatDigest: its title and
// the words its owner typed, newest conversation first, oldest message first.
type ChatDigestConversation struct {
	ID        string   `json:"id"`
	Title     string   `json:"title"`
	Persona   string   `json:"persona"`
	UpdatedAt int64    `json:"updated_at"`
	Messages  []string `json:"messages"`
}

// UserChatDigest returns what the user themselves wrote across their recent
// conversations, for the persona_studio tool to draft a digital twin from.
//
// Only the owner's OWN typed messages are returned — never assistant replies,
// tool output or server-injected context, which can carry text from the web
// or a connector that the user did not write (and that a draft persona must
// not launder into standing instructions). Lockdown conversations, archived or
// deleted conversations and scheduled patrol wakeups are skipped. Each message is cut
// at maxChars runes and the whole digest at maxTotalChars.
func (s *Store) UserChatDigest(ctx context.Context, userEmail string, maxConversations, maxMessagesPerConversation, maxChars, maxTotalChars int) ([]ChatDigestConversation, error) {
	// conversations.user_email is matched as stored, like every other
	// conversation query (the auth layer hands the same string to both).
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, title, persona, updated_at FROM conversations
		WHERE user_email=$1 AND archived_at IS NULL AND deleted_at IS NULL AND NOT lockdown
		ORDER BY updated_at DESC LIMIT $2`,
		userEmail, maxConversations)
	if err != nil {
		return nil, err
	}
	var convs []ChatDigestConversation
	for rows.Next() {
		var c ChatDigestConversation
		if err := rows.Scan(&c.ID, &c.Title, &c.Persona, &c.UpdatedAt); err != nil {
			_ = rows.Close()
			return nil, err
		}
		convs = append(convs, c)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	total := 0
	out := make([]ChatDigestConversation, 0, len(convs))
	for _, c := range convs {
		if total >= maxTotalChars {
			break
		}
		mrows, err := s.db.QueryContext(ctx, `
			SELECT content FROM (
				SELECT id, content FROM messages
				WHERE conversation_id=$1 AND role='user' AND type='text'
				ORDER BY id DESC LIMIT $2
			) recent ORDER BY id ASC`,
			c.ID, maxMessagesPerConversation*2)
		if err != nil {
			return nil, err
		}
		for mrows.Next() {
			var content string
			if err := mrows.Scan(&content); err != nil {
				_ = mrows.Close()
				return nil, err
			}
			var body struct {
				Text string `json:"text"`
			}
			if json.Unmarshal([]byte(content), &body) != nil {
				continue
			}
			text := strings.TrimSpace(body.Text)
			if text == "" || strings.HasPrefix(text, ChatPatrolInputPrefix) {
				continue
			}
			if r := []rune(text); len(r) > maxChars {
				text = string(r[:maxChars]) + "…"
			}
			if total+len(text) > maxTotalChars {
				break
			}
			total += len(text)
			c.Messages = append(c.Messages, text)
		}
		_ = mrows.Close()
		if len(c.Messages) > maxMessagesPerConversation {
			c.Messages = c.Messages[len(c.Messages)-maxMessagesPerConversation:]
		}
		if len(c.Messages) > 0 {
			out = append(out, c)
		}
	}
	return out, nil
}
