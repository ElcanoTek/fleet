import { t as tr } from "@/app/lib/i18n";
import type { EmailTemplateSection } from "@/app/shared/lib/orchestratorApi";

// How the Email templates library words a template's enforced limits
// (docs/EMAIL-TEMPLATES.md). The limit names are the bundle's; a name this
// build does not know is left out rather than shown raw, so a newer bundle
// never puts "max_something: 3" in front of a person.

const BLOCK_LABELS: Record<string, () => string> = {
  text: () => tr("Text"),
  headline_metrics: () => tr("Number cards"),
  metrics: () => tr("Key figures"),
  callout: () => tr("Callout box"),
  table: () => tr("Table"),
  chart: () => tr("Chart"),
  cta: () => tr("Button"),
  closing: () => tr("Sign-off"),
};

/** The reader's name for a mail block type; the raw type when unknown. */
export function blockLabel(block: string): string {
  return BLOCK_LABELS[block]?.() ?? block;
}

/** A section's limits as short phrases, in a fixed reading order. */
export function limitPhrases(limits: EmailTemplateSection["limits"]): string[] {
  const l = limits ?? {};
  const has = (key: string) => typeof l[key] === "number";
  const out: string[] = [];

  if (has("max_words")) out.push(tr("Up to {0} words", [l.max_words]));

  if (l.max_paragraphs === 0) out.push(tr("No paragraphs"));
  else if (l.max_paragraphs === 1) out.push(tr("One paragraph"));
  else if (has("max_paragraphs")) out.push(tr("Up to {0} paragraphs", [l.max_paragraphs]));

  if (l.max_bullets === 0) out.push(tr("No bullets"));
  else if (has("min_bullets") && l.min_bullets === l.max_bullets) out.push(tr("Exactly {0} bullets", [l.max_bullets]));
  else if (has("min_bullets") && has("max_bullets")) out.push(tr("{0} to {1} bullets", [l.min_bullets, l.max_bullets]));
  else if (has("max_bullets")) out.push(tr("Up to {0} bullets", [l.max_bullets]));
  else if (has("min_bullets")) out.push(tr("At least {0} bullets", [l.min_bullets]));
  if (has("max_words_per_bullet")) out.push(tr("{0} words per bullet at most", [l.max_words_per_bullet]));

  if (has("min_items") && l.min_items === l.max_items) out.push(tr("Exactly {0} numbers", [l.max_items]));
  else if (has("min_items") && has("max_items")) out.push(tr("{0} to {1} numbers", [l.min_items, l.max_items]));
  else if (has("max_items")) out.push(tr("Up to {0} numbers", [l.max_items]));
  else if (has("min_items")) out.push(tr("At least {0} numbers", [l.min_items]));

  if (has("max_rows")) out.push(tr("Up to {0} rows", [l.max_rows]));
  if (has("max_columns")) out.push(tr("Up to {0} columns", [l.max_columns]));
  return out;
}

/** A never-say entry as shown: a `re:` pattern is described, not printed. */
export function neverSayLabel(entry: string): string | null {
  return entry.trim().startsWith("re:") ? null : entry.trim();
}
