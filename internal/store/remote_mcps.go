package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/secretbox"
)

// Per-user remote (hosted) MCP servers + their OAuth state (#443). All access is
// scoped by user_email at the query level so one user can never read or use
// another's connection. OAuth tokens, the DCR client secret, and the RFC 7592
// registration access token are encrypted at rest via the Store's tokenCipher,
// with the AEAD AAD bound to (user_email, canonical url) — see internal/secretbox.
//
// Seats (#988): a user may hold several rows under one name, one per login
// ("account"), each with its own sealed credential and share grants. The AAD
// deliberately stays (owner, url) rather than growing the label: labels are
// renamable, and two seats of one user at one vendor are the same trust
// domain (transplanting a ciphertext between them needs DB write access, which
// already lets an attacker flip is_default or the grants).

// Remote-MCP connection status values.
const (
	RemoteMCPStatusLoginRequired = "login_required"
	RemoteMCPStatusConnected     = "connected"
	RemoteMCPStatusNeedsReauth   = "needs_reauth"
	RemoteMCPStatusError         = "error"
)

// Remote-MCP transports.
const (
	RemoteMCPTransportStreamableHTTP = "streamable_http"
	RemoteMCPTransportSSE            = "sse"
)

// Remote-MCP auth kinds — how a connection authenticates to the server.
const (
	RemoteMCPAuthOAuth  = "oauth"   // OAuth 2.1 discovery + PKCE (tokens in remote_mcp_oauth)
	RemoteMCPAuthOpen   = "open"    // no Authorization header at all
	RemoteMCPAuthAPIKey = "api_key" // static vendor key, sealed in api_key_enc
)

// AAD purpose strings — distinct per secret kind so a ciphertext from one column
// can't be opened as another even within the same (user, server) row. These are
// domain-separation labels (the AEAD's Additional Authenticated Data), NOT
// secrets themselves.
const (
	aadPurposeAccessTok  = "fleet:mcp-oauth-access-token:v1"
	aadPurposeRefreshTok = "fleet:mcp-oauth-refresh-token:v1"
	aadPurposeClientSe   = "fleet:mcp-oauth-client-secret:v1"
	aadPurposeRegTok     = "fleet:mcp-oauth-reg-token:v1"
	aadPurposeFlow       = "fleet:mcp-oauth-flow:v1"
	aadPurposeAPIKey     = "fleet:mcp-api-key:v1" //nolint:gosec // G101: AAD domain-separation label, not a credential
)

var (
	// ErrRemoteMCPNotFound is returned when a server id/name isn't owned by the user.
	ErrRemoteMCPNotFound = errors.New("remote mcp server not found")
	// ErrRemoteMCPNeedsReauth is returned by EnsureFreshToken when the stored
	// refresh token is dead (the connection must be re-authorized by the user).
	ErrRemoteMCPNeedsReauth = errors.New("remote mcp server needs re-authorization")
	// ErrRemoteMCPAccountInvalid is returned for a seat label that does not
	// canonicalize to the public account shape (see CanonicalRemoteMCPAccount).
	ErrRemoteMCPAccountInvalid = errors.New("invalid remote mcp account label")
	// ErrRemoteMCPSeatExists is returned when a (user, name, account) seat
	// already exists — adding a second login under a name requires a label.
	ErrRemoteMCPSeatExists = errors.New("remote mcp seat already exists")
	// ErrOAuthFlowNotFound is returned when a callback state is unknown/expired/used.
	ErrOAuthFlowNotFound = errors.New("oauth flow state not found or expired")
)

// RemoteMCPServer is a user's hosted MCP connection. It carries the OAuth
// discovery + DCR result but NOT the secrets (those decrypt only via the
// dedicated token methods).
type RemoteMCPServer struct {
	ID        string `json:"id"`
	UserEmail string `json:"-"`
	Name      string `json:"name"`
	// Account is this seat's public label under Name (#988): "" is the
	// unlabeled seat every pre-#988 connection is; "work"/"personal" name a
	// further login. Several rows may share a Name, one per seat, each with
	// its own sealed credential. Registered in a run as
	// agentcore.RegisteredMCPName(Name, Account) — the bundle seat formula.
	Account string `json:"account"`
	// IsDefault marks the seat a user's chats mount for Name when neither the
	// conversation nor a task pins another. Exactly one per (user, name).
	IsDefault             bool   `json:"is_default"`
	URL                   string `json:"url"`
	Transport             string `json:"transport"`
	Status                string `json:"status"`
	StatusDetail          string `json:"status_detail,omitempty"`
	AuthKind              string `json:"auth_kind,omitempty"` // oauth | open | api_key (non-secret; drives the UI's connect affordance)
	APIKeyHeader          string `json:"-"`                   // header NAME the sealed key is sent under; "" = Authorization: Bearer
	APIKeyQuery           string `json:"-"`                   // query-parameter NAME the sealed key is sent under; "" = not query-authenticated
	Issuer                string `json:"-"`
	AuthorizationEndpoint string `json:"-"`
	TokenEndpoint         string `json:"-"`
	RegistrationEndpoint  string `json:"-"`
	RevocationEndpoint    string `json:"-"`
	Scopes                string `json:"-"` // space-delimited
	AuthMethods           string `json:"-"` // space-delimited
	ClientID              string `json:"-"`
	CreatedAt             int64  `json:"created_at"`
	UpdatedAt             int64  `json:"updated_at"`
}

// RemoteMCPServerInput is the full server row to insert after discovery + DCR.
type RemoteMCPServerInput struct {
	UserEmail             string
	Name                  string
	Account               string // seat label (#988); canonicalized; "" = the unlabeled seat
	URL                   string // canonical
	Transport             string
	Issuer                string
	AuthorizationEndpoint string
	TokenEndpoint         string
	RegistrationEndpoint  string
	RevocationEndpoint    string
	Scopes                string
	AuthMethods           string
	ClientID              string
	ClientSecret          string // plaintext; encrypted before insert ("" → NULL)
	RegistrationToken     string // plaintext RFC 7592 token; encrypted ("" → NULL)
	Status                string // empty defaults to login_required; open/api_key servers use connected
	AuthKind              string // empty defaults to oauth
	APIKeyHeader          string // api_key only: header NAME ("" = Authorization: Bearer)
	APIKeyQuery           string // api_key only: query-parameter NAME ("" = header auth)
	APIKey                string // api_key only: plaintext; encrypted before insert
}

// RemoteMCPTokens is a decrypted token pair plus its expiry (unix seconds).
type RemoteMCPTokens struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    int64
}

// sealSecret encrypts plaintext under the Store cipher with AAD bound to
// (purpose, email, url). Empty plaintext stores NULL.
func (s *Store) sealSecret(plaintext, purpose, email, url string) ([]byte, error) {
	if plaintext == "" {
		return nil, nil
	}
	if s.tokenCipher == nil {
		return nil, secretbox.ErrNoCipher
	}
	return s.tokenCipher.Seal([]byte(plaintext), secretbox.AAD(purpose, email, url))
}

// openSecret decrypts ciphertext with the same AAD. NULL/empty → "".
func (s *Store) openSecret(ciphertext []byte, purpose, email, url string) (string, error) {
	if len(ciphertext) == 0 {
		return "", nil
	}
	if s.tokenCipher == nil {
		return "", secretbox.ErrNoCipher
	}
	pt, err := s.tokenCipher.Open(ciphertext, secretbox.AAD(purpose, email, url))
	if err != nil {
		return "", err
	}
	return string(pt), nil
}

// remoteMCPAccountRE is the canonical public shape of a seat label: the same
// character class the env-seat convention produces after CanonicalAccount
// (lowercased here so "Work" and "work" cannot become two seats), bounded so
// it stays a readable tool-name segment (mcp_<name>_<account>_<tool>).
var remoteMCPAccountRE = regexp.MustCompile(`^[a-z0-9_]{1,32}$`)

// remoteMCPAccountFolder mirrors creds.CanonicalAccount's separator folding
// (the store cannot import creds without a cycle).
var remoteMCPAccountFolder = strings.NewReplacer("-", "_", " ", "_")

// CanonicalRemoteMCPAccount normalizes a seat label the way the run path will
// key on it: trimmed, lowercased, hyphens/spaces folded to underscore. "" (the
// unlabeled seat) passes through. Anything that does not then match
// [a-z0-9_]{1,32} is rejected with ErrRemoteMCPAccountInvalid so a label can
// never break the mcp_<name>_<account>_<tool> identity or collide with a
// differently-cased twin.
func CanonicalRemoteMCPAccount(label string) (string, error) {
	acct := strings.ToLower(remoteMCPAccountFolder.Replace(strings.TrimSpace(label)))
	if acct == "" {
		return "", nil
	}
	if !remoteMCPAccountRE.MatchString(acct) {
		return "", fmt.Errorf("%w: %q (use 1-32 letters, digits, or underscores, e.g. work or personal)", ErrRemoteMCPAccountInvalid, label)
	}
	return acct, nil
}

// CreateRemoteMCPServer inserts a discovered server in status login_required.
// Fails with ErrRemoteMCPSeatExists if the (user, name, account) seat already
// exists. The first seat under a name becomes the default (#988); a further
// seat never displaces it.
func (s *Store) CreateRemoteMCPServer(ctx context.Context, in RemoteMCPServerInput) (*RemoteMCPServer, error) {
	if s.tokenCipher == nil {
		return nil, secretbox.ErrNoCipher
	}
	email := normalizeEmail(in.UserEmail)
	if email == "" || in.Name == "" || in.URL == "" {
		return nil, errors.New("user email, name, and url are required")
	}
	account, err := CanonicalRemoteMCPAccount(in.Account)
	if err != nil {
		return nil, err
	}
	secretEnc, err := s.sealSecret(in.ClientSecret, aadPurposeClientSe, email, in.URL)
	if err != nil {
		return nil, fmt.Errorf("encrypt client secret: %w", err)
	}
	regEnc, err := s.sealSecret(in.RegistrationToken, aadPurposeRegTok, email, in.URL)
	if err != nil {
		return nil, fmt.Errorf("encrypt registration token: %w", err)
	}
	apiKeyEnc, err := s.sealSecret(in.APIKey, aadPurposeAPIKey, email, in.URL)
	if err != nil {
		return nil, fmt.Errorf("encrypt api key: %w", err)
	}
	id := uuid.NewString()
	now := time.Now().Unix()
	status := in.Status
	if status == "" {
		status = RemoteMCPStatusLoginRequired
	}
	authKind := in.AuthKind
	if authKind == "" {
		authKind = RemoteMCPAuthOAuth
	}
	// is_default is decided in the same statement that inserts the row: the
	// seat is the default iff no other seat of this name already holds it. The
	// partial unique index makes a concurrent double-insert fail loudly rather
	// than yield two defaults.
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO remote_mcp_servers (
			id, user_email, name, url, transport, status, status_detail,
			issuer, authorization_endpoint, token_endpoint, registration_endpoint, revocation_endpoint,
			scopes, auth_methods, client_id, client_secret_enc, registration_access_token_enc,
			auth_kind, api_key_header, api_key_query, api_key_enc,
			account, is_default,
			created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,'',$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,
			$22,
			NOT EXISTS (SELECT 1 FROM remote_mcp_servers d WHERE d.user_email = $2 AND d.name = $3 AND d.is_default),
			$21,$21)`,
		id, email, in.Name, in.URL, in.Transport, status,
		in.Issuer, in.AuthorizationEndpoint, in.TokenEndpoint, in.RegistrationEndpoint, in.RevocationEndpoint,
		in.Scopes, in.AuthMethods, in.ClientID, secretEnc, regEnc,
		authKind, in.APIKeyHeader, in.APIKeyQuery, apiKeyEnc, now, account)
	if err != nil {
		if pgUniqueViolation(err) {
			if account == "" {
				return nil, fmt.Errorf("%w: a connection named %q already exists — give this login an account label (e.g. work or personal) to add it as another account", ErrRemoteMCPSeatExists, in.Name)
			}
			return nil, fmt.Errorf("%w: a connection named %q with account %q already exists", ErrRemoteMCPSeatExists, in.Name, account)
		}
		return nil, err
	}
	return s.GetRemoteMCPServer(ctx, email, id)
}

const remoteMCPColumns = `id, user_email, name, url, transport, status, status_detail,
	issuer, authorization_endpoint, token_endpoint, registration_endpoint, revocation_endpoint,
	scopes, auth_methods, client_id, auth_kind, api_key_header, api_key_query, account, is_default, created_at, updated_at`

func scanRemoteMCPServer(row interface{ Scan(...any) error }) (*RemoteMCPServer, error) {
	var m RemoteMCPServer
	if err := row.Scan(&m.ID, &m.UserEmail, &m.Name, &m.URL, &m.Transport, &m.Status, &m.StatusDetail,
		&m.Issuer, &m.AuthorizationEndpoint, &m.TokenEndpoint, &m.RegistrationEndpoint, &m.RevocationEndpoint,
		&m.Scopes, &m.AuthMethods, &m.ClientID, &m.AuthKind, &m.APIKeyHeader, &m.APIKeyQuery, &m.Account, &m.IsDefault, &m.CreatedAt, &m.UpdatedAt); err != nil {
		return nil, err
	}
	return &m, nil
}

// GetRemoteMCPServer fetches one server scoped to the user.
func (s *Store) GetRemoteMCPServer(ctx context.Context, userEmail, id string) (*RemoteMCPServer, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+remoteMCPColumns+` FROM remote_mcp_servers WHERE user_email = $1 AND id = $2`,
		normalizeEmail(userEmail), id)
	m, err := scanRemoteMCPServer(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrRemoteMCPNotFound
	}
	return m, err
}

// ListRemoteMCPServers returns all of a user's servers (no secrets), newest first.
func (s *Store) ListRemoteMCPServers(ctx context.Context, userEmail string) ([]RemoteMCPServer, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+remoteMCPColumns+` FROM remote_mcp_servers WHERE user_email = $1 ORDER BY created_at DESC`,
		normalizeEmail(userEmail))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RemoteMCPServer
	for rows.Next() {
		m, err := scanRemoteMCPServer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

// DeleteRemoteMCPServer removes a server (cascading its oauth + flow rows).
// Removing the default seat of a name promotes another seat of that name
// (the unlabeled one first, else the oldest) so the name never has no
// default while it still has seats (#988).
func (s *Store) DeleteRemoteMCPServer(ctx context.Context, userEmail, id string) error {
	email := normalizeEmail(userEmail)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var name string
	var wasDefault bool
	err = tx.QueryRowContext(ctx,
		`DELETE FROM remote_mcp_servers WHERE user_email = $1 AND id = $2 RETURNING name, is_default`,
		email, id).Scan(&name, &wasDefault)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrRemoteMCPNotFound
	}
	if err != nil {
		return err
	}
	if wasDefault {
		if _, err := tx.ExecContext(ctx, `
			UPDATE remote_mcp_servers SET is_default = TRUE, updated_at = $3
			WHERE id = (
				SELECT id FROM remote_mcp_servers
				WHERE user_email = $1 AND name = $2
				ORDER BY (account = '') DESC, created_at ASC, id ASC
				LIMIT 1)`,
			email, name, time.Now().Unix()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SetRemoteMCPDefaultSeat makes the seat id the default among the owner's
// seats of the same name (#988). Owner-only; a foreign or unknown id is
// ErrRemoteMCPNotFound. Idempotent.
func (s *Store) SetRemoteMCPDefaultSeat(ctx context.Context, userEmail, id string) error {
	email := normalizeEmail(userEmail)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var name string
	err = tx.QueryRowContext(ctx,
		`SELECT name FROM remote_mcp_servers WHERE user_email = $1 AND id = $2 FOR UPDATE`,
		email, id).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrRemoteMCPNotFound
	}
	if err != nil {
		return err
	}
	now := time.Now().Unix()
	// Clear first, then set: the partial unique index would reject the
	// transient two-defaults state of the opposite order.
	if _, err := tx.ExecContext(ctx,
		`UPDATE remote_mcp_servers SET is_default = FALSE, updated_at = $3 WHERE user_email = $1 AND name = $2 AND is_default AND id <> $4`,
		email, name, now, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE remote_mcp_servers SET is_default = TRUE, updated_at = $3 WHERE user_email = $1 AND id = $2 AND NOT is_default`,
		email, id, now); err != nil {
		return err
	}
	return tx.Commit()
}

// RenameRemoteMCPAccount changes a seat's public label (#988). Secrets are
// unaffected: their AAD binds (owner, url), not the label. The new label must
// be free under the same name (ErrRemoteMCPSeatExists otherwise).
func (s *Store) RenameRemoteMCPAccount(ctx context.Context, userEmail, id, label string) error {
	account, err := CanonicalRemoteMCPAccount(label)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE remote_mcp_servers SET account = $3, updated_at = $4 WHERE user_email = $1 AND id = $2`,
		normalizeEmail(userEmail), id, account, time.Now().Unix())
	if err != nil {
		if pgUniqueViolation(err) {
			if account == "" {
				return fmt.Errorf("%w: this connection already has an unlabeled seat", ErrRemoteMCPSeatExists)
			}
			return fmt.Errorf("%w: a seat labeled %q already exists for this connection", ErrRemoteMCPSeatExists, account)
		}
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrRemoteMCPNotFound
	}
	return nil
}

// SetRemoteMCPStatus updates the connection status + a non-secret detail string.
func (s *Store) SetRemoteMCPStatus(ctx context.Context, userEmail, id, status, detail string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE remote_mcp_servers SET status = $3, status_detail = $4, updated_at = $5 WHERE user_email = $1 AND id = $2`,
		normalizeEmail(userEmail), id, status, detail, time.Now().Unix())
	return err
}

// LoadServerSecrets reads + decrypts the client secret and registration token
// for a server (host-side use only; never returned to a browser).
func (s *Store) LoadServerSecrets(ctx context.Context, server *RemoteMCPServer) (clientSecret, registrationToken string, err error) {
	var secretEnc, regEnc []byte
	row := s.db.QueryRowContext(ctx,
		`SELECT client_secret_enc, registration_access_token_enc FROM remote_mcp_servers WHERE user_email = $1 AND id = $2`,
		normalizeEmail(server.UserEmail), server.ID)
	if err := row.Scan(&secretEnc, &regEnc); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", "", ErrRemoteMCPNotFound
		}
		return "", "", err
	}
	clientSecret, err = s.openSecret(secretEnc, aadPurposeClientSe, server.UserEmail, server.URL)
	if err != nil {
		return "", "", fmt.Errorf("decrypt client secret: %w", err)
	}
	registrationToken, err = s.openSecret(regEnc, aadPurposeRegTok, server.UserEmail, server.URL)
	if err != nil {
		return "", "", fmt.Errorf("decrypt registration token: %w", err)
	}
	return clientSecret, registrationToken, nil
}

// GetRemoteMCPAPIKey reads + decrypts an api_key server's static key (host-side
// use only; never returned to a browser). The AAD is bound to the OWNER's email
// (server.UserEmail), so shared connections open under the owner's identity.
func (s *Store) GetRemoteMCPAPIKey(ctx context.Context, server *RemoteMCPServer) (string, error) {
	var keyEnc []byte
	row := s.db.QueryRowContext(ctx,
		`SELECT api_key_enc FROM remote_mcp_servers WHERE user_email = $1 AND id = $2`,
		normalizeEmail(server.UserEmail), server.ID)
	if err := row.Scan(&keyEnc); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrRemoteMCPNotFound
		}
		return "", err
	}
	key, err := s.openSecret(keyEnc, aadPurposeAPIKey, server.UserEmail, server.URL)
	if err != nil {
		return "", fmt.Errorf("decrypt api key: %w", err)
	}
	return key, nil
}

// SetRemoteMCPAPIKey replaces an api_key server's sealed key (rotation) and
// marks the connection usable again. Scoped to the owner; a grantee of a shared
// connection can never rotate the owner's key.
func (s *Store) SetRemoteMCPAPIKey(ctx context.Context, userEmail, id, apiKey string) error {
	email := normalizeEmail(userEmail)
	server, err := s.GetRemoteMCPServer(ctx, email, id)
	if err != nil {
		return err
	}
	if server.AuthKind != RemoteMCPAuthAPIKey {
		return errors.New("this connection does not use an API key")
	}
	keyEnc, err := s.sealSecret(apiKey, aadPurposeAPIKey, email, server.URL)
	if err != nil {
		return fmt.Errorf("encrypt api key: %w", err)
	}
	_, err = s.db.ExecContext(ctx,
		`UPDATE remote_mcp_servers SET api_key_enc = $3, status = $4, status_detail = '', updated_at = $5
		 WHERE user_email = $1 AND id = $2`,
		email, id, keyEnc, RemoteMCPStatusConnected, time.Now().Unix())
	return err
}

// StoreOAuthTokens upserts a server's tokens (encrypted) and marks it connected.
func (s *Store) StoreOAuthTokens(ctx context.Context, server *RemoteMCPServer, tokens RemoteMCPTokens) error {
	email := normalizeEmail(server.UserEmail)
	accessEnc, err := s.sealSecret(tokens.AccessToken, aadPurposeAccessTok, email, server.URL)
	if err != nil {
		return fmt.Errorf("encrypt access token: %w", err)
	}
	refreshEnc, err := s.sealSecret(tokens.RefreshToken, aadPurposeRefreshTok, email, server.URL)
	if err != nil {
		return fmt.Errorf("encrypt refresh token: %w", err)
	}
	now := time.Now().Unix()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO remote_mcp_oauth (server_id, access_token_enc, refresh_token_enc, expires_at, last_refreshed_at, failed_refresh_count)
		VALUES ($1,$2,$3,$4,$5,0)
		ON CONFLICT (server_id) DO UPDATE SET
			access_token_enc = EXCLUDED.access_token_enc,
			refresh_token_enc = EXCLUDED.refresh_token_enc,
			expires_at = EXCLUDED.expires_at,
			last_refreshed_at = EXCLUDED.last_refreshed_at,
			failed_refresh_count = 0`,
		server.ID, accessEnc, refreshEnc, tokens.ExpiresAt, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE remote_mcp_servers SET status = $2, status_detail = '', updated_at = $3 WHERE id = $1`,
		server.ID, RemoteMCPStatusConnected, now); err != nil {
		return err
	}
	return tx.Commit()
}

// ClearOAuthTokens deletes a server's stored tokens and marks it
// needs_reauth — "sign out": the registration (URL, sealed client secret,
// shares) survives, so Reconnect works without re-entering credentials.
// The Remove path (DeleteRemoteMCPServer) is the one that erases everything.
func (s *Store) ClearOAuthTokens(ctx context.Context, userEmail, id string) error {
	email := normalizeEmail(userEmail)
	now := time.Now().Unix()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx,
		`UPDATE remote_mcp_servers SET status = $3, status_detail = $4, updated_at = $5
		 WHERE id = $1 AND user_email = $2`,
		id, email, RemoteMCPStatusNeedsReauth, "signed out — reconnect to use", now)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return ErrRemoteMCPNotFound
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM remote_mcp_oauth WHERE server_id = $1`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// GetOAuthTokens decrypts and returns a server's current tokens WITHOUT
// refreshing. Used for best-effort revocation on disconnect. Returns
// ErrRemoteMCPNeedsReauth when the server was never authorized.
func (s *Store) GetOAuthTokens(ctx context.Context, server *RemoteMCPServer) (*RemoteMCPTokens, error) {
	email := normalizeEmail(server.UserEmail)
	var accessEnc, refreshEnc []byte
	var expiresAt int64
	row := s.db.QueryRowContext(ctx,
		`SELECT access_token_enc, refresh_token_enc, expires_at FROM remote_mcp_oauth WHERE server_id = $1`,
		server.ID)
	if err := row.Scan(&accessEnc, &refreshEnc, &expiresAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrRemoteMCPNeedsReauth
		}
		return nil, err
	}
	t := &RemoteMCPTokens{ExpiresAt: expiresAt}
	var err error
	if t.AccessToken, err = s.openSecret(accessEnc, aadPurposeAccessTok, email, server.URL); err != nil {
		return nil, err
	}
	if t.RefreshToken, err = s.openSecret(refreshEnc, aadPurposeRefreshTok, email, server.URL); err != nil {
		return nil, err
	}
	return t, nil
}

// RefreshResult is what a RefreshFunc returns: either fresh tokens, or a signal
// that the grant is dead and the connection needs re-authorization.
type RefreshResult struct {
	Tokens     RemoteMCPTokens
	NeedReauth bool
	// ReauthDetail is the user-facing reason shown in the connections UI when
	// NeedReauth is set. Empty falls back to the generic expiry wording, so a
	// caller that doesn't classify keeps the previous behavior.
	ReauthDetail string
}

// RefreshFunc performs the network token refresh for the given current tokens.
// It MUST be bounded by a timeout (it runs while a DB row lock is held). Return
// NeedReauth=true with a nil error for a terminal failure (the grant or client
// registration itself is dead — retrying cannot help); return a non-nil error
// for transient failures (the transaction rolls back).
type RefreshFunc func(ctx context.Context, current RemoteMCPTokens) (RefreshResult, error)

// EnsureFreshToken returns a valid access token for server, refreshing if the
// stored one expires within marginSeconds. It serializes concurrent callers for
// the same server with SELECT ... FOR UPDATE and double-checks expiry after
// acquiring the lock, so two simultaneous chat turns / scheduled tasks issue at
// most one network refresh and both observe the rotated token. On a terminal
// refresh failure it marks the server needs_reauth and returns
// ErrRemoteMCPNeedsReauth so the caller degrades gracefully.
//
// The refresh HTTP call happens while the row lock is held; refreshFn MUST bound
// it with a timeout. This is the simplest correct implementation for OAuth 2.1
// single-use refresh-token rotation; a lease pattern is the scale upgrade.
func (s *Store) EnsureFreshToken(ctx context.Context, server *RemoteMCPServer, marginSeconds int64, refreshFn RefreshFunc) (string, error) {
	email := normalizeEmail(server.UserEmail)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	var accessEnc, refreshEnc []byte
	var expiresAt int64
	row := tx.QueryRowContext(ctx,
		`SELECT access_token_enc, refresh_token_enc, expires_at FROM remote_mcp_oauth WHERE server_id = $1 FOR UPDATE`,
		server.ID)
	if err := row.Scan(&accessEnc, &refreshEnc, &expiresAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrRemoteMCPNeedsReauth // never authorized
		}
		return "", err
	}

	current := RemoteMCPTokens{ExpiresAt: expiresAt}
	if current.AccessToken, err = s.openSecret(accessEnc, aadPurposeAccessTok, email, server.URL); err != nil {
		return "", fmt.Errorf("decrypt access token: %w", err)
	}
	if current.RefreshToken, err = s.openSecret(refreshEnc, aadPurposeRefreshTok, email, server.URL); err != nil {
		return "", fmt.Errorf("decrypt refresh token: %w", err)
	}

	now := time.Now().Unix()
	// Double-checked after acquiring the lock (another worker may have refreshed
	// while we blocked). Two "usable as-is" cases:
	//   - a token still comfortably within its expiry margin; or
	//   - a token the AS issued with NO expiry signal (expires_at == 0): we have
	//     no basis to call it stale, so we use it rather than (a) forcing reauth
	//     when there's no refresh token, or (b) rotating it on every single call
	//     when there is one. If such a token is in fact revoked, the next MCP call
	//     gets a 401 the model sees, and the user reconnects.
	if current.AccessToken != "" && (expiresAt == 0 || expiresAt-now > marginSeconds) {
		committed = true
		return current.AccessToken, tx.Commit()
	}
	if current.RefreshToken == "" {
		return "", ErrRemoteMCPNeedsReauth
	}

	res, ferr := refreshFn(ctx, current)
	if ferr != nil {
		return "", ferr // transient → rollback (defer)
	}
	if res.NeedReauth {
		// Terminal: mark needs_reauth + bump the failure counter, then COMMIT so
		// the status sticks. Return the sentinel for graceful degradation.
		detail := res.ReauthDetail
		if detail == "" {
			detail = "authorization expired — reconnect required"
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE remote_mcp_oauth SET failed_refresh_count = failed_refresh_count + 1 WHERE server_id = $1`, server.ID); err != nil {
			return "", err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE remote_mcp_servers SET status = $2, status_detail = $3, updated_at = $4 WHERE id = $1`,
			server.ID, RemoteMCPStatusNeedsReauth, detail, now); err != nil {
			return "", err
		}
		committed = true
		if err := tx.Commit(); err != nil {
			return "", err
		}
		return "", ErrRemoteMCPNeedsReauth
	}

	// Success: write the rotated tokens + access token in this same transaction
	// and commit immediately, minimizing the burned-token window.
	accessEnc2, err := s.sealSecret(res.Tokens.AccessToken, aadPurposeAccessTok, email, server.URL)
	if err != nil {
		return "", fmt.Errorf("encrypt access token: %w", err)
	}
	refreshEnc2, err := s.sealSecret(res.Tokens.RefreshToken, aadPurposeRefreshTok, email, server.URL)
	if err != nil {
		return "", fmt.Errorf("encrypt refresh token: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE remote_mcp_oauth SET access_token_enc = $2, refresh_token_enc = $3, expires_at = $4,
			last_refreshed_at = $5, failed_refresh_count = 0 WHERE server_id = $1`,
		server.ID, accessEnc2, refreshEnc2, res.Tokens.ExpiresAt, now); err != nil {
		return "", err
	}
	// Re-assert connected status in case it had drifted.
	if _, err := tx.ExecContext(ctx,
		`UPDATE remote_mcp_servers SET status = $2, status_detail = '', updated_at = $3 WHERE id = $1 AND status <> $2`,
		server.ID, RemoteMCPStatusConnected, now); err != nil {
		return "", err
	}
	committed = true
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return res.Tokens.AccessToken, nil
}

// BeginOAuthFlow stores the single-use PKCE/CSRF state for an in-flight
// authorization. codeVerifier is encrypted with AAD bound to (email, state).
func (s *Store) BeginOAuthFlow(ctx context.Context, state, serverID, userEmail, codeVerifier string, ttl time.Duration) error {
	email := normalizeEmail(userEmail)
	verEnc, err := s.sealSecret(codeVerifier, aadPurposeFlow, email, state)
	if err != nil {
		return fmt.Errorf("encrypt code verifier: %w", err)
	}
	now := time.Now()
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO remote_mcp_oauth_flow (state, server_id, user_email, code_verifier_enc, created_at, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6)`,
		state, serverID, email, verEnc, now.Unix(), now.Add(ttl).Unix())
	return err
}

// OAuthFlowState is a consumed authorization flow record.
type OAuthFlowState struct {
	ServerID     string
	UserEmail    string
	CodeVerifier string
}

// ConsumeOAuthFlow atomically loads-and-deletes the flow for state, enforcing
// single-use and expiry. Returns ErrOAuthFlowNotFound when absent/expired.
func (s *Store) ConsumeOAuthFlow(ctx context.Context, state string) (*OAuthFlowState, error) {
	now := time.Now().Unix()
	var serverID, email string
	var verEnc []byte
	var expiresAt int64
	// DELETE ... RETURNING is atomic: a concurrent replay finds no row.
	row := s.db.QueryRowContext(ctx,
		`DELETE FROM remote_mcp_oauth_flow WHERE state = $1 RETURNING server_id, user_email, code_verifier_enc, expires_at`,
		state)
	if err := row.Scan(&serverID, &email, &verEnc, &expiresAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrOAuthFlowNotFound
		}
		return nil, err
	}
	if expiresAt < now {
		return nil, ErrOAuthFlowNotFound
	}
	verifier, err := s.openSecret(verEnc, aadPurposeFlow, email, state)
	if err != nil {
		return nil, fmt.Errorf("decrypt code verifier: %w", err)
	}
	return &OAuthFlowState{ServerID: serverID, UserEmail: email, CodeVerifier: verifier}, nil
}

// SweepExpiredOAuthFlows deletes abandoned authorization flows past their TTL.
func (s *Store) SweepExpiredOAuthFlows(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM remote_mcp_oauth_flow WHERE expires_at < $1`, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}
