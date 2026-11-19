# Credits: how fleet labels money for the people using it

Design note. Honest scope: this is a **wording and display** change. fleet
still meters model spend in US dollars; nothing about metering, ceilings,
budgets, the API or the configuration changed.

## What shipped

Everywhere a person using fleet sees an amount of model spend, it is labelled
in **credits** — `4.50 credits`, never `$4.50`, `USD` or "dollars". One credit
is one US dollar of metered spend, so the numbers are the same numbers; only
the label changed.

- **Web UI.** `web/src/app/shared/lib/credits.ts` is the one formatter
  (`formatCredits`, and `formatCreditAmount` for a cell whose header already
  names the unit); `shared/ui/CreditsValue.tsx` draws the headline-tile form
  (large number, small unit, never wrapped). It replaced four copies of an
  `Intl` currency formatter and several hand-built `"$" + n` strings. Surfaces:
  the chat "Show details" turn line, the compaction note and the conversation
  totals; the task form's **Max cost (credits)** ceiling, its estimate chip and
  the cost forecast; the run log's metrics strip; Analytics (overview banner,
  Models & Cost, Problem Chats, Spend & Budgets including the budget form,
  Team Adoption, Server & Live Agents); and the admin overview and settings
  hints (per-run cost ceiling, monthly soft spend limit, model price list).
- **Model pickers.** The `$ … $$$$` glyphs became a colour-coded four-pip
  credit-usage meter — see [`MODEL-COST-INDICATORS.md`](MODEL-COST-INDICATORS.md).
- **Text the server writes for people.** Run-stop reasons and budget refusals
  that the UI shows or a notification carries (`internal/agentcore` ceiling and
  wind-down notices, `internal/agent` run-stop reasons, `internal/scheduledrun`
  loop ceiling, `internal/sched/budget` hard-cap refusal and soft-budget
  alert), the task notification email's **Cost** row (`internal/notify`), the
  "Discuss this run" digest (`internal/webui`), and the usage report's pricing
  note (`internal/sched/handlers`). The ceiling and wind-down notices are also
  what the model reads, so an assistant relaying one says "credits" too.
- **Spanish and Portuguese** catalogs carry the same change (`créditos`).
- **User guides** (`/help`, the `fleet-guide` skill), in all three languages.

## What deliberately did not change

- **The wire and the configuration stay in USD.** API fields (`cost_usd`,
  `max_cost_usd`, `soft_usd`, …), `docs/openapi.yaml`, environment variables
  (`FLEET_MAX_COST_USD`, …), admin setting keys, the CSV export's column names,
  metrics, logs and the operator CLI (`fleet sched …`, `fleet cache-stats`) are
  unchanged. Renaming them would break API clients and operators for a label.
- **No exchange rate.** There is no setting that makes a credit worth anything
  other than one US dollar of metered spend. A deployment that wants a
  different rate needs a real conversion feature, which this is not.
- **Other people's prices and the user's own money are left alone.** The
  Microsoft 365 Copilot licence price on the Connections page is Microsoft's
  price in dollars. Patrol Launchpad example instructions ("Report money in USD
  with two decimals", "…more than $500 as urgent") are about the customer's own
  business figures, as are amounts in chat content, approval cards and
  dashboards that an agent or connector produced. The `$` shell prompt on a
  bash tool chip is not money.
- **Operator-authored templates.** A webhook notification template an operator
  wrote with a literal `$` renders as written.
- **Operator documentation** under `docs/` still describes pricing, ceilings
  and budgets in USD, because that is what an operator configures.

## Tests

- `web/src/app/shared/lib/credits.test.ts` — formatting, the sub-0.01
  widening, and that no currency symbol or code is ever produced.
- `modelCost.test.ts`, `ModelPicker.test.tsx`, `e2e/mocked/model-cost.spec.ts`
  — the meter, its accessible sentence in credits, no `$` in the listbox.
- The existing Analytics, Usage, Adoption, cost forecast, log viewer, task
  form and admin tests now assert the credit wording, as do the Go tests for
  each reworded server message.
