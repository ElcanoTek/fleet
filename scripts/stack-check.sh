#!/usr/bin/env bash
# SPDX-License-Identifier: MIT
# Copyright (c) 2026 ElcanoTek, Inc. (canonical copy: ElcanoTek/flag tools/product/)
#
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
#   3. No served file loads a script, stylesheet, font, image, frame or module from another host.
#   4. npm trees: no server frameworks (next, express, ...) unless backend= is a
#      JavaScript runtime; Tier A has no runtime deps; every Tier B runtime dep
#      beyond react/react-dom is named in AGENTS.md with its reason.
set -euo pipefail
set -f # stack.conf values are words, never globs
unset CDPATH
export LC_ALL=C

STACK_CHECK_VERSION=25
STANDARD_SHA=5b3a1c9dab098277570a37d58366b0802d4378370f9717d48743c386b194bdbb
ROOT="$(cd -P -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)"
CONF="$ROOT/stack.conf"
fail=0
err() { echo "stack-check: $*" >&2; fail=1; }

sha() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum | cut -d' ' -f1
  else shasum -a 256 | cut -d' ' -f1; fi
}
conf() { sed -n "s/^$1=//p" "$CONF" | sed 's/[[:space:]]*#.*$//' | tail -1; }
# outside_block: AGENTS.md without the shared standard block.
outside_block() { awk '/^<!-- BEGIN:elcano-frontend-standard/{f=1} !f{print} /^<!-- END:elcano-frontend-standard/{f=0}' "$ROOT/AGENTS.md" 2>/dev/null; }
# agents_names WORD: AGENTS.md (outside the shared block) names WORD as a whole
# word, so "ws" is not satisfied by "rows". It reads a captured copy: under
# pipefail, `outside_block | grep -q` fails whenever grep exits before awk
# finishes writing (SIGPIPE), which made the answer depend on timing.
agents_names() { grep -wF -- "$1" <<< "$OUTSIDE" >/dev/null; }
# repo_dir NAME: a stack.conf directory must be a plain path inside the repo.
repo_dir() {
  case "$1" in /*|*\\*) return 1 ;; esac
  case "/$1/" in */../*) return 1 ;; esac
  return 0
}

[ -f "$CONF" ] || { echo "stack-check: missing stack.conf" >&2; exit 2; }
command -v perl >/dev/null 2>&1 || { echo "stack-check: perl is required (core JSON::PP)" >&2; exit 2; }
tier="$(conf tier)"; backend="$(conf backend)"
OUTSIDE="$(outside_block || true)"
read -r -a web <<< "$(conf web)" || true
read -r -a npm <<< "$(conf npm)" || true
read -r -a dev_allow <<< "$(conf dev_allow)" || true
case "$tier" in A|B) ;; *) err "stack.conf: tier must be A or B (got '$tier')";; esac
# The backend is declared, never assumed: a non-Go product must not pass as Go
# by leaving the line out.
[ -n "$backend" ] || err "stack.conf: backend= is required (go, or another backend with an AGENTS.md exception)"
# An empty scan scope would silently disable the no-CDN gate.
[ "${#web[@]}" -gt 0 ] || err "stack.conf: web= must list at least one served directory"

# 1. The standard block.
if [ ! -f "$ROOT/AGENTS.md" ]; then err "missing AGENTS.md"
else
  got="$(awk '/^<!-- BEGIN:elcano-frontend-standard/{f=1;next}/^<!-- END:elcano-frontend-standard/{f=0}f' "$ROOT/AGENTS.md" | sha)"
  [ "$got" = "$STANDARD_SHA" ] || err "AGENTS.md: the elcano-frontend-standard block is missing or edited (sha $got). It is shared verbatim across repos; change it everywhere and bump STANDARD_SHA in every scripts/stack-check.sh."
  # A non-Go backend needs its own AGENTS.md heading naming the exception and
  # the backend (the shared block mentions exceptions, so it does not count).
  if [ -n "$backend" ] && [ "$backend" != go ] && ! { grep -iE '^#{1,6} .*exception' <<< "$OUTSIDE" || true; } | grep -iF -- "$backend" >/dev/null; then
    err "stack.conf: backend=$backend needs an AGENTS.md heading like '## Exception: $backend backend' explaining why"
  fi
fi

# dev_allow= names are exceptions to Tier A: each must be explained in AGENTS.md.
for pkg in "${dev_allow[@]+"${dev_allow[@]}"}"; do
  if ! agents_names "$pkg"; then
    err "stack.conf: dev_allow=$pkg must be explained in AGENTS.md (name the package and why it is needed)"
  fi
done

# 2. Vendored Flag files.
if [ -f "$ROOT/flag.lock" ]; then
  "$ROOT/scripts/flag-sync.sh" check || fail=1
else
  err "missing flag.lock (vendor Flag with scripts/flag-sync.sh add)"
fi

# 3. Third-party hosts in served files.
#
# What is scanned: every file git tracks (or would add: untracked, not
# ignored) under each web= dir, so build output and node_modules that are
# gitignored are skipped but anything committed — in dist/, test/, wherever —
# is scanned. Without git, every file under the dir except node_modules/ and
# dist/. Only Go's own test-only paths are skipped (*_test.go, testdata/): the
# go tool never builds them into the binary. Binary assets (fonts, images,
# archives) are skipped by extension. A symlink or submodule in a web dir fails
# the check: its target would be served unscanned.
#
# What fails: loading a script, stylesheet, font, image, media, frame, object,
# SVG <use>/<image>, module, worker or import-map entry from another host, in
# any static form a reviewer can grep for:
#   - tags (HTML, SVG, and markup inside JS/Go strings): any src / srcset /
#     poster / background / data-src attribute, <object data>, href on <base>,
#     <use>, <image>, <script>, <feImage> and other SVG references, and
#     <link href> unless every rel token is a plain link (canonical,
#     alternate, preconnect, ...); <script type=importmap> entries;
#   - JS: x.src= / .srcset= / .href= (not location.href), x["src"]=,
#     setAttribute(NS)("src"|"href"|...), {src: ...}, new Worker(...),
#     new Audio(...), worklet .addModule(...), importScripts(...),
#     import(...), import/export ... from "...";
#   - CSS: @import, url(...), image-set("...").
# Evasions that are still static are normalized first: HTML entities
# (&#104;ttps, &colon;, &sol;), JS/CSS escapes (\x2f, /, \/, \2f),
# tabs/newlines inside URLs, JSX {"..."} values, uppercase schemes, "\\"
# backslash slashes, protocol-relative "//", scheme without slashes, and
# adjacent string-literal concatenation ("https:" + "//cdn...").
#
# What passes: plain links (<a href>, <area href>, <link rel=canonical>,
# <form action>), navigation (location.href =, window.open, meta refresh), and
# loopback hosts (exactly localhost / 127.0.0.1, optional port).
#
# Out of scope (cannot be decided by a static scan; CSP is the backstop):
# URLs computed at run time (variables, config, string building other than
# adjacent literals, server-side templates such as {{.CDN}}), fetch()/XHR/
# WebSocket/EventSource data calls (the API surface; governed by CSP
# connect-src — fetch-then-eval needs 'unsafe-eval', which the standard's CSP
# forbids), and anything outside the web= dirs.
#
# shellcheck disable=SC2016 # the perl program is single-quoted on purpose
SCAN_PL='
use strict; use warnings;
my $markup = qr/\.(?:html?|xhtml|svg|xml|xsl|xslt|tmpl|gohtml|jinja|j2|vue|svelte|md|mdx|hbs|handlebars|mustache|ejs|njk|liquid|shtml|astro)$/i;
my $Q = "\x27"; # single quote
my $sp = qr{(?:\s|/\*(?:[^*]|\*(?!/))*\*/|//[^\n]*\n)*};
# A loopback host: exactly localhost or 127.0.0.1, optional port, then the
# end of the authority.
my $lb = qr{(?:localhost|127\.0\.0\.1)(?::\d+)?(?:[/\\?#\s"`)>$Q]|$)}i;
# Start of a remote URL. Raw text: "//" (or JSON "\/\/", or "\\") so that
# JSON-escaped relative paths ("\/assets") are not mistaken for one. Decoded
# text and HTML attributes: any two slashes or backslashes (the URL parser
# treats "\" as "/"). With a scheme, any number of slashes.
my $pre_raw = qr{(?>https?:[/\\]*|//|\\/\\/|\\\\)}i;
my $pre_dec = qr{(?>https?:[/\\]*|[/\\]{2,})}i;
my $host = qr{(?![/\\])[^\s"`)>$Q]};
my %ent = (colon=>":", sol=>"/", bsol=>"\\", period=>".", lpar=>"(", rpar=>")", quot=>"\"", apos=>$Q, lt=>"<", gt=>">",
  amp=>"&", tab=>"\t", newline=>"\n", num=>"#", equals=>"=", grave=>"`", comma=>",", semi=>";", quest=>"?", commat=>"@",
  percnt=>"%", plus=>"+", lsqb=>"[", rsqb=>"]", lbrack=>"[", rbrack=>"]", lcub=>"{", rcub=>"}", lbrace=>"{", rbrace=>"}");
sub cp { my $n = shift; return ($n > 0 && $n < 0x110000) ? chr($n) : ""; }
sub ents { my $s = shift;
  $s =~ s/&(?:#[xX]0*([0-9a-fA-F]{1,6});?|#0*([0-9]{1,7});?|([a-zA-Z]+);)/defined $1 ? cp(hex $1) : defined $2 ? cp($2) : exists $ent{lc $3} ? $ent{lc $3} : $&/ge;
  return $s; }
sub jsun { my $s = shift;
  $s =~ s/\\(?:x([0-9a-fA-F]{2})|u\{([0-9a-fA-F]{1,6})\}|u([0-9a-fA-F]{4})|([0-9a-fA-F]{1,6}) ?|([tnr])|(.))/defined $1 ? cp(hex $1) : defined $2 ? cp(hex $2) : defined $3 ? cp(hex $3) : defined $4 ? cp(hex $4) : defined $5 ? "" : $6/gse;
  return $s; }
sub remote_val { my ($v, $html) = @_;
  $v = ents($v);
  $v =~ s/[\t\n\r]//g; $v =~ s/^[\x00-\x20]+//; $v =~ s/^\{\s*["`$Q]?\s*//;
  my $pre = $html ? $pre_dec : $pre_raw;
  return $v =~ /^$pre(?!$lb)$host/ ? 1 : 0; }
sub remote_srcset { my ($v, $html) = @_; for my $c (split /,/, ents($v)) { return 1 if remote_val($c, $html); } return 0; }
my %navrel = map { $_ => 1 } qw(alternate author bookmark canonical external help license me next nofollow noopener
  noreferrer opener prev previous search tag dns-prefetch preconnect pingback privacy-policy terms-of-service shortlink
  first last up index start contents edit webmention);
my %refel = map { $_ => 1 } qw(base use image script feimage pattern lineargradient radialgradient filter textpath mpath tref cursor animate set animatemotion animatetransform);
my $attr = qr{(?>[\s/]+|[^\s/>=][^\s/>=]*(?>\s*=\s*(?>"[^"]*"|\x27[^\x27]*\x27|[^\s>]*))?|=[^\s/>=]*)};
my $nattr = qr{([^\s/>=][^\s/>=]*|=[^\s/>=]*)(?:\s*=\s*("[^"]*"|\x27[^\x27]*\x27|[^\s>]*))?};
my (%seen, $file);
sub hit { my $m = shift; $m =~ s/\s+/ /g; $m =~ s/[^\x20-\x7e]/?/g; $m = substr($m, 0, 200);
  print "$file: $m\n" unless $seen{$m}++; }
# Every "<" starts a tag parse of its own (a tag hidden inside another
# tag'"'"'s quoted attribute is still parsed), with the HTML attribute grammar.
sub tags { my ($t, $html) = @_;
  while ($t =~ /<(?=[a-zA-Z])/g) {
    my $p = pos($t);
    if ($t =~ /\G([a-zA-Z][^\s\/>]*)((?:$attr)*+)/gc) {
      my ($name, $as, $end) = (lc $1, $2, pos($t));
      # XML documents may prefix any element or attribute (svg:script,
      # x:href): compare local names.
      $name =~ s/^[^:]*://;
      my %a; while ($as =~ /$nattr/g) { my ($n, $v) = (lc $1, $2 // ""); $v =~ s/^"(.*)"$/$1/s or $v =~ s/^\x27(.*)\x27$/$1/s; push @{$a{$n}}, $v; }
      my $bad = 0;
      for my $n (qw(src poster background data-src lowsrc dynsrc)) { $bad ||= grep { remote_val($_, $html) } @{$a{$n} || []}; }
      for my $n (qw(srcset data-srcset imagesrcset)) { $bad ||= grep { remote_srcset($_, $html) } @{$a{$n} || []}; }
      $bad ||= grep { remote_val($_, $html) } @{$a{data} || []} if $name eq "object";
      if ($refel{$name}) { for my $n (grep { /^(?:[^:]*:)?href$|^xlinkhref$/ } keys %a) { $bad ||= grep { remote_val($_, $html) } @{$a{$n}}; } }
      if ($name eq "link") {
        my @rel = map { lc } map { split " ", ents($_) } @{$a{rel} || []};
        if (grep { !$navrel{$_} } @rel) { $bad ||= grep { remote_val($_, $html) } @{$a{href} || []}; }
      }
      if ($name eq "script" && grep { ents($_) =~ /importmap/i } @{$a{type} || []}) {
        my $body = substr($t, $end); $body =~ s{</script.*}{}is;
        $bad ||= $body =~ /["`$Q]\s*$pre_dec(?!$lb)$host/;
      }
      hit("<" . substr($t, $p, $end - $p) . ">") if $bad;
    }
    pos($t) = $p;
  }
}
sub patterns { my ($t, $pre) = @_;
  my $r = qr{["`$Q(]?$sp$pre(?!$lb)$host+};
  my $q = qr{["`$Q]};
  # A URL object stringifies to its href, so new URL("https://…") (also as
  # window./globalThis./self.URL) loads the same remote file as the bare string.
  # Any assignment that can store the URL: = ||= ??= &&= += (not == or =>).
  my $set = qr{(?:\|\||\?\?|&&|\+)?=(?![=>])};
  my $nu = qr{(?:\bnew$sp+(?:(?:window|globalThis|self)$sp\.$sp)?URL$sp\($sp)?};
  my @re = (
    qr{(?:\.(?:src|poster)|(?<!\blocation)\.href|\[$sp$q(?:src|href|srcset|poster)$q$sp\])$sp$set$sp$nu$r},
    qr{\.srcset$sp$set$sp$q(?:[^"`$Q]*,)?\s*$pre(?!$lb)$host+},
    qr{\bsetAttribute$sp\($sp$q(?:src|href|xlink:href|srcset|poster|data)$q$sp,$sp$nu$r}i,
    qr{\bsetAttributeNS$sp\(${sp}[^,()]*,$sp$q(?:xlink:)?(?:src|href)$q$sp,$sp$nu$r}i,
    qr{(?<![\w\$.\-])$q?src$q?$sp:$sp$nu$r},
    qr{\bnew(?=[\s/])$sp(?:(?:window|globalThis|self)$sp\.$sp)?(?:Shared)?Worker$sp\($sp$nu$r},
    qr{\bimportScripts$sp\($sp$nu$r},
    # Constructors and loaders that fetch media or code as soon as they run.
    qr{\bnew(?=[\s/])$sp(?:(?:window|globalThis|self)$sp\.$sp)?Audio$sp\($sp$nu$r},
    qr{\.addModule$sp\($sp$nu$r},
    qr{\@import$sp(?:url$sp\()?$sp$r}i,
    qr{(?<![\w.\$])url\($sp$r}i,
    qr{\bimage-set$sp\((?:[^;{}()]|\([^()]*\))*?$q$sp$pre(?!$lb)$host+}i,
    qr{(?<![\w.\$])import$sp\(?$sp$nu$r},
    qr{(?<![\w.\$])from$sp$r},
  );
  for my $re (@re) { while ($t =~ /$re/g) { hit($&); } }
}
for (@ARGV) {
  $file = $_; %seen = ();
  open(my $fh, "<", $file) or die "stack-check scanner: cannot read $file: $!\n";
  local $/; my $raw = <$fh>; close $fh;
  die "stack-check scanner: cannot read $file\n" unless defined $raw;
  # Go comments are never served: drop them with an exact Go lexer (strings,
  # raw strings and runes kept), so prose about <img src=//x> is not a hit.
  $raw =~ s{("(?:[^"\\\n]|\\.)*"|`[^`]*`|\x27(?:[^\x27\\\n]|\\.)*\x27|//[^\n]*|/\*.*?\*/)}{substr($1, 0, 1) eq "/" ? " " : $1}gse if $file =~ /\.go$/;
  # Decoded view: entities, JS/CSS escapes, then adjacent literal joins.
  my $dec = jsun(ents($raw));
  $dec =~ s/["`$Q]${sp}\+${sp}["`$Q]//g;
  tags($raw, $file =~ $markup ? 1 : 0);
  tags($dec, 1);
  patterns($raw, $pre_raw);
  patterns($dec, $pre_dec);
}
'
for d in "${web[@]+"${web[@]}"}"; do
  repo_dir "$d" || { err "stack.conf: web dir $d must be a path inside the repo"; continue; }
  if [ ! -d "$ROOT/$d" ] || [ -L "$ROOT/$d" ]; then err "stack.conf: web dir $d does not exist (or is a symlink)"; continue; fi
  # A symlinked ancestor hides the files from git ls-files while a server
  # still serves them: every component must be a real directory.
  p="$ROOT"; anc=""
  IFS=/ read -r -a segs <<< "$d"
  for seg in "${segs[@]}"; do
    [ -n "$seg" ] && [ "$seg" != . ] || continue
    p="$p/$seg"; [ -L "$p" ] && { anc="${p#"$ROOT"/}"; break; }
  done
  if [ -n "$anc" ]; then err "stack.conf: web dir $d is under a symlink ($anc)"; continue; fi
  list="$(mktemp)"; scan="$(mktemp)"
  if git -C "$ROOT" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
    lister=(git -C "$ROOT" -c core.quotePath=false --literal-pathspecs ls-files -z --cached --others --exclude-standard -- "$d")
  else
    lister=(find "$d" \( -name node_modules -o -name dist \) -prune -o ! -type d -print0)
  fi
  if ! (cd "$ROOT" && "${lister[@]}") > "$list"; then
    err "could not list files in $d"; rm -f "$list" "$scan"; continue
  fi
  files=()
  while IFS= read -r -d '' f; do
    case "/$f/" in */testdata/*) continue ;; esac
    case "$f" in
      *_test.go) continue ;;
      *.woff|*.woff2|*.ttf|*.otf|*.eot|*.png|*.jpg|*.jpeg|*.gif|*.webp|*.avif|*.ico|*.bmp|*.gz|*.br|*.zst|*.zip|*.pdf|*.mp4|*.webm|*.mp3|*.ogg|*.wav) continue ;;
    esac
    if [ -L "$ROOT/$f" ]; then err "symlink in served dir: $f (its target is served unscanned; commit a real file)"; continue; fi
    if [ -d "$ROOT/$f" ]; then err "submodule or nested repo in served dir: $f (its files are served unscanned)"; continue; fi
    [ -e "$ROOT/$f" ] || continue # tracked but deleted in this checkout: not served
    files+=("$f")
  done < "$list"
  if [ "${#files[@]}" -gt 0 ] && ! (cd "$ROOT" && printf '%s\0' "${files[@]}" | xargs -0 perl -e "$SCAN_PL") > "$scan"; then
    err "remote-asset scanner failed in $d"
  fi
  while IFS= read -r hit; do
    err "remote asset in served file: $hit"
  done < "$scan"
  rm -f "$list" "$scan"
done

# 4. npm trees. Every package.json must be declared in stack.conf, so adding
# one cannot slip past these rules (recorded fixtures under testdata/ or
# fixtures/ are data, not npm trees) — except that any package a scanned
# package.json names in "workspaces" is installed by npm, so it is always
# included, wherever it lives.
# shellcheck disable=SC2016 # the perl program is single-quoted on purpose
NPM_FIND_PL='
use strict; use warnings; use JSON::PP; use File::Find;
my $root = shift;
my (%all, %out, @queue);
find({ no_chdir => 1, wanted => sub {
  # A directory holding its own .git is a separate checkout (e.g. an agent
  # worktree under .claude/worktrees), not part of this tree. Git never tracks
  # a path named .git, so this cannot hide committed content.
  if (-d $_ && !-l $_ && (m{/(?:node_modules|\.git)$} || ($_ ne $root && -e "$_/.git"))) { $File::Find::prune = 1; return; }
  return unless m{(?:^|/)package\.json$} && (-f $_ || -l $_);
  (my $rel = $File::Find::dir) =~ s{^\Q$root\E/?}{}; $all{$rel eq "" ? "." : $rel} = 1;
} }, $root);
for my $d (sort keys %all) { push @queue, $d unless $d =~ m{(?:^|/)(?:\.flag|dist|testdata|fixtures)(?:/|$)}; }
push @queue, @ARGV;
sub glob_re { my $g = shift; my $re = "";
  while ($g =~ /\G(\*\*\/?|\*|\?|\{[^{}]*\}|\[[^\]]*\]|.)/gs) { my $t = $1;
    if ($t =~ /^\*\*/) { $re .= "(?:.*/)?" . ($t =~ m{/$} ? "" : "[^/]*"); }
    elsif ($t eq "*") { $re .= "[^/]*"; } elsif ($t eq "?") { $re .= "[^/]"; }
    elsif ($t =~ /^\{(.*)\}$/) { $re .= "(?:" . join("|", map { quotemeta } split /,/, $1, -1) . ")"; }
    elsif ($t =~ /^\[/) { $re .= $t; } else { $re .= quotemeta $t; } }
  return qr{^$re$}; }
while (defined(my $d = shift @queue)) {
  next if $out{$d}++;
  my $pj = $d eq "." ? "$root/package.json" : "$root/$d/package.json";
  next unless -e $pj;
  open(my $fh, "<", $pj) or die "cannot read $pj: $!\n"; local $/; my $txt = <$fh>; close $fh;
  my $j = eval { JSON::PP->new->decode($txt) };
  next unless ref $j eq "HASH" && defined $j->{workspaces}; # invalid JSON is reported by the rules check
  my $ws = $j->{workspaces}; $ws = $ws->{packages} if ref $ws eq "HASH";
  die "$d/package.json: workspaces must be an array of paths\n" unless ref $ws eq "ARRAY";
  for my $w (@$ws) {
    die "$d/package.json: bad workspaces entry\n" if ref $w || !defined $w;
    next if $w =~ /^!/; # an exclusion can only shrink the set; checking more is safe
    my $path = $d eq "." ? $w : "$d/$w"; $path =~ s{/+$}{}; $path =~ s{(?:^|/)\./}{/}g; $path =~ s{^/}{};
    my @seg; for (split m{/+}, $path) { if ($_ eq "..") { die "$d/package.json: workspace $w is outside the repo\n" unless @seg; pop @seg; } else { push @seg, $_; } }
    my $re = glob_re(join "/", @seg);
    push @queue, grep { $_ =~ $re } sort keys %all;
  }
}
print "$_\n" for sort keys %out;
'
# shellcheck disable=SC2016 # the perl program is single-quoted on purpose
NPM_RULES_PL='
use strict; use warnings; use JSON::PP;
local $/; my $txt = <STDIN>;
my $j = eval { JSON::PP->new->decode($txt) };
unless (ref $j eq "HASH") { print "is not a valid JSON object\n"; exit 0; }
my $servers = qr/^(?:next|express|koa|fastify|\@hapi\/.+|\@remix-run\/.+|\@react-router\/(?:serve|node|express|architect|cloudflare)|nuxt|\@sveltejs\/kit|astro|hono|\@hono\/node-server|\@nestjs\/.+|\@adonisjs\/.+|\@feathersjs\/.+|\@loopback\/.+|\@redwoodjs\/.+|\@solidjs\/start|\@tanstack\/(?:react-)?start|\@builder\.io\/qwik-city|\@angular\/ssr|\@analogjs\/platform|\@marko\/run|sails|restify|polka|micro|h3|nitropack|nitro|vinxi|elysia|waku|gatsby)$/;
# A documented JavaScript backend exception (backend=node|bun|deno) may use a
# server framework; every other backend serves the app itself.
my $backend = $ENV{BACKEND} // "go";
my $js_backend = $backend =~ /^(?:node|nodejs|bun|deno)$/;
# The Tier B runtime baseline; anything else must be justified by name in AGENTS.md.
my %tier_b_base = map { $_ => 1 } qw(react react-dom);
my $testing = qr/^(?:\@playwright\/test|playwright(?:-core)?|axe-core|\@axe-core\/[\w.-]+|\@types\/node)$/;
my @runtime = qw(dependencies optionalDependencies peerDependencies);
my %extra = map { $_ => 1 } split " ", ($ENV{DEV_ALLOW} // "");
# names NAME SPEC: the package names a dependency really installs — its key,
# an "npm:" alias target, and the repo/package named by a git or URL spec.
sub names { my ($n, $s) = @_; my @n = ($n); $s = "" unless defined $s && !ref $s;
  push @n, $1 if $s =~ /^npm:((?:\@[^\/\@]+\/)?[^\@]+)/;
  if ($s =~ m{^(?:git\+|git:|github:|gitlab:|bitbucket:|https?:|ssh:|file:|link:)|^[\w.-]+/[\w.-]+(?:#|$)}) {
    (my $p = $s) =~ s/#.*//; $p =~ s{\.git$}{}; $p =~ s{/+$}{}; push @n, $1 if $p =~ m{([^/:]+)$}; }
  return @n; }
for my $f (@runtime, "devDependencies") {
  next unless exists $j->{$f};
  unless (ref $j->{$f} eq "HASH") { print "$f must be an object\n"; next; }
  for my $n (sort keys %{$j->{$f}}) {
    for my $real (names($n, $j->{$f}{$n})) { print "depends on JavaScript server framework $real ($f: $n); the server is $backend\n" if !$js_backend && $real =~ $servers; }
  }
}
# overrides / resolutions can swap any installed package for another one.
sub walk { my ($v, $k) = @_;
  if (ref $v eq "HASH") { walk($v->{$_}, $_) for sort keys %$v; }
  elsif (!ref $v && defined $v) { for my $real (names($k // "", $v)) { print "overrides/resolutions install JavaScript server framework $real; the server is $backend\n" if !$js_backend && $real =~ $servers; } } }
walk($j->{overrides}); walk($j->{resolutions}); walk($j->{pnpm}{overrides}) if ref $j->{pnpm} eq "HASH";
if ($ENV{TIER} eq "B") {
  # Tier B keeps runtime dependencies few and justified: the shell checks that
  # each name below appears in AGENTS.md (outside the shared block).
  for my $f (@runtime) {
    next unless ref $j->{$f} eq "HASH";
    for my $n (sort keys %{$j->{$f}}) {
      for my $real (names($n, $j->{$f}{$n})) { print "\tJUSTIFY\t$real\t$f\n" unless $tier_b_base{$real}; }
    }
  }
}
if ($ENV{TIER} eq "A") {
  # Tier A ships no build step: dev deps are browser-test tooling only,
  # plus any names stack.conf lists in dev_allow= (explained in AGENTS.md).
  if (ref $j->{devDependencies} eq "HASH") {
    for my $n (sort keys %{$j->{devDependencies}}) {
      for my $real (names($n, $j->{devDependencies}{$n})) {
        next if $real =~ $testing || $extra{$real};
        print "Tier A devDependencies are browser-test tooling only; $real" . ($real eq $n ? "" : " (as $n)") . " is not (add it to stack.conf dev_allow= with a reason in AGENTS.md if it truly belongs)\n";
      }
    }
  }
  for my $f (@runtime) { print "Tier A allows devDependencies only (browser tests), but $f is not empty\n" if ref $j->{$f} eq "HASH" && %{$j->{$f}}; }
  for my $f (qw(bundleDependencies bundledDependencies)) { print "Tier A allows devDependencies only, but $f is set\n" if $j->{$f} && (ref $j->{$f} ne "ARRAY" || @{$j->{$f}}); }
}
'
for d in "${npm[@]+"${npm[@]}"}"; do repo_dir "$d" || err "stack.conf: npm dir $d must be a path inside the repo"; done
if ! found="$(perl -e "$NPM_FIND_PL" "$ROOT" "${npm[@]+"${npm[@]}"}")"; then
  err "package.json discovery failed"; found=""
fi
while IFS= read -r rel; do
  [ -n "$rel" ] || continue
  case " ${npm[*]+"${npm[*]}"} " in *" $rel "*) ;; *) err "$rel/package.json is not declared in stack.conf npm= (every npm tree, workspaces included)" ;; esac
done <<< "$found"

for d in "${npm[@]+"${npm[@]}"}"; do
  repo_dir "$d" || continue
  pj="$ROOT/$d/package.json"
  [ -f "$pj" ] || { err "stack.conf: no package.json in $d"; continue; }
  if ! out="$(TIER="$tier" BACKEND="$backend" DEV_ALLOW="${dev_allow[*]+"${dev_allow[*]}"}" perl -e "$NPM_RULES_PL" < "$pj")"; then
    err "$d/package.json: rules check failed"; continue
  fi
  while IFS= read -r msg; do
    case "$msg" in
      "") ;;
      $'\t'JUSTIFY$'\t'*)
        IFS=$'\t' read -r _ pkg field <<< "$msg"
        if ! agents_names "$pkg"; then
          err "$d/package.json: Tier B runtime dependency $pkg ($field) must be named in AGENTS.md with why it is needed (outside the shared block)"
        fi ;;
      *) err "$d/package.json: $msg" ;;
    esac
  done <<< "$out"
done

if [ "$fail" -ne 0 ]; then echo "stack-check: FAILED (frontend standard v2, tier $tier)" >&2; exit 1; fi
echo "stack-check v$STACK_CHECK_VERSION: ok (frontend standard v2, tier $tier, backend $backend)"
