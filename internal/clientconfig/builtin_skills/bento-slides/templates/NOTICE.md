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
