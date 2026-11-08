#!/usr/bin/env bash
# Copyright (c) 2025 ElcanoTek
# SPDX-License-Identifier: MIT
#
# install-pinned-tool.sh — install a CI tool from its pinned, checksum-verified
# GitHub release asset, surviving a GitHub release-asset outage.
#
#   scripts/install-pinned-tool.sh --url URL --sha256 SHA --cache-dir DIR
#       [--member PATH] [--go-install PKG@vVERSION] [--dest DIR] NAME
#
# GitHub's release-asset CDN has returned HTTP 504 on every attempt of
# `curl --retry 5` for minutes at a time, failing the Helm, Grype and Go jobs
# before they ran anything (run 42464708985). So, in order:
#
#   1. CACHE. DIR is an actions/cache path keyed by tool + version + SHA-256 +
#      OS/arch. A cached asset is used only if it still hashes to SHA, so a
#      hit never touches the network and a stale or tampered entry is simply
#      re-fetched.
#   2. DOWNLOAD the asset from URL (curl's own bounded retry, as every workflow
#      curl) and require SHA. A MISMATCH IS FATAL: it means a re-tag or
#      tampering, never an outage, and never falls through to step 3. Only a
#      verified asset is written to DIR, so only it is ever cached.
#   3. FALLBACK, only when the download itself failed and --go-install names
#      the tool's Go package at the SAME pinned version: `go install
#      PKG@vVERSION` through the Go module proxy (proxy.golang.org, not GitHub
#      releases), verified by go.sum / the checksum database (sum.golang.org).
#      The pinned version is unchanged; the binary is built from that
#      version's source instead of being the release's prebuilt bytes, so its
#      self-reported version may read "development"/"[not provided]". Nothing
#      built this way is cached.
#
# --member is the binary's path inside a .tar.gz asset; without it the asset
# IS the binary. NAME is installed (mode 0755) into --dest, /usr/local/bin by
# default (through sudo when not writable).
set -euo pipefail

url="" sha="" cache_dir="" member="" go_pkg="" dest="/usr/local/bin"

usage() {
  echo "usage: $0 --url URL --sha256 SHA --cache-dir DIR [--member PATH] [--go-install PKG@vVERSION] [--dest DIR] NAME" >&2
  exit 2
}

while [ $# -gt 0 ]; do
  case "$1" in
    --url)        [ $# -ge 2 ] || usage; url="$2"; shift 2 ;;
    --sha256)     [ $# -ge 2 ] || usage; sha="$2"; shift 2 ;;
    --cache-dir)  [ $# -ge 2 ] || usage; cache_dir="$2"; shift 2 ;;
    --member)     [ $# -ge 2 ] || usage; member="$2"; shift 2 ;;
    --go-install) [ $# -ge 2 ] || usage; go_pkg="$2"; shift 2 ;;
    --dest)       [ $# -ge 2 ] || usage; dest="$2"; shift 2 ;;
    --) shift; break ;;
    -*) usage ;;
    *) break ;;
  esac
done
[ $# -eq 1 ] || usage
name="$1"
[ -n "$url" ] && [ -n "$cache_dir" ] || usage
[[ "$sha" =~ ^[0-9a-f]{64}$ ]] || { echo "$0: --sha256 must be 64 lowercase hex digits" >&2; exit 2; }
[[ "$name" =~ ^[A-Za-z0-9._-]+$ ]] || { echo "$0: NAME must be a plain file name" >&2; exit 2; }
if [ -n "$go_pkg" ] && ! [[ "$go_pkg" =~ ^[A-Za-z0-9._/-]+@v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo "$0: --go-install must be PKG@vX.Y.Z (a pinned version, never latest)" >&2
  exit 2
fi

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

hash_ok() { [ "$(sha256sum "$1" | awk '{print $1}')" = "$sha" ]; }

place() { # BINARY — install it as $dest/$name
  if [ -w "$dest" ]; then
    install -m 0755 "$1" "$dest/$name"
  else
    sudo install -m 0755 "$1" "$dest/$name"
  fi
}

asset="$cache_dir/$(basename "$url")"
if [ -f "$asset" ] && hash_ok "$asset"; then
  echo "$name: cached asset $(basename "$url") verified (sha256 ${sha:0:12}…); no download"
else
  if [ -e "$asset" ]; then
    echo "::warning::$name: cached $(basename "$url") does not hash to the pin; discarding it and downloading"
    rm -f "$asset"
  fi
  if curl -fsSL --retry 5 --retry-delay 2 --retry-all-errors --connect-timeout 20 -o "$work/asset" "$url"; then
    if ! hash_ok "$work/asset"; then
      echo "::error::$name: $(basename "$url") hashes to $(sha256sum "$work/asset" | awk '{print $1}'), the pin says $sha — refusing it (a re-tag or tampering, not an outage)"
      exit 1
    fi
    mkdir -p "$cache_dir"
    mv "$work/asset" "$asset"
    echo "$name: downloaded $(basename "$url"), sha256 verified"
  else
    rc=$?
    # Leave no empty cache directory behind: actions/cache would save it under
    # this key and every later run would "hit" nothing.
    rmdir "$cache_dir" 2>/dev/null || true
    if [ -z "$go_pkg" ]; then
      echo "::error::$name: could not download $url (curl exit $rc) and there is no fallback source for it"
      exit "$rc"
    fi
    if ! command -v go >/dev/null 2>&1; then
      echo "::error::$name: could not download $url (curl exit $rc), and no Go toolchain is on PATH for the go install fallback"
      exit "$rc"
    fi
    echo "::warning::$name: the GitHub release asset is unreachable (curl exit $rc); building the same pinned version with go install $go_pkg through the Go module proxy (verified by sum.golang.org)"
    mkdir -p "$work/bin"
    # The module proxy only (no `direct`, so no fetch from GitHub itself), and
    # the checksum database forced on: nothing in the job's environment may
    # exempt these modules from sum.golang.org.
    GOBIN="$work/bin" GOFLAGS='' GOWORK=off GOPROXY=https://proxy.golang.org \
      GOSUMDB=sum.golang.org GOPRIVATE='' GONOSUMDB='' GONOPROXY='' GOINSECURE='' \
      "$here/retry.sh" --what "go install $go_pkg" -- go install "$go_pkg"
    [ -x "$work/bin/$name" ] || { echo "::error::$name: go install $go_pkg produced no $name binary"; exit 1; }
    place "$work/bin/$name"
    exit 0
  fi
fi

if [ -n "$member" ]; then
  tar -xzf "$asset" -C "$work" "$member"
  place "$work/$member"
else
  place "$asset"
fi
