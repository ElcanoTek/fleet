"use client";

import { useState } from "react";
import { FormFields } from "./FormFields";
import { Icon } from "./Icon";
import {
  formInitialValues,
  isPillReady,
  pillToPrompt,
  type PillField,
  type PillFieldValue,
  type PillFormShape,
  type PillValues,
} from "@/app/chat/ui/protocolPills";
import type { PromptLibraryItem } from "@/app/shared/lib/orchestratorApi";

// A Prompt Library FORM prompt: a Git YAML prompt that declares `fields` and a
// `promptTemplate` (docs/PROMPT-LIBRARY.md, "Form prompts"). Selecting one in
// the picker shows this form where a plain entry shows its text, so nobody has
// to hunt for `[PLACEHOLDERS]` inside an inserted prompt and overwrite each
// one by hand — the failure that motivated forms in the first place.
//
// What it deliberately does NOT do is send anything. "Use prompt" renders the
// template and hands the text to the same onInsert a plain entry uses, so it
// lands in the chat composer or the task's prompt exactly where a plain entry
// would, and the user can still read it, edit it and attach files before
// anything runs. The form is a faster way to write the draft, not a new way to
// start work.
//
// The fields are drawn by FormFields, the components the chat empty-state
// cards use, and rendered by the cards' pillToPrompt with one extra rule
// (dropBlankLines): a template line whose tokens were all left blank is
// dropped, so an unanswered optional field vanishes instead of leaving
// "Deals: {deals}" in the prompt. The cards do not pass that option, so their
// output is unchanged.

/** A library entry carrying a usable form. The server only sends `fields`
 *  and `prompt_template` for a form that validated, but the picker checks
 *  both anyway so a partial payload falls back to the plain behaviour. */
export type FormPromptItem = PromptLibraryItem & {
  fields: PillField[];
  prompt_template: string;
};

export function isFormPrompt(p: PromptLibraryItem): p is FormPromptItem {
  return (
    Array.isArray(p.fields) &&
    p.fields.length > 0 &&
    typeof p.prompt_template === "string" &&
    p.prompt_template.trim() !== ""
  );
}

export function PromptLibraryForm({
  prompt,
  note,
  onUse,
}: {
  prompt: FormPromptItem;
  /** Provenance line shown beside the actions ("Tracked in prompts/…"). */
  note: string;
  /** Receives the text to insert: the rendered prompt, or the raw template. */
  onUse: (text: string) => void;
}) {
  const shape: PillFormShape = {
    title: prompt.name,
    fields: prompt.fields,
    promptTemplate: prompt.prompt_template,
  };
  const [values, setValues] = useState<PillValues>(() => formInitialValues(shape));
  const set = (key: string, value: PillFieldValue) =>
    setValues((prev) => ({ ...prev, [key]: value }));

  const rendered = pillToPrompt(shape, values, { dropBlankLines: true });
  const ready = isPillReady(shape, values) && rendered.trim() !== "";

  return (
    <section
      aria-label={`${prompt.name} form`}
      className="mt-3 flex min-h-0 flex-1 flex-col gap-3"
    >
      <p className="m-0 text-xs text-[var(--color-text-muted)]">
        Fill in the form, then <strong>Use prompt</strong> puts the finished
        prompt in your draft to review before you send it. Fields marked{" "}
        <span className="text-[var(--color-accent)]">*</span> are required.
      </p>
      <div className="grid gap-3">
        <FormFields
          fields={prompt.fields}
          values={values}
          onChange={set}
          advancedLabel="More options"
        />
      </div>
      <div className="flex min-h-0 flex-1 flex-col gap-1">
        <span className="text-[0.66rem] font-semibold uppercase tracking-[0.1em] text-[var(--color-text-muted)]">
          Prompt preview
        </span>
        <pre
          data-testid="prompt-form-preview"
          className="m-0 min-h-24 flex-1 overflow-auto whitespace-pre-wrap rounded-lg border border-[var(--color-border)] bg-[var(--color-surface-2)] p-3 text-xs text-[var(--color-text-secondary)]"
        >
          {rendered}
        </pre>
      </div>
      <div className="flex flex-wrap items-center justify-between gap-3">
        <span className="text-xs text-[var(--color-text-muted)]">{note}</span>
        <div className="flex flex-wrap items-center gap-2">
          <button
            type="button"
            className="btn btn-secondary"
            // The template with its {tokens} intact, for someone who would
            // rather fill it in by hand in the draft.
            onClick={() => onUse(prompt.prompt_template)}
          >
            Insert raw prompt
          </button>
          <button
            type="button"
            className="btn btn-primary"
            disabled={!ready}
            onClick={() => ready && onUse(rendered)}
          >
            <Icon name="arrow-up" className="mr-1.5 size-3.5 rotate-90" />
            Use prompt
          </button>
        </div>
      </div>
    </section>
  );
}
