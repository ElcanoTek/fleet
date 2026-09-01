# scripts/lib/caddyfile.sh — the ONE renderer of the fleet-managed Caddyfile.
#
# Sourced (not executed) by bootstrap.sh, update.sh and doctor.sh. It exists
# because the TLS front's layout used to live in three places — deploy/Caddyfile
# (the annotated reference), a printf in bootstrap.sh, and the operators' heads
# — and they drifted: the printf only knew the web tier, so every documented
# public API URL (/v1/…, /api-info, the A2A agent card, /triggers/…, /webhooks/…)
# reached Next.js instead of the Go backend and 404'd there. One renderer here,
# with deploy/Caddyfile kept byte-for-byte in lockstep by a test
# (internal/admincli TestCaddyfileRendererMatchesDeployReference), means a
# routing fix ships to every path at once and doctor/update can DETECT a box
# whose installed Caddyfile predates it.
#
# Contract (see docs/DEPLOYMENT.md "TLS" + ADR-0053):
#   * Everything defaults to the Next.js web tier (:3000) — the browser's only
#     entrypoint, unchanged.
#   * The public HTTP API — the orchestrator's versioned /v1 surface plus the
#     unversioned-forever paths it fixes (/api-info, the A2A agent card, /a2a,
#     the /triggers/… inbound task triggers) — goes to the orchestrator
#     listener with the Next-proxy header-trust channel STRIPPED
#     (X-Orchestrator-Server-Token / X-User-Email / X-User-Session-Epoch), so
#     the impersonation path stays reachable from loopback only even though the
#     API is now reachable from the internet. API-key (X-API-Key) auth is the
#     public contract; nothing else is.
#   * The chat listener's signed inbound webhooks (/webhooks/…, GitHub/Slack
#     HMAC-authenticated) go to the chat listener, likewise stripped.
#
# Every function is pure shell (no caddy binary needed) so doctor's --check and
# the Go tests can run it anywhere.

# shellcheck shell=bash

# The marker bootstrap has always written as line 1. An existing Caddyfile
# WITHOUT it belongs to someone else (a legacy chat/moc install, a hand-written
# config) and is never overwritten without --force-caddy. Keep the text stable:
# it is how every already-provisioned box is recognised as fleet-managed.
CADDY_MARKER="# Managed by fleet (scripts/bootstrap.sh) — re-runs overwrite this file."

# The upstream defaults mirror the Go process (config.Load defaults Addr to
# 127.0.0.1:8080; orchestratorAddr() to 127.0.0.1:8000) and fleet-web.env's
# PORT=3000. Callers pass the env-file values when they differ.
CADDY_DEFAULT_WEB_ADDR="127.0.0.1:3000"
CADDY_DEFAULT_CHAT_ADDR="127.0.0.1:8080"
CADDY_DEFAULT_ORCH_ADDR="127.0.0.1:8000"

# caddyfile_is_managed [FILE] — true when FILE carries the fleet marker.
caddyfile_is_managed() {
  local f="${1:-/etc/caddy/Caddyfile}"
  [[ -s "$f" ]] && grep -qF "$CADDY_MARKER" "$f"
}

# caddyfile_is_foreign [FILE] — true when FILE exists, is non-empty and was NOT
# written by fleet (no marker). The bootstrap refusal keys on this.
caddyfile_is_foreign() {
  local f="${1:-/etc/caddy/Caddyfile}"
  [[ -s "$f" ]] && ! grep -qF "$CADDY_MARKER" "$f"
}

# caddyfile_domain [FILE] — the site address of the fleet-managed block: the
# first top-level "<host> {" line that is not the bare global-options "{".
# Empty when none (a truncated or hand-emptied file).
caddyfile_domain() {
  local f="${1:-/etc/caddy/Caddyfile}"
  [[ -r "$f" ]] || return 0
  grep -E '^[^[:space:]#{][^[:space:]]*[[:space:]]*\{[[:space:]]*$' "$f" 2>/dev/null \
    | head -n1 | sed -E 's/[[:space:]]*\{[[:space:]]*$//' || true
}

# caddyfile_acme_email [FILE] — the `email` global option bootstrap writes from
# FLEET_ACME_EMAIL, or empty. Read back so a re-render keeps the operator's
# Let's Encrypt contact rather than silently dropping it.
caddyfile_acme_email() {
  local f="${1:-/etc/caddy/Caddyfile}"
  [[ -r "$f" ]] || return 0
  grep -E '^[[:space:]]*email[[:space:]]+' "$f" 2>/dev/null | head -n1 | awk '{print $2}' || true
}

# caddyfile_functional_body [FILE] — the file (or stdin when FILE is "-" or
# omitted) with comments and blank lines stripped, so a reworded header is
# never mistaken for drift. Same rule update.sh/doctor.sh apply to unit files.
caddyfile_functional_body() {
  local f="${1:--}"
  if [[ "$f" == "-" ]]; then
    grep -vE '^[[:space:]]*(#|$)' || true
  else
    grep -vE '^[[:space:]]*(#|$)' "$f" 2>/dev/null || true
  fi
}

# caddyfile_routes_api [FILE] — a cheap structural probe for the one regression
# this library was written to end: does FILE send the API to the orchestrator
# at all? True when a /v1 matcher and an orchestrator-port upstream are both
# present. Used for the ADVISORY on a foreign (operator-owned) Caddyfile, where
# a functional diff against our layout would be meaningless.
caddyfile_routes_api() {
  local f="${1:-/etc/caddy/Caddyfile}" orch="${2:-$CADDY_DEFAULT_ORCH_ADDR}"
  [[ -r "$f" ]] || return 1
  grep -qE '(^|[[:space:]])/v1(/\*)?([[:space:]]|$)' "$f" && grep -qF "$orch" "$f"
}

# render_fleet_caddyfile DOMAIN [ACME_EMAIL] [CHAT_ADDR] [ORCH_ADDR] [WEB_ADDR]
# Print the complete fleet-managed Caddyfile (marker first) to stdout. Keep the
# body in lockstep with deploy/Caddyfile — the test compares their functional
# bodies, so a change here without the matching reference edit fails CI.
render_fleet_caddyfile() {
  local domain="$1" email="${2:-}"
  local chat="${3:-$CADDY_DEFAULT_CHAT_ADDR}" orch="${4:-$CADDY_DEFAULT_ORCH_ADDR}" web="${5:-$CADDY_DEFAULT_WEB_ADDR}"
  [[ -n "$domain" ]] || { echo "render_fleet_caddyfile: DOMAIN is required" >&2; return 1; }
  chat="${chat:-$CADDY_DEFAULT_CHAT_ADDR}"; orch="${orch:-$CADDY_DEFAULT_ORCH_ADDR}"; web="${web:-$CADDY_DEFAULT_WEB_ADDR}"

  printf '%s\n' "$CADDY_MARKER"
  if [[ -n "$email" ]]; then
    printf '{\n\temail %s\n}\n\n' "$email"
  fi
  cat <<EOF
${domain} {
	encode zstd gzip

	# Security headers for the Next-served pages, mirroring exactly what the
	# Go backends already set on their own responses (cmd/fleet/tls.go
	# securityHeadersMiddleware) so the whole origin carries one policy.
	# \`header\` replaces existing values, so proxied API responses that already
	# carry these are not duplicated.
	header {
		Strict-Transport-Security "max-age=63072000; includeSubDomains"
		X-Content-Type-Options "nosniff"
		X-Frame-Options "DENY"
	}

	# The public HTTP API → the orchestrator listener. /v1 is the versioned
	# surface (docs/api-versioning.md, docs/openapi.yaml); the bare paths are
	# the ones fleet fixes as unversioned-forever: version discovery, the A2A
	# agent card + JSON-RPC endpoint, and the inbound task triggers (webhook +
	# email). Nothing else on the orchestrator is exposed — its legacy bare
	# paths (/tasks, /keys, …) stay loopback-only behind Next's /api/* proxy.
	#
	# The header_up deletions are load-bearing: the orchestrator trusts
	# X-User-Email when X-Orchestrator-Server-Token matches (the Next-proxy
	# impersonation channel, #157). Stripping them here means that channel is
	# reachable from loopback ONLY, so exposing the API adds exactly one
	# public auth path — X-API-Key — and no header-trust surface (ADR-0053).
	@fleet_api path /v1 /v1/* /api-info /.well-known/agent-card.json /a2a /a2a/* /triggers/*
	handle @fleet_api {
		reverse_proxy ${orch} {
			header_up -X-Orchestrator-Server-Token
			header_up -X-User-Email
			header_up -X-User-Session-Epoch
			# SSE task streams (/v1/tasks/{id}/stream, A2A streaming): no
			# response buffering, generous read timeout.
			flush_interval -1
			transport http {
				read_timeout 30m
			}
		}
	}

	# Signed inbound chat webhooks (docs/WEBHOOKS.md: GitHub/Slack HMAC) →
	# the chat listener. Same stripping rule for chat's header-trust channel.
	@fleet_chat_webhooks path /webhooks/*
	handle @fleet_chat_webhooks {
		reverse_proxy ${chat} {
			header_up -X-Chat-Server-Token
			header_up -X-User-Email
			header_up -X-User-Session-Epoch
		}
	}

	# Everything else → the Next.js web app: pages, /_next/*, and its own
	# /api/* route handlers (which proxy to the backends server-side).
	handle {
		reverse_proxy ${web} {
			# SSE: stream tokens straight through, no response buffering, and a
			# generous read timeout so long agent turns don't get cut off.
			flush_interval -1
			transport http {
				read_timeout 30m
			}
		}
	}
}
EOF
}
