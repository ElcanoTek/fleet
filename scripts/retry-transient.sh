#!/usr/bin/env bash
# scripts/retry-transient.sh CMD [ARGS…] — run CMD, retrying it with backoff
# ONLY when it failed for a transient reason: an HTTP 5xx from a service it
# talks to (Sigstore's Rekor and Fulcio, a registry), a timeout, or a dropped
# connection. Any other failure — a refused signature, a bad flag, a 4xx —
# fails at once, with CMD's output. release.yml wraps every cosign sign,
# attest and sign-blob with it: a Rekor 502 must not fail a release whose
# artifacts are already pushed.
#
# Up to 5 attempts, waiting 15, 30, 60 and 90 seconds between them — about
# three minutes in all. Re-running a cosign sign or attest after a failure is safe:
# at worst the digest carries one more signature or attestation.
set -uo pipefail

[[ $# -ge 1 ]] || { echo "usage: $0 CMD [ARGS…]" >&2; exit 2; }
attempts=5
delays=(15 30 60 90)
transient='(status|code|HTTP)[^0-9]{0,3}5[0-9][0-9]|5[0-9][0-9] (Bad Gateway|Service Unavailable|Gateway Time-?out|Internal Server Error)|timeout|timed out|deadline exceeded|connection reset|connection refused|broken pipe|unexpected EOF|TLS handshake|no such host|temporary failure|i/o timeout'

out="$(mktemp)"
trap 'rm -f "$out"' EXIT
for ((i = 1; ; i++)); do
  if "$@" >"$out" 2>&1; then
    cat "$out"
    exit 0
  else
    rc=$?
  fi
  cat "$out" >&2
  if ! grep -Eqi "$transient" "$out"; then
    echo "retry-transient: $1 failed (exit $rc) for a reason retrying cannot fix" >&2
    exit "$rc"
  fi
  if ((i >= attempts)); then
    echo "retry-transient: $1 still failing after ${i} attempts on transient errors" >&2
    exit "$rc"
  fi
  pause="${delays[i - 1]}"
  echo "::warning::$1 failed on a transient error (attempt ${i}/${attempts}); retrying in ${pause}s" >&2
  sleep "$pause"
done
