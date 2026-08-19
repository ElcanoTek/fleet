# Third-party notice — Bento

`Bento_Slides.bento.html` in this directory is **vendored third-party software**,
redistributed unmodified. It is not fleet code.

| | |
| --- | --- |
| Project | Bento (`bento/slides`) — <https://bento.page> |
| Source | <https://github.com/nyblnet/bento> |
| Version | release **v1.0.18** (2026-08-15), asset `Bento_Slides.bento.html` |
| Size | 689,316 bytes, byte-identical to the upstream release artifact |
| sha256 | `9fef088beb763e86a7c13b6b5e2226816a9e8e1c61331f0c5270fdd5cf538424` |
| License | MIT — Copyright (c) 2026 The Bento authors |

`../references/authoring.md` is the matching authoring guide for the same version,
taken from the published <https://bento.page/agents.md> (also MIT, same copyright).
The repository copy at `docs/agents.md` was *not* used: it still carries an
unsubstituted `__APP_VERSION__` placeholder where the published build has `1.0.18`.
sha256 `82d8d7291a772dd3da4af112233a04504a8480a63ac68ab07bf7b8828e7add4f`.

## MIT license

The full MIT license text is reproduced in the `NOTICE` comment near the top of
`Bento_Slides.bento.html` itself, alongside the notices for the components Bento
bundles into its shell. Under the MIT terms that notice must accompany copies —
because it lives *inside* the file, it travels with every deck the agent produces
from this template, which is exactly what the license requires.

## Components bundled inside the shell

Bento's own `THIRD_PARTY_NOTICES.md` enumerates these; they are listed here so a
reader of this repo does not have to open a 689KB file to learn what is in it:

- **reveal.js** — MIT, © 2011-2024 Hakim El Hattab and contributors. Powers the
  present-mode slideshow.
- **Moveable** — MIT, © 2019 Daybrush (Younkue Choi). On-canvas drag/resize/rotate,
  plus the author's supporting `@daybrush/*`, `@scena/*`, `@egjs/*`, `@cfcs/*`
  modules (all MIT, same copyright).
- **Selecto** — MIT, © 2020 Daybrush (Younkue Choi). Marquee selection.
- **Fraunces** — SIL Open Font License 1.1, © 2020 The Fraunces Project Authors.
- **Instrument Sans** — SIL Open Font License 1.1, © 2022 The Instrument Sans
  Project Authors.

Fonts are embedded as document assets only in decks that use them.

## Supply-chain scope — read this before upgrading

**No CI scanner watches this file or the dependencies pinned inside it.**
`govulncheck` covers Go modules only; the Grype job scans the sandbox container
image built from `config/default/sandbox/Containerfile`, not this repo or the
`fleet` binary; and Dependabot has no manifest to read here. The pinned sha256 in
`internal/clientconfig/builtin_skills_bento_test.go` is the only tripwire, and it
detects *corruption or an undeclared swap* — not a newly disclosed CVE in
reveal.js.

Re-vendoring is therefore a deliberate manual act:

1. Download the new release asset from the upstream release page.
2. Replace `Bento_Slides.bento.html` with it, unmodified.
3. Update the version, size and sha256 **in this file and in the Go test** in the
   same commit — the test is what makes step 2 verifiable.
4. Re-check `references/authoring.md` against the published guide for that version;
   the document format is additive, but the recipes and gotchas change.
5. Confirm the document block is still `<script type="application/bento+json"
   id="bento-doc">` and still appears exactly once — `scripts/bento_doc.py` and the
   test both depend on that anchor.

## Offline posture — what fleet disables, and how

fleet ships Bento as a strictly offline viewer/editor. The vendored shell has two
behaviors that would make a delivered deck a network client, and
`scripts/bento_doc.py new` disables both.

| Upstream behavior | Endpoint | Trigger | Status |
| --- | --- | --- | --- |
| App update check | `https://bento.page/releases/slides/manifest.json` | every launch, on by default | **blocked** |
| Live collaboration | `wss://sync.bento.page/d/<room>` | any deck whose document has a `collab` block | **blocked** |

The collaboration one is the sharper of the two. `bornWithCollab = !!doc.collab`
is the whole eligibility test, so a deck that merely *carries* a collab object
opens a session on load with no user action and retries on failure. A file like
that is a live, writable door into whoever opens it.

### Three layers, deliberately

1. **A CSP `<meta>` tag** planted ahead of the runtime, with `connect-src 'none'`.
   The **browser** enforces this, so it does not depend on the app cooperating,
   on `localStorage`, or on our own script running. `default-src 'none'` with
   `object-src`/`frame-src`/`base-uri`/`form-action` at `'none'` and `img-src`
   limited to `data:`/`blob:` also make several of SKILL.md's authoring rules
   browser-enforced rather than remembered — including remote images, which are
   beacons that report who opened a deck and when. `script-src 'unsafe-inline'`
   is unavoidable: the app *is* inline script.
2. **Upstream's own offline switch** (`bento-offline=on`). The app then refuses
   network at its own chokepoints — `fi()` for fetch, `Ry()` for WebSocket —
   never attaches a collab transport, and strips remote asset URLs at render.
   This makes it fail *coherently*, with its own error type and UI, instead of
   retrying into the CSP wall. It needs `localStorage`, which some browsers deny
   for `file://` URLs; layer 1 is what covers that case.
3. **No `collab` block in the document.** `set` refuses to write one — it drops
   the block whether it came from the target file or from the incoming document —
   so a deck fleet produces has nothing for a session to attach to. Reported on
   stderr, never silently: only the user can weigh a session they may be relying
   on. Note this reverses #1197, which restored collab keys so redaction could
   not destroy a live session; that treated a collab block as inert data, which
   it is not.

### Verified in Chromium, by hand

The Go tests are structural — they pin presence, ordering and idempotency, which
is what rots silently. They cannot prove wire behavior, and CI has no browser in
the Go lane. This matrix was run by hand with every request intercepted and the
page instrumented (`WebSocket`, `fetch`, `XMLHttpRequest`, `sendBeacon` recorded
from an init script before any app code ran):

| Deck | WebSocket | fetch | Left the process |
| --- | --- | --- | --- |
| upstream shell, unguarded | — | `bento.page` manifest | manifest |
| upstream shell, unguarded, document carries `collab` | `sync.bento.page` ×5 (retrying) | `bento.page` manifest | both |
| **fleet deck from `new`** | none | none | **none** |
| **fleet deck, document force-fed a live `collab` block** | none | none | **none** |
| **as above, with `localStorage` denied** (layer 2 cannot engage) | attempted, **CSP refused** | attempted, **CSP refused** | **none** |

The last row is the point of the layering: the app tries both connections and the
browser refuses both.

### Known residue, not papered over

The app calls `ensureCollab()` unconditionally when it attaches a document, so a
deck **saved from the app's own UI** gets a freshly minted `collab` block —
including a room URL and private key — written into the file, even though nothing
ever connects. Both guard layers survive that save (verified: the CSP meta and the
guard script are both present in `serialize()` output), so the saved deck stays
offline. But the key material is in the file. It is inert while the guard is
there, and the room has no other participant. If anyone ever strips the guard from
such a deck, treat its keys as public and rotate.

`scripts/bento_doc.py set` removes the block again, so any revision delivered
through this skill ships clean.
