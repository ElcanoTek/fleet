# ADR-0063: Remove the Rampart PII engine, its installer and the `scripts/rampart-service` npm tree

- **Status:** Accepted
- **Date:** 2026-09-09
- **Deciders:** fleet maintainers
- **Amends:** [ADR-0028](0028-optional-pii-redaction.md) (the external
  ONNX/HTTP classifier it left "interface-ready", and which later shipped, is
  removed again) and [ADR-0036](0036-sandboxed-file-tools-and-host-io-exceptions.md)
  (the admin-triggered host `podman` build/run exception it enumerated for
  `internal/rampartinstall` no longer exists).

## Context

ADR-0028 added optional PII redaction over tool output with a deterministic,
dependency-free pattern engine and a provider-neutral `Redactor` interface. A
follow-on then shipped a second engine behind that interface: **Rampart**, a
MiniLM token-classification ONNX model running out of process behind a small
HTTP service. To make it usable it grew a lot of surface area:

- `scripts/rampart-service/` — a reference Node service over
  `@nationaldesignstudio/rampart` and `@huggingface/transformers`, with its own
  `package.json`, lockfile and `node:24-slim` Containerfile, **embedded into the
  fleet binary** (`embed.go`) so fleet could build the image itself;
- `internal/rampartinstall/` — a one-click installer that shelled out to host
  `podman` to build, run, health-check and supervise that container, restart it
  after reboots, and write the resulting URL into the settings store;
- two admin settings (`pii_redaction_engine`, `pii_rampart_url`), two env vars,
  two admin endpoints (`/admin/pii-redaction/test`, `/admin/pii-redaction/install`),
  their web proxies, and an "Install Rampart service" block in the Features panel;
- `internal/piiredact/rampart.go` — the HTTP client, with pattern-engine
  fallback and rate-limited degradation logging;
- a second `npm audit` step in CI, two npm `overrides` (`sharp`, `adm-zip`)
  force-patching transitive dependencies the locked parents did not accept, and
  `scripts/check-npm-overrides.sh`, a canary watching for the day those
  overrides became droppable; a Dependabot npm entry and a Dependabot docker
  entry; and their share of every version-agreement test and node-LTS checklist.

Two facts decided this:

1. **Nobody runs it.** The repository owner confirmed on 2026-09-09 that no
   deployment uses the Rampart engine. Everything above was carried for a
   feature with zero users.
2. **Its dependency tree cannot be made clean.** The `npm audit` gate is
   deliberately "any severity, any finding" (`docs/SCANNING.md`). On
   2026-09-08, GHSA-vwc7-r8mq-g2x9 was published against `adm-zip`
   `>=0.5.9 <=0.6.0` — **every release that exists**, including the `^0.6.0`
   the override already forced, with no patched version and only an open
   upstream PR. `adm-zip` reaches fleet solely through
   `@huggingface/transformers → onnxruntime-node → adm-zip`, i.e. solely
   through the rampart service. From that moment every open PR was red on the
   `web` job (#1465, #1466, #1467), and the only ways out were (a) an
   accepted-advisory register that weakens the npm gate for the whole repo,
   (b) a downgrade to `@huggingface/transformers` 3.x that reintroduces
   GHSA-xcpc-8h2w-3j85, or (c) removing the tree.

An unused feature that holds the CI gate hostage to a third party's release
schedule is not worth an exception mechanism. Option (c) it is.

## Decision

**Remove Rampart entirely.** Deleted in one PR:

- `scripts/rampart-service/` (service, Containerfile, `install.sh`, README,
  `embed.go`, `package.json`, `package-lock.json`) and
  `scripts/check-npm-overrides.sh`;
- `internal/rampartinstall/`;
- `internal/piiredact/rampart.go` and its tests;
- `internal/httpapi/admin_pii_install.go`, `admin_pii_probe.go` and their tests,
  the `/admin/pii-redaction/*` routes, and the web proxies under
  `web/src/app/api/admin/pii-redaction/`;
- the `pii_redaction_engine` and `pii_rampart_url` settings, the
  `Config.PIIRedactionEngine` / `Config.PIIRampartURL` fields, the engine/URL
  hooks in `cmd/fleet/workspace_settings.go` (the PII state is now mode-only),
  and the Rampart rows and action block in the Features panel;
- the rampart `npm audit` step and the override canary in `ci.yml`, the
  matching `make ci-web` lines, the Dependabot npm entry and docker directory,
  the node-LTS checklist items, and the rampart cases in
  `scripts/check_versions_test.go` / `check_release_version_test.go`.

**What stays.** PII redaction itself (ADR-0028) is untouched: the
`pii_redaction_mode` setting, `FLEET_PII_REDACTION_ENABLED` /
`FLEET_PII_REDACTION_MODE`, the tool-output choke point, the deterministic
`PatternRedactor`, and the provider-neutral `Redactor` interface (an external
classifier *could* still implement it; none is shipped, and the docs say so).
The `/admin/guardrail/test` probe and the guardrail settings are unrelated and
stay.

**Compatibility for deployments that had Rampart configured.**

- *Settings rows.* `pii_redaction_engine` and `pii_rampart_url` are no longer in
  the registry. `internal/settings` resolves only registered keys, so a
  persisted row under either key is simply never read again: the deployment
  keeps its saved `pii_redaction_mode` and runs the pattern engine. No
  migration deletes the rows; they are inert. (Deleting the setting from the
  registry rather than leaving a one-value enum was deliberate — a "choice"
  with one option is a lie in the admin panel.)
- *Env vars.* `FLEET_PII_REDACTION_ENGINE` and `FLEET_PII_RAMPART_URL` remain
  in `allowedEnvVars` so an env-file value still loads, and `config.Load`
  prints one boot warning naming the stale key. Nothing else reads them.
- *A managed container.* An operator who used the one-click installer has a
  `podman` container named by the old installer still running on their box.
  fleet no longer knows about it; `podman rm -f` it by hand. This ADR is the
  only place that says so, on purpose: with zero known users a runbook would
  be ceremony.

## Consequences

- The `web` CI job audits **one** npm tree and is green again on every open PR.
  The npm gate keeps its "any finding blocks" threshold; no accepted-advisory
  register was added, and `docs/SCANNING.md` now records the rampart tree as
  the cautionary tale behind that threshold.
- ~1,800 lines of Go, TypeScript and shell leave the repo, along with an
  embedded container build context and the host-side `podman` exception
  ADR-0036 had to enumerate after the fact. The invariant set only narrows.
- `docs/PII-REDACTION.md` describes one engine honestly instead of two, and the
  Features panel shows the PII mode alone.
- If an ML PII engine is wanted again, it comes back as **a bundle-side
  service fleet talks to over HTTP**, behind the existing `Redactor` interface,
  with fleet shipping neither the service, its dependency tree, nor an
  installer — the same engine-vs-bundle boundary `AGENTS.md` draws for every
  other customer-specific component.
