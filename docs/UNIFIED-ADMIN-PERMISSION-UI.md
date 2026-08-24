# Unified admin permission in the Users UI

## Shipped behavior

Settings → Admin → Users presents three permission sections when editing an
account:

- **Admin** — one Admin option that grants admin on both planes.
- **Chat** — Viewer or Contributor.
- **Ops Center** — None, Viewer, or Contributor.

Admin appears first so the broadest, cross-plane grant is visually distinct
before the two narrower per-plane role selectors.

The Add user form uses the same permission sections and Team field as the edit
popover, so an account can receive its complete Chat, Ops Center, and team
assignment when it is created.

Every role option has a hover description that explains its effective access.
The descriptions are shared by the edit and account-creation controls.

The Admin control sets both `role: "admin"` and `ops_role: "admin"` in the
pending edit; the PATCH sends whichever fields changed. This is a UI expression
of the existing server contract: promoting a chat account to admin also ensures
the matching scheduler account is an admin. A unified admin has one Admin badge
in the user table rather than a second `ops: admin` badge.

Choosing a narrower Chat or Ops Center permission while editing a unified admin
leaves unified-admin mode. The other plane falls back to its least-privileged
state (Chat Contributor or no Ops access), after which either narrower role can
be selected explicitly before saving.

## Deliberate scope

The HTTP API accepts `ops_role` on both create and update, including
`ops_role: "admin"` independently for backward compatibility and operator
automation. An existing ops-only admin is displayed accurately, but the Users
UI no longer offers two separate ways to grant admin. No database schema or
authorization middleware changed.
