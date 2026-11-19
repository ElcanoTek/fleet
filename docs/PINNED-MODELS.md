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
FLEET_PINNED_MODELS='[{"slug":"qwen/qwen3-235b-a22b","label":"Acme Nova","tag":"alpha","free":true},{"slug":"deepseek/deepseek-v4.1-flash","tag":"beta","input_price_per_mtok":2,"output_price_per_mtok":8}]'
```

| Field   | Required | Meaning |
|---------|----------|---------|
| `slug`  | yes      | The model slug a turn or task runs on, exactly as you would type it in the picker (any routable slug; usually an OpenRouter catalog id). |
| `label` | no       | Display name for the row and the composer's model chip. Omitted: the catalog's own name. Max 80 characters. |
| `tag`   | no       | Short pill shown on the row (`alpha`, `beta`, `preview`, …). Max 16 characters. |
| `free`  | no       | `true` shows **Free** in place of the `$` cost band, on the row and on the composer's model chip. Cannot be combined with the price fields. |
| `input_price_per_mtok` / `output_price_per_mtok` | no | Displayed price in USD per million input / output tokens, replacing the catalog's for the cost band and its tooltip. Set either side or both; the side you leave out keeps the catalog price. Allowed range: 0–10,000. |

- **Validation fails at boot.** Bad JSON, an unknown field (such as a typo
  like `lable`), a missing or whitespace-containing slug, a duplicate slug,
  more than 20 pins, an over-long label/tag, a price outside 0–10,000, or
  `free` together with a price stops `fleet serve` with a
  `FLEET_PINNED_MODELS:` error. Pins are never dropped silently.
- **Delivery.** The pins ride on `/api/client-config` as `models.pinned`
  (omitted when unset), next to the model tiers.
- **Chat picker.** The pins come right after the two **recommended** rows, in
  the order declared, ahead of workspace and catalog rows. A row with a tag
  shows it as its pill; **recommended** still wins if a pin is also a tier.
  Rows get the catalog's cost band when the catalog has the slug. A pin with a
  label is renamed everywhere the picker and chip name it, lockdown lists
  included. A pin for a workspace-provider model keeps that model's name and
  context size. Its tooltip leaves
  out the slug, and search finds it by label only, so typing the base model's
  name does not bring up the renamed row.
- **The lab keeps a row of its own.** Before a search, the chat picker lists
  the newest model from each major lab (`/api/model-rankings`). When a lab's
  newest model is a pin with a `label`, that list also carries the lab's
  next-newest model that is not relabelled, so it appears under the lab's own
  name beside the relabelled row. The pin may name the model by a `:variant`
  or by its dated canonical slug. A pin without a label still shows the lab's
  name, so nothing is added for it. If the workspace's routing does not offer
  the pin, the lab's newest model is still listed under the lab's name, along
  with the next-newest. If the relabelled model is the lab's only one in the
  catalog, the lab has no other row to show.
- **Operations Center task picker.** Same label and tag. The pins come right
  after the recommended rows. As before, any workspace-provider rows still
  lead this picker. A relabelled row hides the grey slug line and does not
  match on the slug. Pins configured after the picker first loaded show up
  the next time it opens.
- If a native provider's catch-all shadows OpenRouter, a pin is offered on the
  explicit `<openrouter-provider>/<slug>` route, the same as a catalog row,
  and keeps its label and tag.
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

A user who opens the developer tools can find the base model.

**Zero-rated (`free: true`) pins also zero out runtime USD metering.** Setting
`free: true` (or setting both `input_price_per_mtok` and `output_price_per_mtok`
to `0`) registers a `$0.00` runtime pricing override for the pinned slug (unless
the client bundle manifest already declares an explicit override for that model).
Runs on a zero-rated pinned model therefore accrue `$0.00` toward per-run cost
ceilings, principal USD budgets, and `/analytics` billed spend while still
recording 100% of prompt, completion, and cached tokens. Non-zero price
overrides on a pin remain display-only for the picker cost band unless paired
with a bundle `pricing.overrides` entry.

## Deferred

- Changing pins needs a restart. They are not an admin setting in
  **Settings → Admin** and do not hot-reload.
- Tags are free text with a single style. There is no per-tag colour.
