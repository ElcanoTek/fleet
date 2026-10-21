-- The first name a user saved for themselves (Settings → General → "Your
-- first name"). It is the ONLY source of the ${FLEET_USER_NAME} bundle token
-- (ADR-0087): fleet's sign-in paths (password, OIDC from the Elcano Auth
-- service, the elcano_auth cookie) carry an email and no name claim, and a
-- name guessed from the email is not a name. '' means "not set", and the token
-- is then unset, so a connector uses its no-name wording ("Hi team,").
--
-- The CHECK is a backstop for the 64-character limit PUT /me/name enforces
-- along with the character rule (agentcore.ValidMCPUserName). Adding a NOT
-- NULL column with a constant default is a metadata-only change in Postgres.
--
-- Numbered 081 because 079 and 080 are claimed by open PRs; the runner
-- applies any unapplied version, so the gap is harmless.
ALTER TABLE users ADD COLUMN given_name TEXT NOT NULL DEFAULT ''
  CHECK (char_length(given_name) <= 64);
