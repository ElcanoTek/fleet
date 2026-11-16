import { t as tr } from "@/app/lib/i18n";
import { useState } from "react";
import type { PillField } from "@/app/chat/ui/protocolPills";
import {
  formatCustomPalette,
  paletteColors,
  parseCustomPalette,
  recentPalettes,
} from "@/app/shared/lib/formPickers";

// The `palette` form field: a choice of looks, each drawn with its own
// colors, instead of a dropdown of names. With `custom` on the field the
// person can also pick the colors themselves, and the looks this browser
// used last are offered first so a client's colors do not have to be found
// again every week.
//
// The value is a plain string for the form's template: an option, or
// "custom (Header #1F3A5F, Accent #C2410C)" (shared/lib/formPickers.ts).

/** What a custom color starts at before the person picks one. */
const CUSTOM_START = "#1F3A5F";

function Dots({ colors }: { colors: string[] }) {
  return (
    <span className="inline-flex shrink-0 overflow-hidden rounded-full border border-[var(--color-border-strong)]" aria-hidden="true">
      {colors.map((color, i) => (
        <span key={`${color}-${i}`} className="block h-4 w-3.5" style={{ backgroundColor: color }} />
      ))}
    </span>
  );
}

const CHOICE =
  "inline-flex min-h-8 items-center gap-2 rounded-full border px-2.5 py-1 text-[0.8rem] transition";
const CHOICE_ON = "border-[var(--color-accent)] bg-[var(--color-surface-2)] text-[var(--color-text-primary)]";
const CHOICE_OFF =
  "border-[var(--color-border)] text-[var(--color-text-secondary)] hover:border-[var(--color-border-strong)] hover:text-[var(--color-text-primary)]";

export function PalettePicker({
  field,
  value,
  onChange,
}: {
  field: PillField;
  value: string;
  onChange: (value: string) => void;
}) {
  const names = field.custom ?? [];
  const custom = parseCustomPalette(value, names);
  // Read once per mount: the list changes only when a form is used, which
  // closes this one.
  const [recent] = useState(() => recentPalettes(field));
  // The custom colors outlive a detour to a preset, so switching back to
  // Custom returns to what was picked.
  const [colors, setColors] = useState<Record<string, string>>(
    () => custom ?? Object.fromEntries(names.map((name) => [name, CUSTOM_START])),
  );
  const label = (option: string) => field.optionLabels?.[option] ?? option;
  const customLabel = (name: string) => field.customLabels?.[name] ?? name;
  const describe = (v: string) => {
    const picked = parseCustomPalette(v, names);
    return picked ? names.map((name) => `${customLabel(name)} ${picked[name]}`).join(", ") : label(v);
  };

  const pickColor = (name: string, color: string) => {
    const next = { ...colors, [name]: color.toUpperCase() };
    setColors(next);
    onChange(formatCustomPalette(names, next));
  };

  return (
    <div role="group" aria-label={field.label} className="grid gap-1.5 sm:col-span-2">
      <span className="px-0.5 text-[0.72rem] font-medium text-[var(--color-text-secondary)]">
        {field.label}
      </span>

      <div role="radiogroup" aria-label={field.label} className="flex flex-wrap gap-1.5">
        {(field.options ?? []).map((option) => (
          <button
            key={option}
            type="button"
            role="radio"
            aria-checked={value === option}
            className={`${CHOICE} ${value === option ? CHOICE_ON : CHOICE_OFF}`}
            onClick={() => onChange(option)}
          >
            <Dots colors={field.swatches?.[option] ?? []} />
            <span className="capitalize">{label(option)}</span>
          </button>
        ))}
        {names.length > 0 ? (
          <button
            type="button"
            role="radio"
            aria-checked={custom !== null}
            className={`${CHOICE} ${custom ? CHOICE_ON : CHOICE_OFF}`}
            onClick={() => onChange(formatCustomPalette(names, colors))}
          >
            <Dots colors={names.map((name) => colors[name])} />
            {tr("Custom colors")}
          </button>
        ) : null}
      </div>

      {custom ? (
        <div className="flex flex-wrap gap-3" data-testid="palette-custom">
          {names.map((name) => (
            <label key={name} className="inline-flex items-center gap-2 text-[0.8rem] text-[var(--color-text-secondary)]">
              <input
                type="color"
                aria-label={customLabel(name)}
                className="h-8 w-10 cursor-pointer rounded border border-[var(--color-border)] bg-transparent p-0.5"
                value={colors[name].toLowerCase()}
                onChange={(e) => pickColor(name, e.target.value)}
              />
              {customLabel(name)}
              <code className="text-[0.72rem] text-[var(--color-text-muted)]">{colors[name]}</code>
            </label>
          ))}
        </div>
      ) : null}

      {recent.length > 0 ? (
        <div className="grid gap-1">
          <span className="px-0.5 text-[0.66rem] font-semibold uppercase tracking-[0.1em] text-[var(--color-text-muted)]">
            {tr("Recently used")}
          </span>
          <div className="flex flex-wrap gap-1.5" data-testid="palette-recent">
            {recent.map((v) => (
              <button
                key={v}
                type="button"
                aria-pressed={v === value}
                aria-label={tr("Use {0}", [describe(v)])}
                title={describe(v)}
                className={`${CHOICE} ${v === value ? CHOICE_ON : CHOICE_OFF}`}
                onClick={() => {
                  const picked = parseCustomPalette(v, names);
                  if (picked) setColors(picked);
                  onChange(v);
                }}
              >
                <Dots colors={paletteColors(field, v) ?? []} />
                {parseCustomPalette(v, names) ? tr("Custom") : <span className="capitalize">{label(v)}</span>}
              </button>
            ))}
          </div>
        </div>
      ) : null}

      {field.hint ? (
        <span className="px-0.5 text-[0.7rem] text-[var(--color-text-muted)]">{field.hint}</span>
      ) : null}
    </div>
  );
}
