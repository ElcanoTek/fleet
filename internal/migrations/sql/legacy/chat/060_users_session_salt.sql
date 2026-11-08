-- A per-account salt folded into the password session epoch. The epoch was
-- sha256(password_hash) alone, so the only way to end a Fleet password session
-- was to change the password. A central (Auth) sign-out must end EVERY Fleet
-- session of the account — the signed-out page promises exactly that — so the
-- back-channel receiver now rotates this salt for the email alongside the
-- external epoch. The default '' keeps every existing epoch byte-identical:
-- sha256(hash || '') = sha256(hash), so deploying this signs nobody out.
ALTER TABLE users ADD COLUMN session_salt TEXT NOT NULL DEFAULT '';
