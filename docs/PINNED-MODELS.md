# Pinned and relabelled picker models (`FLEET_PINNED_MODELS`)

## Why

An operator sometimes wants a specific model to be easy to find and to appear
under a name of their choosing. One example is an alpha run of an in-house
fine-tune: pin the open-weights base model it will be trained from, show it
as "Acme Nova" with an **alpha** pill, and collect feedback before the
fine-tune ships.

## What shipped

`FLEET_PINNED_MODELS` is a JSON array of pins, read at boot:

```sh
FLEET_PINNED_MODELS='[{"slug":"qwen/qwen3-235b-a22b","label":"Acme Nova","tag":"alpha"},{"slug":"deepseek/deepseek-v4.1-flash","tag":"beta"}]'
```

| Field   | Required | Meaning |
|---------|----------|---------|
| `slug`  | yes      | The model slug a turn or task runs on, exactly as you would type it in the picker (any routable slug; usually an OpenRouter catalog id). |
| `label` | no       | Display name for the row and the composer's model chip. Omitted: the catalog's own name. Max 80 characters. |
| `tag`   | no       | Short pill shown on the row (`alpha`, `beta`, `preview`, …). Max 16 characters. |

- **Validation fails at boot.** Bad JSON, an unknown field (such as a typo
  like `lable`), a missing or whitespace-containing slug, a duplicate slug,
  more than 20 pins, or an over-long label/tag stops `fleet serve` with a
  `FLEET_PINNED_MODELS:` error. Pins are never dropped silently.
- **Delivery.** The pins ride on `/api/client-config` as `models.pinned`
  (omitted when unset), next to the model tiers.
- **Chat picker.** The pins come right after the two **recommended** rows, in
  the order declared, ahead of workspace and catalog rows. A row with a tag
  shows it as its pill; **recommended** still wins if a pin is also a tier.
  Rows get the catalog's cost band when the catalog has the slug. A pin with a
  label is renamed everywhere the picker and chip name it. Its tooltip leaves
  out the slug, and search finds it by label only, so typing the base model's
  name does not bring up the renamed row.
- **Operations Center task picker.** Same order, label and tag. A relabelled
  row hides the grey slug line and does not match on the slug.
- Pins honour routing the same way recommended rows do: a pin the workspace's
  providers cannot route is hidden.
- No changes to cost ceilings, routing, credentials, sandboxing, or audit.

## What this is not

**This is a display alias, not a secrecy boundary.** The real slug is still:

- the value the browser sends for each turn and task, so it can be seen in
  the browser's network tools and the `/api/client-config` payload;
- what the server resolves, prices, meters and records. Admin and API
  surfaces that show a run's or task's model, and exported definitions, show
  the real slug.

A user who opens the developer tools can find the base model. Hiding it would
take a server-side alias: a slug of your own that the resolver maps to the
real model, with pricing, context-window and reasoning-family lookups all
following that mapping. That was deliberately left out of this change.

## Deferred

- Changing pins needs a restart. They are not an admin setting in
  **Settings → Admin** and do not hot-reload.
- Tags are free text with a single style. There is no per-tag colour.
