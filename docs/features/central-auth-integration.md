# Central Auth integration

Fleet keeps all three login paths independent:

- Fleet email/password remains available and is governed only by Fleet's user
  database and password-derived session epoch.
- The legacy `elcano_auth` magic-link cookie remains supported when
  `AUTH_SIGNING_PUBKEY` is configured.
- A central password Auth deployment can be configured through Fleet's existing
  OIDC Authorization Code + PKCE client.

For the central Auth service, set:

```text
FLEET_OIDC_ISSUER=https://auth.example.com
FLEET_OIDC_CLIENT_ID=fleet-client-id
FLEET_OIDC_CLIENT_SECRET=<one-time secret from auth app create>
FLEET_OIDC_SCOPES=openid email
AUTH_SIGNING_PUBKEY=<Auth Ed25519 public key>
```

Register the exact callback and signed logout endpoint on Auth:

```text
https://fleet.example.com/api/auth/oidc/callback
https://fleet.example.com/api/auth/backchannel-logout
```

Fleet sends client credentials with `client_secret_basic` when discovery
advertises it, as Auth does. Providers that do not advertise that method keep
the existing `client_secret_post` fallback.

Central sessions carry a separate Postgres-backed epoch keyed by OIDC issuer
and subject. A signed back-channel logout rotates only that epoch. Tokens must
carry `exp` (Auth signs a fresh `iat`/`exp` per delivery attempt). Duplicate
events are idempotent by JWT `jti`; Fleet-native password sessions and the
legacy magic-link route are not revoked or disabled. Every Fleet data request
for an OIDC session forwards the signed cookie's issuer, subject, and epoch to
both Go planes, which compare it with the live chat-store generation.

The back-channel endpoint is intentionally public at the browser proxy: it is a
server-to-server endpoint authenticated by Auth's Ed25519 signature, exact
issuer/audience, and event shape. Invalid tokens return 400; temporary backend
failures return 503 so Auth's durable delivery worker retries.
