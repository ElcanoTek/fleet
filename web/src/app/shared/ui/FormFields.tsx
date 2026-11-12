import { t as tr } from "@/app/lib/i18n";
import { useState } from "react";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select";
import { Switch } from "@/components/ui/switch";
import { Textarea } from "@/components/ui/textarea";
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
// prompts (PromptLibraryForm), backed by Flag's @flag/* form primitives.

/** Every field of a form: the always-visible fields in a grid, then the
 *  `advanced` ones behind a disclosure that, while closed, summarises their
 *  current values so nothing is hidden without being visible at a glance. */
export function FormFields({
  fields,
  values,
  onChange,
  advancedLabel = tr("Advanced"),
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

const DENSE_FIELD_CLASS =
  "h-9 bg-[var(--color-bg)] px-2.5 py-2 text-[0.85rem]";

const DENSE_SELECT_CLASS =
  "h-9 bg-[var(--color-bg)] py-2 pr-8 pl-2.5 text-[0.85rem]";

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
      <div className={`flex min-h-9 items-center gap-2.5 self-end ${wide}`}>
        <Switch
          size="lg"
          checked={on}
          aria-label={field.label}
          onCheckedChange={(checked) => onChange(checked)}
        />
        <span className="text-[0.82rem] text-[var(--color-text-secondary)]">{field.label}</span>
      </div>
    );
  }

  const range = (value as DateRangeValue) ?? { from: "", to: "" };

  return (
    <Label className={`grid gap-1.5 font-normal ${wide}`}>
      <span className="px-0.5 text-[0.72rem] font-medium text-[var(--color-text-secondary)]">
        {field.label}
        {field.required ? <span className="ml-0.5 text-[var(--color-accent)]">*</span> : null}
      </span>

      {field.type === "select" ? (
        <NativeSelect
          className={DENSE_SELECT_CLASS}
          value={asText(value)}
          onChange={(e) => onChange(e.target.value)}
        >
          {(field.options ?? []).map((opt) => (
            <NativeSelectOption key={opt} value={opt}>
              {field.optionLabels?.[opt] ?? opt}
            </NativeSelectOption>
          ))}
        </NativeSelect>
      ) : null}

      {field.type === "text" ? (
        <Input
          type="text"
          className={DENSE_FIELD_CLASS}
          placeholder={field.placeholder}
          value={asInputText(value)}
          onChange={(e) => onChange(e.target.value)}
        />
      ) : null}

      {field.type === "textarea" ? (
        <Textarea
          rows={4}
          className="min-h-[5.5rem] resize-y bg-[var(--color-bg)] px-2.5 py-2 text-[0.85rem]"
          placeholder={field.placeholder}
          value={asInputText(value)}
          onChange={(e) => onChange(e.target.value)}
        />
      ) : null}

      {field.type === "number" ? (
        <Input
          type="number"
          className={DENSE_FIELD_CLASS}
          min={field.min ?? 0}
          value={asText(value)}
          onChange={(e) => onChange(e.target.value === "" ? "" : Number(e.target.value))}
        />
      ) : null}

      {field.type === "daterange" ? (
        <span className="flex items-center gap-2">
          <Input
            type="date"
            aria-label={tr("{0} from", [field.label])}
            className={DENSE_FIELD_CLASS}
            value={range.from}
            onChange={(e) => onChange({ ...range, from: e.target.value })}
          />
          <span className="text-[var(--color-text-muted)]">→</span>
          <Input
            type="date"
            aria-label={tr("{0} to", [field.label])}
            className={DENSE_FIELD_CLASS}
            value={range.to}
            onChange={(e) => onChange({ ...range, to: e.target.value })}
          />
        </span>
      ) : null}

      {field.hint ? (
        <span className="px-0.5 text-[0.7rem] text-[var(--color-text-muted)]">{field.hint}</span>
      ) : null}
    </Label>
  );
}

function describeValue(field: PillField, value: PillFieldValue | undefined): string {
  if (field.type === "toggle") return value ? tr("on") : tr("off");
  if (field.type === "daterange") {
    const r = (value as DateRangeValue) ?? { from: "", to: "" };
    return r.from || r.to ? `${r.from || "?"} → ${r.to || "?"}` : "—";
  }
  return asText(value) || "—";
}
