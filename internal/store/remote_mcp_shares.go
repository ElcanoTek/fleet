package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Sharing a remote MCP connection (#443 follow-up). A grant lets another user
// on this box USE the owner's connected server: their runs mount its tools and
// authenticate with the owner's OAuth token host-side. The grantee never sees
// the token — secrets stay AEAD-sealed to the (owner email, url) AAD and only
// decrypt through the owner's row — and revocation is immediate because every
// run resolves grants fresh. GranteeEveryone ('*') shares with all users.

// GranteeEveryone is the wildcard grantee meaning every user on this box.
const GranteeEveryone = "*"

// ErrRemoteMCPShareInvalid is returned for a malformed or self-targeted grant.
var ErrRemoteMCPShareInvalid = errors.New("invalid share grantee")

// normalizeGrantee canonicalizes a grantee: the wildcard passes through, and
// anything else is treated as a user email.
func normalizeGrantee(g string) string {
	g = strings.TrimSpace(g)
	if g == GranteeEveryone {
		return g
	}
	return normalizeEmail(g)
}

// ownsRemoteMCPServer verifies the server exists and belongs to owner — share
// management is an owner-only surface.
func (s *Store) ownsRemoteMCPServer(ctx context.Context, ownerEmail, serverID string) error {
	var one int
	err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM remote_mcp_servers WHERE user_email = $1 AND id = $2`,
		normalizeEmail(ownerEmail), serverID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrRemoteMCPNotFound
	}
	return err
}

// ShareRemoteMCPServer grants grantee (an email, or GranteeEveryone) use of the
// owner's server. Idempotent: re-granting is a no-op.
func (s *Store) ShareRemoteMCPServer(ctx context.Context, ownerEmail, serverID, grantee string) error {
	owner := normalizeEmail(ownerEmail)
	g := normalizeGrantee(grantee)
	if g == "" || !strings.Contains(g, "@") && g != GranteeEveryone {
		return fmt.Errorf("%w: %q (want a user email or %q)", ErrRemoteMCPShareInvalid, grantee, GranteeEveryone)
	}
	if g == owner {
		return fmt.Errorf("%w: cannot share a server with its owner", ErrRemoteMCPShareInvalid)
	}
	if err := s.ownsRemoteMCPServer(ctx, owner, serverID); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO remote_mcp_shares (server_id, grantee, created_at)
		VALUES ($1, $2, $3) ON CONFLICT (server_id, grantee) DO NOTHING`,
		serverID, g, time.Now().Unix())
	return err
}

// UnshareRemoteMCPServer revokes a grant. Unknown grants error so a typo'd
// revocation can't read as success.
func (s *Store) UnshareRemoteMCPServer(ctx context.Context, ownerEmail, serverID, grantee string) error {
	if err := s.ownsRemoteMCPServer(ctx, normalizeEmail(ownerEmail), serverID); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM remote_mcp_shares WHERE server_id = $1 AND grantee = $2`,
		serverID, normalizeGrantee(grantee))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrRemoteMCPNotFound
	}
	return nil
}

// ListRemoteMCPSharesByOwner returns every grant on the owner's servers in one
// query, keyed by server id — the list endpoint decorates each row without an
// N+1.
func (s *Store) ListRemoteMCPSharesByOwner(ctx context.Context, ownerEmail string) (map[string][]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT g.server_id, g.grantee
		FROM remote_mcp_shares g
		JOIN remote_mcp_servers m ON m.id = g.server_id
		WHERE m.user_email = $1
		ORDER BY g.grantee`,
		normalizeEmail(ownerEmail))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var id, g string
		if err := rows.Scan(&id, &g); err != nil {
			return nil, err
		}
		out[id] = append(out[id], g)
	}
	return out, rows.Err()
}

// ListRemoteMCPServersSharedWith returns servers other users shared with email
// (directly or via the everyone wildcard), newest first. UserEmail on each row
// is the OWNER — callers surface it as attribution.
func (s *Store) ListRemoteMCPServersSharedWith(ctx context.Context, email string) ([]RemoteMCPServer, error) {
	e := normalizeEmail(email)
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+remoteMCPColumns+` FROM remote_mcp_servers m
		WHERE m.user_email <> $1 AND EXISTS (
			SELECT 1 FROM remote_mcp_shares g
			WHERE g.server_id = m.id AND g.grantee IN ($1, $2))
		ORDER BY m.created_at DESC`,
		e, GranteeEveryone)
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

// GetRemoteMCPServerForUse fetches a server email may USE: their own, or one
// shared with them. The returned row keeps the OWNER's email, so the token
// methods (whose AEAD AAD binds to the owner) work unchanged — and the token
// itself never varies by who is using it.
func (s *Store) GetRemoteMCPServerForUse(ctx context.Context, email, id string) (*RemoteMCPServer, error) {
	e := normalizeEmail(email)
	row := s.db.QueryRowContext(ctx, `
		SELECT `+remoteMCPColumns+` FROM remote_mcp_servers m
		WHERE m.id = $2 AND (m.user_email = $1 OR EXISTS (
			SELECT 1 FROM remote_mcp_shares g
			WHERE g.server_id = m.id AND g.grantee IN ($1, $3)))`,
		e, id, GranteeEveryone)
	m, err := scanRemoteMCPServer(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrRemoteMCPNotFound
	}
	return m, err
}
