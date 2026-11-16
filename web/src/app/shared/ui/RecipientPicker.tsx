import { t as tr } from "@/app/lib/i18n";
import { useEffect, useId, useMemo, useState } from "react";
import { Input } from "@/components/ui/input";
import type { PillField } from "@/app/chat/ui/protocolPills";
import { orchestratorApi } from "@/app/shared/lib/orchestratorApi";
import { isValidEmail } from "@/app/shared/lib/format";
import { describeEmailError } from "@/app/shared/lib/validation";
import {
  joinRecipients,
  rememberRecipients,
  rememberedRecipients,
  splitRecipients,
} from "@/app/shared/lib/formPickers";
import { Icon } from "./Icon";

// The `recipients` form field: who a message goes to, as removable chips, with
// the addresses used most offered underneath so the usual team is one click
// each, and a box to add anyone else.
//
// The offered addresses come from three places, in this order: the ones the
// bundle lists on the field (`options`), the ones the person's own tasks
// already mail (GET /tasks/recipients, most-used first), and the ones this
// browser added here before. An address typed into the box is remembered at
// once, so it is offered the next time without the form having been used.
//
// The value is the plain string the form's template gets: the addresses
// joined with ", ".

/** How many suggestions show before "Show all". */
const SUGGESTIONS_SHOWN = 8;

export function RecipientPicker({
  field,
  value,
  onChange,
}: {
  field: PillField;
  value: string;
  onChange: (value: string) => void;
}) {
  const inputId = useId();
  const chosen = useMemo(() => splitRecipients(value), [value]);
  const [draft, setDraft] = useState("");
  const [error, setError] = useState("");
  const [showAll, setShowAll] = useState(false);
  const [history, setHistory] = useState<string[]>([]);
  const [remembered, setRemembered] = useState<string[]>(() => rememberedRecipients());

  // Optional: a failed or missing history just means fewer suggestions.
  useEffect(() => {
    let cancelled = false;
    Promise.resolve()
      .then(() => orchestratorApi.recipientHistory())
      .then((rows) => {
        if (cancelled || !Array.isArray(rows)) return;
        const ranked = [...rows].sort((a, b) => b.task_count - a.task_count);
        setHistory(ranked.map((r) => r.email.toLowerCase()).filter(isValidEmail));
      })
      .catch(() => {});
    return () => {
      cancelled = true;
    };
  }, []);

  const suggestions = useMemo(() => {
    const seen = new Set(chosen);
    const out: string[] = [];
    const typed = draft.trim().toLowerCase();
    for (const email of [...splitRecipients((field.options ?? []).join(",")), ...history, ...remembered]) {
      if (seen.has(email) || (typed && !email.includes(typed))) continue;
      seen.add(email);
      out.push(email);
    }
    return out;
  }, [chosen, draft, field.options, history, remembered]);
  const visible = showAll ? suggestions : suggestions.slice(0, SUGGESTIONS_SHOWN);

  const set = (next: string[]) => onChange(joinRecipients(next));
  const add = (email: string) => {
    if (!chosen.includes(email)) set([...chosen, email]);
    // Whatever was typed was only narrowing the suggestions.
    setDraft("");
    setError("");
  };

  const commitDraft = (): boolean => {
    const typed = splitRecipients(draft);
    if (typed.length === 0) {
      setError("");
      return true;
    }
    const bad = typed.find((email) => !isValidEmail(email));
    if (bad) {
      setError(describeEmailError(bad));
      return false;
    }
    set([...chosen, ...typed.filter((email) => !chosen.includes(email))]);
    rememberRecipients(typed);
    setRemembered(rememberedRecipients());
    setDraft("");
    setError("");
    return true;
  };

  return (
    <div role="group" aria-label={field.label} className="grid gap-1.5 sm:col-span-2">
      <label
        htmlFor={inputId}
        className="px-0.5 text-[0.72rem] font-medium text-[var(--color-text-secondary)]"
      >
        {field.label}
        {field.required ? <span className="ml-0.5 text-[var(--color-accent)]">*</span> : null}
      </label>

      {chosen.length > 0 ? (
        <ul className="m-0 flex list-none flex-wrap gap-1.5 p-0" data-testid="recipient-chosen">
          {chosen.map((email) => (
            <li
              key={email}
              className="inline-flex max-w-full items-center gap-1 rounded-full border border-[var(--color-accent)] bg-[var(--color-surface-2)] py-0.5 pl-2.5 pr-1 text-[0.78rem] text-[var(--color-text-primary)]"
            >
              <span className="truncate">{email}</span>
              <button
                type="button"
                aria-label={tr("Remove {0}", [email])}
                className="inline-flex size-5 items-center justify-center rounded-full text-[var(--color-text-muted)] hover:text-[var(--color-text-primary)]"
                onClick={() => set(chosen.filter((e) => e !== email))}
              >
                <Icon name="close" className="size-3" />
              </button>
            </li>
          ))}
        </ul>
      ) : null}

      <div className="flex items-center gap-2">
        <Input
          id={inputId}
          type="email"
          autoComplete="off"
          maxLength={254}
          aria-invalid={error ? true : undefined}
          className="h-9 bg-[var(--color-bg)] px-2.5 py-2 text-[0.85rem]"
          placeholder={tr("Add an address")}
          value={draft}
          onChange={(e) => {
            setDraft(e.target.value);
            if (error) setError("");
          }}
          onKeyDown={(e) => {
            if (e.key === "Enter" || e.key === ",") {
              e.preventDefault();
              commitDraft();
            } else if (e.key === "Backspace" && draft === "" && chosen.length > 0) {
              set(chosen.slice(0, -1));
            }
          }}
          onBlur={() => {
            // Leaving the box adds a finished address. A fragment stays as
            // typed without a complaint: it may be narrowing the suggestions,
            // and the click that follows is what takes focus away.
            if (splitRecipients(draft).every(isValidEmail)) commitDraft();
          }}
        />
        <button
          type="button"
          className="btn btn-secondary btn-small shrink-0"
          disabled={draft.trim() === ""}
          onClick={commitDraft}
        >
          {tr("Add")}
        </button>
      </div>

      {error ? (
        <span role="alert" className="px-0.5 text-[0.7rem] text-[var(--color-danger)]">
          {error}
        </span>
      ) : null}

      {visible.length > 0 ? (
        <div className="grid gap-1">
          <span className="px-0.5 text-[0.66rem] font-semibold uppercase tracking-[0.1em] text-[var(--color-text-muted)]">
            {tr("Frequently used")}
          </span>
          <div className="flex flex-wrap gap-1.5" data-testid="recipient-suggestions">
            {visible.map((email) => (
              <button
                key={email}
                type="button"
                title={tr("Add {0}", [email])}
                className="inline-flex max-w-full items-center gap-1 rounded-full border border-[var(--color-border)] bg-[var(--color-surface-2)] px-2.5 py-0.5 text-[0.78rem] text-[var(--color-text-secondary)] hover:border-[var(--color-border-strong)] hover:text-[var(--color-text-primary)]"
                onClick={() => add(email)}
              >
                <Icon name="plus" className="size-3 shrink-0" />
                <span className="truncate">{email}</span>
              </button>
            ))}
            {suggestions.length > SUGGESTIONS_SHOWN ? (
              <button
                type="button"
                className="px-1 text-[0.75rem] text-[var(--color-text-muted)] underline hover:text-[var(--color-text-primary)]"
                onClick={() => setShowAll((v) => !v)}
              >
                {showAll ? tr("Show fewer") : tr("Show all {0}", [suggestions.length])}
              </button>
            ) : null}
          </div>
        </div>
      ) : null}

      {field.hint ? (
        <span className="px-0.5 text-[0.7rem] text-[var(--color-text-muted)]">{field.hint}</span>
      ) : null}
    </div>
  );
}
