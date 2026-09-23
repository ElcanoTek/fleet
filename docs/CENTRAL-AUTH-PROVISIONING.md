# Central Auth membership provisioning

## Shipped behavior

Fleet consumes Auth's signed, versioned application-access desired state at
the existing public back-channel endpoint. Granting Fleet in Auth creates or
enables the matching Fleet Chat account and applies the selected Chat role;
it also creates, enables, disables, or re-roles the matching Operations Center
identity. Revoking Fleet disables both planes and ends current sessions without
deleting either identity, its roles, conversations, tasks, or ownership.

Auth's Fleet permission payload is deliberately small and strictly validated:

| Auth control | Fleet value |
| --- | --- |
| Chat Viewer | `viewer` |
| Chat Contributor (default) | `member` |
| Ops None (default) | disabled Ops identity |
| Ops Viewer | `readonly` |
| Ops Contributor | `client` |
| Fleet Admin | Chat `admin` and Ops `admin` |

Fleet Admin is one unified grant. In Settings → Admin → Users, selecting it
shows Chat Contributor and Ops Contributor as the effective highlighted
choices. Selecting the already-selected Fleet Admin control turns it off and
defaults to Chat Contributor plus Ops None; either narrower role can then be
changed before saving.

## Delivery and ordering

Auth posts exactly one `access_token` form field to
`/api/auth/backchannel-logout`. The Next.js tier verifies Ed25519 signature,
key ID, issuer, audience, lifetime, event namespace, normalized identity,
action, version, and bounded string settings before forwarding the event over
Fleet's shared-token loopback channel.

The Chat store atomically records the last accepted version per issuer and
subject. The same or an older version is ignored. The Operations Center uses a
separate database, so the handler re-reads durable desired state after each Ops
write and reconciles a concurrently committed newer version before it
acknowledges the request. A retry of the same version still reconciles Ops if a
previous cross-database write failed.

Deploy Fleet's receiver before the Auth sender. Auth retains and retries its
latest desired state until Fleet returns success, so an offline receiver
converges after recovery.

## Security and data retention

- The public endpoint accepts exactly one signed logout or access token and is
  body-size bounded. It does not trust roles sent by a browser.
- Chat revocation rotates both password and external-session epochs.
- Ops revocation clears its scheduler session token. Re-grant therefore cannot
  revive a pre-revocation session.
- Ops disable preserves the scheduler UUID so task ownership and history remain
  attached to the same identity.
- A newly provisioned central-only Chat account has an intentionally invalid
  password digest; a new Ops identity has a random unusable password hash.
- Admin must be coherent across both planes. Split `admin` payloads are
  rejected, and unknown settings or roles fail closed.

## Deliberate scope

Auth controls whether its account may enter Fleet and transports Fleet's
chosen roles. Fleet remains the enforcement and data owner. Existing Fleet
admin APIs and operator commands remain available, but a later Auth desired
state for that identity is authoritative for enabled state and the two roles.
This change does not delete dormant accounts or migrate application data into
Auth.
