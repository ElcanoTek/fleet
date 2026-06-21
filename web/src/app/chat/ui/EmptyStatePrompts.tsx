"use client";

import { useState } from "react";
import {
  asText,
  formInitialValues,
  isPillReady,
  type DateRangeValue,
  type PillField,
  type PillFieldValue,
  type PillValues,
  type ProtocolPill,
} from "./protocolPills";

// Sprite-backed icon. Mirrors the (unexported) Icon in chat-experience.tsx;
// duplicated here to keep the import graph acyclic — this module is a child
// of chat-experience, not the other way around.
function PillIcon({ name, className }: { name: string; className?: string }) {
  return (
    <svg className={className} aria-hidden="true">
      <use href={`/icons/core-icons.svg#${name}`} />
    </svg>
  );
}

// ── empty-state card grid ──────────────────────────────────────────────────

export function EmptyStatePrompts({
  pills,
  onPick,
}: {
  pills: ProtocolPill[];
  onPick: (id: string) => void;
}) {
  if (pills.length === 0) return null;

  // One flat grid — no section labels. With four pills this lands as a tidy
  // 2×2 on desktop (single column on mobile) instead of two uneven groups
  // where the lone Optimization card dangled under its own header.
  return (
    <div className="grid w-full max-w-[44rem] gap-2.5 sm:grid-cols-2">
      {pills.map((pill) => (
        <PillCard key={pill.id} pill={pill} onPick={onPick} />
      ))}
    </div>
  );
}

function PillCard({ pill, onPick }: { pill: ProtocolPill; onPick: (id: string) => void }) {
  return (
    <button
      type="button"
      onClick={() => onPick(pill.id)}
      className="group flex items-start gap-3 rounded-[var(--radius-lg)] border border-[var(--color-border)] bg-[var(--gradient-surface-card)] p-3.5 text-left transition hover:border-[var(--color-accent)] hover:shadow-[var(--shadow-sm)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]"
    >
      <span className="flex size-9 shrink-0 items-center justify-center rounded-[var(--radius-md)] border border-[var(--color-border-strong)] bg-[var(--color-overlay-soft)] text-[var(--color-accent)]">
        <PillIcon name={pill.icon} className="size-[1.05rem]" />
      </span>
      <span className="min-w-0 flex-1">
        <span className="block text-[0.95rem] font-semibold leading-tight text-[var(--color-text-primary)]">
          {pill.title}
        </span>
        <span className="mt-1 block text-[0.8rem] leading-snug text-[var(--color-text-muted)]">
          {pill.desc}
        </span>
      </span>
      <PillIcon
        name="arrow-right"
        className="mt-0.5 size-4 shrink-0 text-[var(--color-text-muted)] transition group-hover:translate-x-0.5 group-hover:text-[var(--color-accent)]"
      />
    </button>
  );
}

// ── inline form / intake panel ─────────────────────────────────────────────

export function ProtocolPillForm({
  pill,
  onRun,
  onCancel,
  onDescribe,
  onStartChat,
}: {
  pill: ProtocolPill;
  /** Templated prompt is ready to send. */
  onRun: (prompt: string) => void;
  onCancel: () => void;
  /** Seed the composer instead of sending (form pills). */
  onDescribe: (preload: string) => void;
  /** Start the conversational intake (conversation pills). */
  onStartChat: (starter: string) => void;
}) {
  const [values, setValues] = useState<PillValues>(() => formInitialValues(pill));
  const [advOpen, setAdvOpen] = useState(false);

  const set = (key: string, value: PillFieldValue) =>
    setValues((prev) => ({ ...prev, [key]: value }));

  const fields = pill.fields ?? [];
  const baseFields = fields.filter((f) => !f.advanced);
  const advFields = fields.filter((f) => f.advanced);
  const ready = isPillReady(pill, values);
  const generatedPrompt = pill.promptTemplate?.(values) ?? "";

  // The skip link reads the same on every pill, but conversation pills (the
  // diagnostic) start a real chat intake while form pills seed the composer.
  const canStartChat = Boolean(pill.starterPrompt);

  return (
    <div className="grid w-full max-w-[44rem] gap-3 rounded-[var(--radius-lg)] border border-[var(--color-border)] bg-[var(--gradient-surface-card)] p-4 shadow-[var(--shadow-sm)] sm:p-5">
      <div className="flex items-center justify-between gap-3">
        <span className="inline-flex items-center gap-2 rounded-[var(--radius-pill)] border border-[var(--color-border-strong)] bg-[var(--color-overlay-soft)] px-2.5 py-1 text-[0.78rem] font-semibold text-[var(--color-text-primary)]">
          <PillIcon name={pill.icon} className="size-4 text-[var(--color-accent)]" />
          {pill.title}
        </span>
        <button
          type="button"
          aria-label="Cancel"
          onClick={onCancel}
          className="inline-flex size-8 items-center justify-center rounded-[var(--radius-md)] text-[var(--color-text-muted)] transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]"
        >
          <PillIcon name="close" className="size-4" />
        </button>
      </div>

      <p className="text-[0.83rem] leading-snug text-[var(--color-text-muted)]">
        A guide, not a gate — fill what you know and I’ll confirm the rest.
      </p>
      <FieldGrid fields={baseFields} values={values} onChange={set} />

      {advFields.length > 0 ? (
        <div className="grid gap-2.5">
          <button
            type="button"
            aria-expanded={advOpen}
            onClick={() => setAdvOpen((o) => !o)}
            className="inline-flex w-fit items-center gap-1.5 text-[0.8rem] text-[var(--color-text-secondary)] transition hover:text-[var(--color-text-primary)]"
          >
            <PillIcon
              name="chevron-right"
              className={`size-3.5 transition-transform ${advOpen ? "rotate-90" : ""}`}
            />
            Advanced
          </button>
          {advOpen ? (
            <FieldGrid fields={advFields} values={values} onChange={set} />
          ) : (
            <p className="px-1 text-[0.72rem] leading-snug text-[var(--color-text-muted)]">
              {advFields
                .map((f) => `${f.label}: ${describeValue(f, values[f.key])}`)
                .join("   ·   ")}
            </p>
          )}
        </div>
      ) : null}

      <GeneratedPrompt text={generatedPrompt} />

      <div className="flex flex-wrap items-center justify-between gap-2">
        <button
          type="button"
          onClick={() =>
            canStartChat
              ? onStartChat(pill.starterPrompt ?? "")
              : onDescribe(pill.describePreload?.(values) ?? "")
          }
          className="inline-flex items-center gap-1 text-[0.8rem] text-[var(--color-text-secondary)] transition hover:text-[var(--color-text-primary)] focus-visible:rounded-[var(--radius-sm)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]"
        >
          Skip the form, start in Chat
          <PillIcon name="arrow-right" className="size-3.5" />
        </button>
        <div className="flex items-center gap-2">
          <button
            type="button"
            onClick={onCancel}
            className="rounded-[var(--radius-md)] px-3 py-2 text-[0.85rem] text-[var(--color-text-secondary)] transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]"
          >
            Cancel
          </button>
          <button
            type="button"
            disabled={!ready}
            onClick={() => ready && onRun(generatedPrompt)}
            className="inline-flex items-center gap-1.5 rounded-[var(--radius-md)] bg-[image:var(--gradient-action-primary)] px-3.5 py-2 text-[0.85rem] font-semibold text-white transition hover:opacity-90 focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] disabled:cursor-not-allowed disabled:bg-none disabled:bg-[var(--color-surface-2)] disabled:text-[var(--color-text-muted)]"
          >
            {pill.cta}
            <PillIcon name="arrow-right" className="size-4" />
          </button>
        </div>
      </div>
    </div>
  );
}

function GeneratedPrompt({ text }: { text: string }) {
  if (!text.trim()) return null;
  return (
    <div className="grid gap-1 rounded-[var(--radius-md)] border border-[var(--color-border-subtle)] bg-[var(--subtle-panel-bg)] px-3 py-2">
      <span className="text-[0.66rem] font-semibold uppercase tracking-[0.1em] text-[var(--color-text-muted)]">
        Prompt preview
      </span>
      <p className="text-[0.8rem] leading-snug text-[var(--color-text-secondary)]">{text}</p>
    </div>
  );
}

// ── fields ──────────────────────────────────────────────────────────────

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

// text/daterange want the full row; select/number/toggle pair up.
function fieldSpansRow(field: PillField): boolean {
  return field.type === "text" || field.type === "daterange";
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
          <PillIcon
            name="chevron-down"
            className="pointer-events-none absolute right-3.5 top-1/2 size-4 -translate-y-1/2 text-[var(--color-text-muted)]"
          />
        </div>
      ) : null}

      {field.type === "text" ? (
        <input
          type="text"
          className={INPUT_CLASS}
          placeholder={field.placeholder}
          value={asText(value)}
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
