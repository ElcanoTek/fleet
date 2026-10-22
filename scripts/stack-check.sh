#!/usr/bin/env bash
# stack-check.sh — enforce the Elcano frontend standard (see the
# "Elcano frontend standard" block in AGENTS.md). Shared verbatim by every
# Elcano repo; change it in all of them at once (bump STACK_CHECK_VERSION).
#
# Reads stack.conf at the repo root:
#   tier=A|B                 A: Go templates + plain JS; B: React/Vite SPA embedded in Go
#   web=<dir> [<dir> ...]    directories holding served HTML/CSS/JS (scanned for CDN use)
#   npm=<dir> [<dir> ...]    directories holding a package.json ("." for the root)
#   backend=go|<other>       anything but go must be explained in AGENTS.md
#   dev_allow=<pkg> [...]    Tier A: extra devDependencies beyond browser-test tooling
#
# Checks:
#   1. AGENTS.md carries the shared standard block, byte-identical (hash pinned below).
#   2. flag.lock is honest (scripts/flag-sync.sh check).
#   3. No served file loads a script, stylesheet or font from another host.
#   4. npm trees: no server frameworks (next, express, ...); Tier A has no runtime deps.
set -euo pipefail

STACK_CHECK_VERSION=15
STANDARD_SHA=5b3a1c9dab098277570a37d58366b0802d4378370f9717d48743c386b194bdbb
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CONF="$ROOT/stack.conf"
fail=0
err() { echo "stack-check: $*" >&2; fail=1; }

sha() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum | cut -d' ' -f1
  else shasum -a 256 | cut -d' ' -f1; fi
}
conf() { sed -n "s/^$1=//p" "$CONF" | sed 's/[[:space:]]*#.*$//' | tail -1; }

[ -f "$CONF" ] || { echo "stack-check: missing stack.conf" >&2; exit 2; }
command -v perl >/dev/null 2>&1 || { echo "stack-check: perl is required (core JSON::PP)" >&2; exit 2; }
tier="$(conf tier)"; web="$(conf web)"; npm="$(conf npm)"; backend="$(conf backend)"
case "$tier" in A|B) ;; *) err "stack.conf: tier must be A or B (got '$tier')";; esac
# An empty scan scope would silently disable the no-CDN gate.
[ -n "$(printf '%s' "$web" | tr -d '[:space:]')" ] || err "stack.conf: web= must list at least one served directory"

# 1. The standard block.
if [ ! -f "$ROOT/AGENTS.md" ]; then err "missing AGENTS.md"
else
  got="$(awk '/^<!-- BEGIN:elcano-frontend-standard/{f=1;next}/^<!-- END:elcano-frontend-standard/{f=0}f' "$ROOT/AGENTS.md" | sha)"
  [ "$got" = "$STANDARD_SHA" ] || err "AGENTS.md: the elcano-frontend-standard block is missing or edited (sha $got). It is shared verbatim across repos; change it everywhere and bump STANDARD_SHA in every scripts/stack-check.sh."
  # A non-Go backend needs its own AGENTS.md heading naming the exception and
  # the backend (the shared block mentions exceptions, so it does not count).
  if [ "${backend:-go}" != go ] && ! awk '/^<!-- BEGIN:elcano-frontend-standard/{f=1} !f{print} /^<!-- END:elcano-frontend-standard/{f=0}' "$ROOT/AGENTS.md" \
       | grep -iE '^#{1,6} .*exception' | grep -qi "$backend"; then
    err "stack.conf: backend=$backend needs an AGENTS.md heading like '## Exception: $backend backend' explaining why"
  fi
fi

# dev_allow= names are exceptions to Tier A: each must be explained in AGENTS.md.
for pkg in $(conf dev_allow); do
  if ! awk '/^<!-- BEGIN:elcano-frontend-standard/{f=1} !f{print} /^<!-- END:elcano-frontend-standard/{f=0}' "$ROOT/AGENTS.md" 2>/dev/null \
       | grep -qF -- "$pkg"; then
    err "stack.conf: dev_allow=$pkg must be explained in AGENTS.md (name the package and why it is needed)"
  fi
done

# 2. Vendored Flag files.
if [ -f "$ROOT/flag.lock" ]; then
  "$ROOT/scripts/flag-sync.sh" check || fail=1
else
  err "missing flag.lock (vendor Flag with scripts/flag-sync.sh add)"
fi

# 3. Third-party hosts in served files. Links (<a href>, rel=canonical) are
# fine; loading a script, stylesheet, font, icon or module from elsewhere is
# not. Perl reads each file whole, so tags split across lines and whitespace
# around "=" are caught too. Go files are scanned as well, for markup built
# in Go (only tag/import patterns match, not plain URLs in strings).
# Only Go's own test-only paths are skipped (*_test.go, testdata/): the go
# tool never builds them into the binary. Anything else under a web dir,
# including folders named test/ or e2e/, may be served and is scanned.
# shellcheck disable=SC2016 # the perl program is single-quoted on purpose
for d in $web; do
  [ -d "$ROOT/$d" ] || { err "stack.conf: web dir $d does not exist"; continue; }
  scan="$(mktemp)"
  if ! (set -o pipefail; find "$ROOT/$d" \( -name node_modules -o -name dist -o -name testdata \) -prune -o -type f \
             ! -name '*_test.go' \( -name '*.go' -o -name '*.html' -o -name '*.htm' -o -name '*.xhtml' -o -name '*.svg' -o -name '*.jsx' -o -name '*.cjs' -o -name '*.mts' -o -name '*.cts' -o -name '*.tmpl' -o -name '*.gohtml' -o -name '*.css' -o -name '*.js' -o -name '*.mjs' -o -name '*.ts' -o -name '*.tsx' -o -name '*.jinja' -o -name '*.j2' \) -print0 \
           | xargs -0 -r perl -0777 -ne '
               my $f = $ARGV;
               # $sp: whitespace, or a /* */ or // comment, anywhere a token may be
               # followed by one (import/* x */("https://…") is valid JS).
               my $sp = qr{(?:\s|/\*(?:[^*]|\*(?!/))*\*/|//[^\n]*\n)*};
               my $remote = qr{["'"'"'(]?$sp(?:https?:)?(?://|\\/\\/)(?!(?:localhost|127\.0\.0\.1)(?::\d+)?(?:[/?#\s"'"'"')>]|$))[^\s"'"'"')>]+}i;
               while (/<script\b[^>]*?\bsrc$sp=$sp$remote/gis) { (my $m = $&) =~ s/\s+/ /g; print "$f: $m\n"; }
               while (/(<link\b[^>]*>)/gis) { my $t = $1;
                 if ($t =~ /\brel$sp=$sp["'"'"']?[^"'"'"'>]*\b(?:stylesheet|preload|modulepreload|icon|apple-touch-icon|manifest)\b/i && $t =~ /\bhref$sp=$sp$remote/i) {
                   (my $one = $t) =~ s/\s+/ /g; print "$f: $one\n"; } }
               while (/(?:\.src$sp=|(?<!\blocation)\.href$sp=|\bsetAttribute$sp\($sp["'"'"'](?:src|href)["'"'"']$sp,|\bnew(?=[\s\/])$sp(?:Shared)?Worker$sp\(|\bimportScripts$sp\(|<base\b[^>]*?\bhref$sp=)$sp$remote/gi) { (my $m = $&) =~ s/\s+/ /g; print "$f: $m\n"; }
               while (/(?:\@import$sp(?:url\()?|(?<![\w.\$])url\(|(?<![\w.\$])import$sp\(?|(?<![\w.\$])from)$sp$remote/gi) { (my $m = $&) =~ s/\s+/ /g; print "$f: $m\n"; }
             ' > "$scan"); then
    err "remote-asset scanner failed in $d (is perl installed?)"
  fi
  while IFS= read -r hit; do
    err "remote asset in served file: ${hit#"$ROOT"/}"
  done < "$scan"
  rm -f "$scan"
done

# 4. npm trees. Every package.json must be declared in stack.conf, so adding
# one cannot slip past these rules (recorded fixtures under testdata/ or
# fixtures/ are data, not npm trees).
while IFS= read -r pj; do
  rel="$(dirname "${pj#"$ROOT"/}")"; [ "$rel" = "$ROOT" ] && rel=.
  [ "$pj" = "$ROOT/package.json" ] && rel=.
  case " $npm " in *" $rel "*) ;; *) err "$rel/package.json is not declared in stack.conf npm=" ;; esac
done < <(find "$ROOT" \( -name node_modules -o -name .git -o -name .flag -o -name dist -o -name testdata -o -name fixtures \) -prune -o -name package.json -type f -print)

for d in $npm; do
  pj="$ROOT/$d/package.json"
  [ -f "$pj" ] || { err "stack.conf: no package.json in $d"; continue; }
  # Every field npm installs at runtime (optional and peer deps install by
  # default; bundled deps ship inside the package).
  while IFS= read -r msg; do err "$d/package.json: $msg"; done < <(TIER="$tier" DEV_ALLOW="$(conf dev_allow)" perl -MJSON::PP -0777 -ne '
    my $j = eval { JSON::PP->new->decode($_) } or do { print "is not valid JSON\n"; exit };
    my $servers = qr/^(?:next|express|koa|fastify|\@hapi\/hapi|\@remix-run\/.+|nuxt|\@sveltejs\/kit|astro|hono)$/;
    my @runtime = qw(dependencies optionalDependencies peerDependencies);
    for my $f (@runtime, "devDependencies") {
      next unless ref $j->{$f} eq "HASH";
      for my $n (sort keys %{$j->{$f}}) { print "depends on JavaScript server framework $n ($f); the server is Go\n" if $n =~ $servers; }
    }
    if ($ENV{TIER} eq "A" && ref $j->{devDependencies} eq "HASH") {
      # Tier A ships no build step: dev deps are browser-test tooling only,
      # plus any names stack.conf lists in dev_allow= (explained in AGENTS.md).
      my %extra = map { $_ => 1 } split " ", ($ENV{DEV_ALLOW} // "");
      for my $n (sort keys %{$j->{devDependencies}}) {
        next if $n =~ /^(?:\@playwright\/test|playwright(?:-core)?|axe-core|\@axe-core\/[\w.-]+|\@types\/node)$/ || $extra{$n};
        print "Tier A devDependencies are browser-test tooling only; $n is not (add it to stack.conf dev_allow= with a reason in AGENTS.md if it truly belongs)\n";
      }
    }
    if ($ENV{TIER} eq "A") {
      for my $f (@runtime) { print "Tier A allows devDependencies only (browser tests), but $f is not empty\n" if ref $j->{$f} eq "HASH" && %{$j->{$f}}; }
      for my $f (qw(bundleDependencies bundledDependencies)) { print "Tier A allows devDependencies only, but $f is set\n" if $j->{$f} && (ref $j->{$f} ne "ARRAY" || @{$j->{$f}}); }
    }' "$pj")
done

if [ "$fail" -ne 0 ]; then echo "stack-check: FAILED (frontend standard v2, tier $tier)" >&2; exit 1; fi
echo "stack-check v$STACK_CHECK_VERSION: ok (frontend standard v2, tier $tier, backend ${backend:-go})"
