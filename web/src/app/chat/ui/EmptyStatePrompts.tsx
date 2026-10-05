"use client";

import { useState } from "react";
import { FormFields } from "@/app/shared/ui/FormFields";
import {
  formInitialValues,
  isPillReady,
  pillToPrompt,
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
  // 2×2 on desktop (single column at ≤900px, matching the design's
  // .suggestions breakpoint) instead of two uneven groups where the lone
  // Optimization card dangled under its own header.
  return (
    <div className="grid w-full max-w-[44rem] gap-2.5 min-[901px]:grid-cols-2">
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

  const set = (key: string, value: PillFieldValue) =>
    setValues((prev) => ({ ...prev, [key]: value }));

  const ready = isPillReady(pill, values);
  const generatedPrompt = pillToPrompt(pill, values);

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
      {/* Shared with the Prompt Library's form prompts; see FormFields. */}
      <FormFields fields={pill.fields ?? []} values={values} onChange={set} />

      <GeneratedPrompt text={generatedPrompt} />

      <div className="flex flex-wrap items-center justify-between gap-2">
        <button
          type="button"
          onClick={() =>
            canStartChat
              ? onStartChat(pill.starterPrompt ?? "")
              : onDescribe(generatedPrompt)
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
            className="inline-flex items-center gap-1.5 rounded-[var(--radius-md)] bg-[image:var(--gradient-action-primary)] px-3.5 py-2 text-[0.85rem] font-semibold text-[var(--color-on-primary)] transition hover:opacity-90 focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] disabled:cursor-not-allowed disabled:bg-none disabled:bg-[var(--color-surface-2)] disabled:text-[var(--color-text-muted)]"
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
      <p className="whitespace-pre-wrap text-[0.8rem] leading-snug text-[var(--color-text-secondary)]">
        {text}
      </p>
    </div>
  );
}
