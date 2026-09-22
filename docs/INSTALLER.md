# The public one-line installer (`install.sh`)

## What shipped

`install.sh` at the repo root is the public entry point for a single-box install:

```sh
curl -fsSL https://raw.githubusercontent.com/ElcanoTek/fleet/main/install.sh | sudo bash
```

It installs git if it's missing, clones `main` into `/opt/fleet/src` (`FLEET_SRC_DIR`
overrides it), `cd`s into the checkout, and `exec`s `scripts/bootstrap.sh`. Everything
the box ends up with — Postgres, the build, systemd units, the web tier, TLS — is
bootstrap's work; the installer only gets a checkout in place and hands over.

- **Interactive vs unattended.** With no arguments, the installer reattaches
  `/dev/tty` (the `curl | bash` pipe owns stdin) so bootstrap's prompts reach the
  operator. With arguments (`| sudo bash -s -- --postgres=local …`), it leaves stdin
  alone, so bootstrap sees no terminal and runs on its flags and defaults without
  prompting.
- **`--dry-run` changes nothing on the host.** The installer doesn't install git or
  touch `$FLEET_SRC_DIR`. It prints its own plan, then runs bootstrap's dry run from the
  kept-as-is checkout (dirty or not on `main`, which a real run would also use unchanged),
  or otherwise from a throwaway shallow clone of the current remote `main`. That's what
  a real run would clone or fast-forward to. The temp dir is removed afterwards.
  If git itself is missing, it says so and stops.
- **Reruns.** A clean checkout on `main` (no modified *or untracked* files) is
  fast-forwarded, and bootstrap runs again; it's idempotent. A checkout that's dirty or on
  another branch is left alone with a pointer to `sudo fleet update`. A path that
  exists but isn't a git checkout is refused.
- **Argument handling.** The installer parses flags the way bootstrap does (value-taking
  flags consume the next word), so `--client-config --dry-run` is a bad client-config
  value, not a dry run. A relative local `--client-config` path, or an `--auth-pubkey @file`,
  is made absolute against the caller's directory before the installer `cd`s into the
  checkout. A trailing `/` on `FLEET_SRC_DIR` is stripped.
- **Interrupted clones.** The first clone goes to `$src.partial.<pid>` and is renamed into
  place only on success, so an aborted download never leaves a half-populated
  checkout that blocks the next run.
- **Truncated downloads.** The body is functions only, and the final call is
  `{ main "$@"; }`. A download cut off anywhere before the closing brace is a syntax
  error: an unterminated function, or an unterminated group. A bare trailing `main`
  would still run with no arguments, so the call is wrapped.

## Deviations and known limits

- **`curl | bash` can't report a failed download.** If `curl` fails before sending any
  bytes, `bash` reads empty input and exits 0. The README and DEPLOYMENT.md tell
  automation to download to a file first (`curl -fsSLo … && sudo bash /tmp/fleet-install.sh …`).
  The script can't fix this itself, because it never runs in that case.
- **Private client bundles** are cloned by bootstrap running as root, so git
  credentials must be cached for root (`sudo git config --global credential.helper store`
  plus one `sudo git clone`). A credential helper configured for the invoking user isn't
  used. **Don't put a token in the `--client-config` URL**: bootstrap echoes the argument, and
  git stores the URL, token included, in the bundle checkout's `.git/config`.
- **Unattended means "whatever the flags leave unset takes bootstrap's default."** A
  flagged run started from a real terminal with `sudo bash /tmp/fleet-install.sh
  FLAGS` (not a pipe) still has a TTY, so bootstrap asks for anything the flags don't cover.
  The documented automation form ends in `</dev/null` for this reason.
- **Fedora/RHEL with dnf only**, the same scope as bootstrap.

## Deliberately deferred

- No checksum or signature check on the downloaded script. The trust root is the TLS
  fetch from `raw.githubusercontent.com` of a named branch, the same as `git clone`.
- No pinned-release install (`FLEET_REF=<tag>`). `main` is the release line (ADR-0062);
  bootstrap's `--client-config <url>#<ref>` pins the bundle, not fleet.
