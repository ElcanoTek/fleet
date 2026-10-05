"use client";

import { useState } from "react";
import { Icon } from "./Icon";
import {
  asInputText,
  asText,
  type DateRangeValue,
  type PillField,
  type PillFieldValue,
  type PillValues,
} from "@/app/chat/ui/protocolPills";

// The field controls behind every bundle-defined form: the chat empty-state
// cards (EmptyStatePrompts' ProtocolPillForm) and the Prompt Library's form
// prompts (PromptLibraryForm). Both forms are declared in bundle YAML with the
// one PillField schema, so they are drawn by this one set of components — a
// fix to how a select or a textarea behaves lands on both surfaces at once,
// and a bundle author sees the same field render the same way wherever they
// declared it.
//
// This was lifted out of EmptyStatePrompts.tsx verbatim (markup, classes and
// behaviour unchanged) when the Prompt Library grew forms. The one new knob is
// `advancedLabel`: the cards keep calling their disclosure "Advanced", the
// library calls it "More options", which reads better above optional inputs
// to a prompt than "Advanced" does.

/** Every field of a form: the always-visible fields in a grid, then the
 *  `advanced` ones behind a disclosure that, while closed, summarises their
 *  current values so nothing is hidden without being visible at a glance. */
export function FormFields({
  fields,
  values,
  onChange,
  advancedLabel = "Advanced",
}: {
  fields: PillField[];
  values: PillValues;
  onChange: (key: string, value: PillFieldValue) => void;
  advancedLabel?: string;
}) {
  const [advOpen, setAdvOpen] = useState(false);
  const baseFields = fields.filter((f) => !f.advanced);
  const advFields = fields.filter((f) => f.advanced);

  return (
    <>
      <FieldGrid fields={baseFields} values={values} onChange={onChange} />

      {advFields.length > 0 ? (
        <div className="grid gap-2.5">
          <button
            type="button"
            aria-expanded={advOpen}
            onClick={() => setAdvOpen((o) => !o)}
            className="inline-flex w-fit items-center gap-1.5 text-[0.8rem] text-[var(--color-text-secondary)] transition hover:text-[var(--color-text-primary)]"
          >
            <Icon
              name="chevron-right"
              className={`size-3.5 transition-transform ${advOpen ? "rotate-90" : ""}`}
            />
            {advancedLabel}
          </button>
          {advOpen ? (
            <FieldGrid fields={advFields} values={values} onChange={onChange} />
          ) : (
            <p className="px-1 text-[0.72rem] leading-snug text-[var(--color-text-muted)]">
              {advFields
                .map((f) => `${f.label}: ${describeValue(f, values[f.key])}`)
                .join("   ·   ")}
            </p>
          )}
        </div>
      ) : null}
    </>
  );
}

function FieldGrid({
  fields,
  values,
  onChange,
}: {
  fields: PillField[];
  values: PillValues;
  onChange: (key: string, value: PillFieldValue) => void;
}) {
  if (fields.length === 0) return null;
  return (
    <div className="grid gap-3 sm:grid-cols-2">
      {fields.map((field) => (
        <Field
          key={field.key}
          field={field}
          value={values[field.key]}
          onChange={(v) => onChange(field.key, v)}
        />
      ))}
    </div>
  );
}

const INPUT_CLASS =
  "w-full rounded-[var(--radius-md)] border border-[var(--color-border-strong)] bg-[var(--color-bg)] px-2.5 py-2 text-[0.85rem] text-[var(--color-text-primary)] outline-none transition placeholder:text-[var(--color-text-muted)] focus:border-[var(--color-accent)] focus-visible:shadow-[var(--focus-ring)]";

// text/textarea/daterange want the full row; select/number/toggle pair up.
function fieldSpansRow(field: PillField): boolean {
  return field.type === "text" || field.type === "textarea" || field.type === "daterange";
}

function Field({
  field,
  value,
  onChange,
}: {
  field: PillField;
  value: PillFieldValue | undefined;
  onChange: (value: PillFieldValue) => void;
}) {
  const wide = fieldSpansRow(field) ? "sm:col-span-2" : "";

  if (field.type === "toggle") {
    const on = value === true;
    return (
      // Anchored to the input row (self-end + input-height) so the switch
      // lines up with a sibling field's control, not its label.
      <div className={`flex min-h-9 items-center gap-2.5 self-end ${wide}`}>
        <button
          type="button"
          role="switch"
          aria-checked={on}
          aria-label={field.label}
          onClick={() => onChange(!on)}
          className={`relative inline-flex h-5 w-9 shrink-0 items-center rounded-full transition focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] ${
            on ? "bg-[var(--color-accent)]" : "bg-[var(--color-border-strong)]"
          }`}
        >
          <span
            className={`inline-block size-4 rounded-full bg-white transition-transform ${
              on ? "translate-x-4" : "translate-x-0.5"
            }`}
          />
        </button>
        <span className="text-[0.82rem] text-[var(--color-text-secondary)]">{field.label}</span>
      </div>
    );
  }

  const range = (value as DateRangeValue) ?? { from: "", to: "" };

  return (
    <label className={`grid gap-1.5 ${wide}`}>
      <span className="px-0.5 text-[0.72rem] font-medium text-[var(--color-text-secondary)]">
        {field.label}
        {field.required ? <span className="ml-0.5 text-[var(--color-accent)]">*</span> : null}
      </span>

      {field.type === "select" ? (
        // The native chevron sits at a fixed inset that padding can't budge, so
        // hide it (appearance-none) and render our own with room to its right.
        <div className="relative">
          <select
            className={`${INPUT_CLASS} appearance-none pr-9`}
            value={asText(value)}
            onChange={(e) => onChange(e.target.value)}
          >
            {(field.options ?? []).map((opt) => (
              <option key={opt} value={opt}>
                {opt}
              </option>
            ))}
          </select>
          <Icon
            name="chevron-down"
            className="pointer-events-none absolute right-3.5 top-1/2 size-4 -translate-y-1/2 text-[var(--color-text-muted)]"
          />
        </div>
      ) : null}

      {field.type === "text" ? (
        // Controlled inputs render the RAW string: `asText` trims, and a
        // trimmed re-render swallows the space the user just typed, so a
        // two-word client name could never be entered. Trimming happens once,
        // in pillToPrompt.
        <input
          type="text"
          className={INPUT_CLASS}
          placeholder={field.placeholder}
          value={asInputText(value)}
          onChange={(e) => onChange(e.target.value)}
        />
      ) : null}

      {field.type === "textarea" ? (
        // Multi-line free text (KPI lists, "anything else I should know").
        // Same controlled-value handling as the text input; line breaks
        // survive into the prompt, only the ends are trimmed.
        <textarea
          rows={4}
          className={`${INPUT_CLASS} min-h-[5.5rem] resize-y`}
          placeholder={field.placeholder}
          value={asInputText(value)}
          onChange={(e) => onChange(e.target.value)}
        />
      ) : null}

      {field.type === "number" ? (
        <input
          type="number"
          className={INPUT_CLASS}
          min={field.min ?? 0}
          value={asText(value)}
          onChange={(e) => onChange(e.target.value === "" ? "" : Number(e.target.value))}
        />
      ) : null}

      {field.type === "daterange" ? (
        <span className="flex items-center gap-2">
          <input
            type="date"
            aria-label={`${field.label} from`}
            className={INPUT_CLASS}
            value={range.from}
            onChange={(e) => onChange({ ...range, from: e.target.value })}
          />
          <span className="text-[var(--color-text-muted)]">→</span>
          <input
            type="date"
            aria-label={`${field.label} to`}
            className={INPUT_CLASS}
            value={range.to}
            onChange={(e) => onChange({ ...range, to: e.target.value })}
          />
        </span>
      ) : null}

      {field.hint ? (
        <span className="px-0.5 text-[0.7rem] text-[var(--color-text-muted)]">{field.hint}</span>
      ) : null}
    </label>
  );
}

function describeValue(field: PillField, value: PillFieldValue | undefined): string {
  if (field.type === "toggle") return value ? "on" : "off";
  if (field.type === "daterange") {
    const r = (value as DateRangeValue) ?? { from: "", to: "" };
    return r.from || r.to ? `${r.from || "?"} → ${r.to || "?"}` : "—";
  }
  return asText(value) || "—";
}
