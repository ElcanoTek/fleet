#!/usr/bin/env bash
# Sourced by the live harness; ownership comes from a unique workspace directory,
# not the shared chat-sandbox name prefix or a before/after container snapshot.

e2e_create_workspace() {
  local parent="$1" workspace
  mkdir -p "$parent" || return 1
  workspace="$(mktemp -d "$parent/run.XXXXXXXX")" || return 1
  (cd "$workspace" && pwd -P)
}

e2e_cleanup_sandboxes() {
  local workspace="$1" container source
  # Fail closed if called without the exact generated run directory.
  [[ "$workspace" == /*/run.* && -d "$workspace" ]] || return 1
  command -v podman >/dev/null 2>&1 || return 0
  while IFS= read -r container; do
    [[ -n "$container" ]] || continue
    while IFS= read -r source; do
      if [[ "$source" == "$workspace/"* ]]; then
        podman rm -f "$container" >/dev/null 2>&1 || true
        break
      fi
    done < <(podman inspect --format '{{range .Mounts}}{{println .Source}}{{end}}' "$container" 2>/dev/null)
  done < <(podman ps -aq --filter 'label=fleet.instance' 2>/dev/null)
}
