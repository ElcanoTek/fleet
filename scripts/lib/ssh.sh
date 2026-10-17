# shellcheck shell=bash
# scripts/lib/ssh.sh — key-only SSH safety net at bootstrap.
#
# Sourced (not executed) by bootstrap.sh only: update never touches sshd.

# Vultr's cloud-init writes /etc/ssh/sshd_config.d/50-cloud-init.conf with
# `PasswordAuthentication yes`, so a fresh box takes root password guesses all
# day. The Vultr startup script "elcano-key-only-ssh" fixes that at first boot;
# this is the net for when nobody selected it. Same drop-in, same name.
SSH_KEY_ONLY_CONF="# Key-only SSH: no passwords, root by key only. Sorts before cloud-init's
# 50-cloud-init.conf so it wins (sshd keeps the first value it reads).
PasswordAuthentication no
KbdInteractiveAuthentication no
PermitRootLogin prohibit-password"

# ssh_count_authorized_keys — how many public keys sshd would accept for root
# and /home/* users, honouring the effective AuthorizedKeysFile from `sshd -T`
# (keel's deploy/harden-host.sh count_authorized_keys). The lockout guard.
# FLEET_SSH_ROOT prefixes every path (for the tests only).
ssh_count_authorized_keys() {
  local root="${FLEET_SSH_ROOT:-}" line home rel f path count=0
  local -a files=()
  line="$(sshd -T 2>/dev/null | grep -i '^authorizedkeysfile ' || true)"
  [[ -n "$line" ]] && read -ra files <<<"${line#* }"
  [[ ${#files[@]} -eq 0 ]] && files=(.ssh/authorized_keys .ssh/authorized_keys2)
  for home in "$root"/root "$root"/home/*; do
    [[ -d "$home" ]] || continue
    rel="${home#"$root"}"
    for f in "${files[@]}"; do
      [[ "$f" == none ]] && continue
      path="${f//%h/$rel}"
      path="${path//%u/${rel##*/}}"
      [[ "$path" != /* ]] && path="$rel/$path"
      path="$root$path"
      [[ -r "$path" ]] || continue
      count=$(( count + $(grep -cE '^[[:space:]]*(ssh-|ecdsa-|sk-)' "$path" || true) ))
    done
  done
  printf '%s' "$count"
}

# ssh_key_only — install /etc/ssh/sshd_config.d/01-no-password-login.conf
# (passwords off, root by key only), check it with `sshd -t`, reload sshd.
# FLEET_KEEP_SSH_PASSWORDS=1 skips it. Silent without sshd or sshd_config.d;
# a no-op when the file already has this content; skips with a warning when
# no authorized key is installed (it would lock everyone out). Honours
# DRY_RUN; never fails the caller. Uses the caller's info/ok/warn.
ssh_key_only() {
  if [[ "${FLEET_KEEP_SSH_PASSWORDS:-0}" == "1" ]]; then
    info "FLEET_KEEP_SSH_PASSWORDS=1 — leaving sshd's password settings alone"
    return 0
  fi
  local dir="${FLEET_SSH_ROOT:-}/etc/ssh/sshd_config.d"
  local conf="$dir/01-no-password-login.conf" keys tmp old=""
  command -v sshd >/dev/null 2>&1 && [[ -d "$dir" ]] || return 0
  if [[ -f "$conf" ]] && printf '%s\n' "$SSH_KEY_ONLY_CONF" | cmp -s - "$conf"; then
    return 0
  fi
  keys="$(ssh_count_authorized_keys)"
  if [[ "${keys:-0}" -lt 1 ]]; then
    warn "no SSH authorized keys found — leaving password login on so nobody is locked out; add a key, then re-run bootstrap"
    return 0
  fi
  if [[ "${DRY_RUN:-0}" == "1" ]]; then
    info "[dry-run] would write $conf (key-only SSH), check it with sshd -t and reload sshd"
    return 0
  fi
  tmp="$(mktemp)" || { warn "could not write $conf — SSH still accepts passwords"; return 0; }
  # Keep a differing hand-made copy so a failed sshd -t puts it back.
  if [[ -f "$conf" ]]; then old="$(mktemp)" && cp -p "$conf" "$old" || old=""; fi
  printf '%s\n' "$SSH_KEY_ONLY_CONF" >"$tmp"
  if ! install -m 0600 "$tmp" "$conf"; then
    rm -f "$tmp" ${old:+"$old"}
    warn "could not write $conf — SSH still accepts passwords"
    return 0
  fi
  rm -f "$tmp"
  if ! sshd -t >/dev/null 2>&1; then
    if [[ -n "$old" ]]; then mv -f "$old" "$conf"; else rm -f "$conf"; fi
    warn "sshd -t rejected the key-only drop-in — reverted $conf; sshd left as it was"
    return 0
  fi
  [[ -n "$old" ]] && rm -f "$old"
  if systemctl reload sshd >/dev/null 2>&1 || systemctl reload ssh >/dev/null 2>&1; then
    ok "SSH is key-only: passwords off, root by key ($conf)"
  else
    warn "wrote $conf but could not reload sshd — run: systemctl reload sshd"
  fi
  if sshd -T 2>/dev/null | grep -qiE '^(passwordauthentication|permitrootlogin) yes$'; then
    warn "sshd still accepts passwords: an earlier directive wins (e.g. 'PermitRootLogin yes' above the Include line in /etc/ssh/sshd_config) — fix it by hand"
  fi
  return 0
}
