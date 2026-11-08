-- api_key connections: some vendors want a scheme in front of the key under a
-- named header (PagerDuty's "Authorization: Token token=<key>"). Until now the
-- only way to express that was api_key_header: Authorization plus a hint
-- asking the user to type the scheme themselves. This is the PREFIX only —
-- non-secret, like api_key_header; the key itself stays sealed in api_key_enc
-- and the transport sends prefix + key per request.
ALTER TABLE remote_mcp_servers
    ADD COLUMN api_key_prefix TEXT NOT NULL DEFAULT '';
