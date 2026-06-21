# Email Themes

Per-client color themes for HTML email rendering. Drop a YAML file in
`themes/` and it's automatically available — no registration needed.

The canonical template and rendering rules live in
[`protocols/email-style.yaml`](../email-style.yaml). This directory holds
the per-client theme files that override specific hex values, plus the
canonical palette reference below for theme authors.

## Theme file schema

```yaml
theme_id: <slug>            # must match the filename stem
description: <one-liner>    # human summary of the palette
color_overrides:            # map of canonical_hex -> client_hex
  "#AABBCC": "#112233"
  ...
```

A theme with `color_overrides: {}` is the identity — it renders the
canonical palette unchanged.

## Canonical palette

Every hex value that appears in `canonical_template` paired with its
semantic role. Theme authors use this table to decide which hex values
to override. Anything **not** overridden inherits the canonical value,
which is already accessibility-tested against the body backgrounds.

| Hex       | Role                                                        |
|-----------|-------------------------------------------------------------|
| `#1A0B1E` | Header background (dark brand surface)                      |
| `#7272AB` | Header top accent border, lavender accents                  |
| `#6262A0` | Status badge + CTA button fill                              |
| `#D2D9DE` | Kicker / header subtext                                     |
| `#BEB6CD` | Header meta text (right side)                               |
| `#FAFAFA` | Header heading text, CTA label, neutral row background      |
| `#F4F6FB` | Page background, metrics card bg, footer bg, zebra row      |
| `#EEF3FF` | Status bar bg, table header bg, highlighted stacked-card    |
| `#D7DEEE` | Container border, table border, card border, mobile border  |
| `#E6EBF5` | Table inner row separator                                   |
| `#141824` | Body text                                                   |
| `#33415F` | Secondary text, table header text, outlook/next-steps text  |
| `#586F7C` | Section kickers, mobile inline labels, muted summary labels |
| `#5C6A87` | Footer text                                                 |
| `#FFF7E8` | Risk callout background                                     |
| `#F0D7AF` | Risk callout border (top/right/bottom)                      |
| `#FF8847` | Risk callout left accent border                             |
| `#8F5A12` | Risk callout heading text                                   |
| `#EFFAF3` | Opportunity callout background                              |
| `#CDEDD8` | Opportunity callout border (top/right/bottom)               |
| `#50FA7B` | Opportunity callout left accent border                      |
| `#1C5A33` | Opportunity heading text, positive delta text               |
| `#7C1F1F` | Negative delta text                                         |

## Adding a new theme

1. Review the canonical palette table above to see which hex values map
   to which roles (header surface, CTA fill, accent border, etc.).
2. Create `themes/<client>.yaml` with just the overrides you need.
   Everything else inherits the canonical value.
3. Contrast-check each override (4.5:1 normal text, 3.0:1 large text).
4. Test: ask the agent to send a sample email "with the `<client>` theme".

The agent discovers themes by listing this directory — no need to register
them anywhere else.
