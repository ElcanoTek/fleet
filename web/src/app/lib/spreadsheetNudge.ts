// Spreadsheet-model nudge.
//
// When a user attaches a non-trivial Excel workbook with the cheap
// fast model selected, suggest the stronger model. Spreadsheets with
// curated tracking tabs (multi-band layouts, custom metric vocab,
// goals blocks above the data) are exactly the workload where the
// fast tier produces shallow analysis. The nudge is a one-line
// banner in the composer with a "Switch" link — silent auto-switch
// would change cost without consent.
//
// The helper is pure. The composer wires its result to a banner;
// tests assert the predicate without touching the DOM.

import { ADVANCED_MODEL, DEFAULT_MODEL } from "./modelAliases";

// Roughly the size of a single-sheet ad-hoc export. Anything heavier
// is almost always a multi-sheet workbook with curated tabs — the
// case where Sonnet's depth pays off. Tuned from observed tracking
// docs (~1.7 MB) vs. ad-hoc exports (~30–80 KB); 250 KB sits in the
// quiet middle and avoids false positives on simple downloads.
export const HEAVY_SPREADSHEET_THRESHOLD_BYTES = 250_000;

// The nudge fires only on these extensions. CSVs and Google Sheets
// exports as TSV are excluded — they're flat by definition.
export const SPREADSHEET_EXTENSIONS = [".xlsx", ".xlsm", ".xls"] as const;

export type AttachmentLike = {
  name: string;
  size: number;
};

export function isHeavySpreadsheet(file: AttachmentLike): boolean {
  const lowered = file.name.toLowerCase();
  const matchesExt = SPREADSHEET_EXTENSIONS.some((ext) => lowered.endsWith(ext));
  if (!matchesExt) return false;
  return file.size >= HEAVY_SPREADSHEET_THRESHOLD_BYTES;
}

export type NudgeArgs = {
  attachments: ReadonlyArray<AttachmentLike>;
  selectedModel: string;
  dismissed: boolean;
  // Slugs are injected (not imported) so callers can swap them in
  // tests and so the helper stays decoupled from the alias module.
  defaultModel?: string;
  advancedModel?: string;
};

export type NudgeDecision = {
  show: boolean;
  // The slug to switch to when the user clicks "Switch". Stable
  // across renders so the button can wire it directly.
  recommendedModel: string;
};

export function decideSpreadsheetNudge(args: NudgeArgs): NudgeDecision {
  const defaultModel = args.defaultModel ?? DEFAULT_MODEL;
  const advancedModel = args.advancedModel ?? ADVANCED_MODEL;
  const decision: NudgeDecision = { show: false, recommendedModel: advancedModel };
  if (args.dismissed) return decision;
  // Only nudge from the cheap fast tier — if the user is already on
  // a non-default model they've made an explicit choice we should
  // respect.
  if (args.selectedModel !== defaultModel) return decision;
  if (!args.attachments.some(isHeavySpreadsheet)) return decision;
  return { show: true, recommendedModel: advancedModel };
}
