import { useEffect, useImperativeHandle, useMemo, useRef, useState, type Ref } from "react";
import { orchestratorApi, type RecipientUse } from "@/app/shared/lib/orchestratorApi";
import { describeEmailError } from "@/app/shared/lib/validation";
import { isValidEmail, plural } from "@/app/shared/lib/format";
import { rankRecipientSuggestions } from "./recipientSuggestions";

// RecipientsField — the New Task form's "Email results" chip field: committed
// addresses as removable chips, a text input for the next one, and a combobox
// over the addresses other tasks already mail. Moved out of TaskCreateModal
// (#2034); the markup, focus and Escape behaviour are unchanged.
//
// The modal owns the state it resets and dirty-checks (the list, the
// half-typed draft, the validation error); the field owns the suggestion list.
// submit reads the list through the imperative commit(): it folds a typed but
// uncommitted address into the list and RETURNS the result, so the caller
// never sends the list from before a setState it has not seen yet (a typed
// address submitted without leaving the field used to be dropped).

export type RecipientsFieldHandle = {
  /**
   * Commit the draft. Returns the recipient list including it, or null when
   * the draft is not a valid address (the field then shows why).
   */
  commit: () => string[] | null;
  focus: () => void;
};

export function RecipientsField({
  value,
  onChange,
  draft,
  onDraftChange,
  error,
  onErrorChange,
  ref,
}: {
  value: string[];
  onChange: (next: string[]) => void;
  draft: string;
  onDraftChange: (next: string) => void;
  error: string;
  onErrorChange: (next: string) => void;
  ref?: Ref<RecipientsFieldHandle>;
}) {
  const inputRef = useRef<HTMLInputElement | null>(null);
  // Remembered recipients: every address a visible task already mails
  // (GET /tasks/recipients), offered as the field's suggestions. Optional —
  // a failed fetch leaves plain typing exactly as it was.
  const [recipientHistory, setRecipientHistory] = useState<RecipientUse[]>([]);
  const [suggestOpen, setSuggestOpen] = useState(false);
  const [suggestIndex, setSuggestIndex] = useState(-1);

  // Load the remembered recipients each time the form opens (the field mounts
  // with the open form), so an address a task started using since the last
  // open is offered too. The call goes through a resolved promise so a
  // missing or throwing client is caught.
  useEffect(() => {
    let cancelled = false;
    Promise.resolve()
      .then(() => orchestratorApi.recipientHistory())
      .then((rows) => {
        if (!cancelled && Array.isArray(rows)) setRecipientHistory(rows);
      })
      .catch(() => {
        // Suggestions are a convenience; typing addresses still works.
      });
    return () => {
      cancelled = true;
    };
  }, []);

  const recipientSuggestions = useMemo(
    () => rankRecipientSuggestions(recipientHistory, draft, value),
    [recipientHistory, draft, value],
  );
  const suggestionsShown = suggestOpen && recipientSuggestions.length > 0;
  const activeSuggestion =
    suggestionsShown && suggestIndex >= 0 && suggestIndex < recipientSuggestions.length
      ? suggestIndex
      : -1;

  const pickSuggestion = (email: string) => {
    if (!value.includes(email)) onChange([...value, email]);
    onDraftChange("");
    onErrorChange("");
    setSuggestIndex(-1);
    inputRef.current?.focus();
  };

  const commit = (): string[] | null => {
    const raw = draft.trim().toLowerCase();
    if (!raw) {
      onErrorChange("");
      return value;
    }
    if (!isValidEmail(raw)) {
      onErrorChange(describeEmailError(raw));
      return null;
    }
    const next = value.includes(raw) ? value : [...value, raw];
    if (next !== value) onChange(next);
    onDraftChange("");
    onErrorChange("");
    return next;
  };

  useImperativeHandle(ref, () => ({
    commit,
    focus: () => inputRef.current?.focus(),
  }));

  // The group boundary lives on the section — it wraps the field's label AND
  // its chips, which is what a screen-reader user is actually being told to
  // treat as one thing. It used to sit on the chip box below, which forced
  // that box to be both a group and a click target; a group is not a widget,
  // so the handler had no keyboard equivalent to offer.
  return (
      <section className="task-group" role="group" aria-label="Email recipients">
        <div className="task-label-row">
          <label className="task-label" htmlFor="emailChipInput">
            Email results
          </label>
        </div>
        {/* The chip box is a painted surrogate for the text field
            (cursor: text, focus ring via :focus-within) — presentational,
            with the real control being the input inside it. Its click
            handler only forwards a click on the box's padding to that
            input, which a keyboard user reaches by Tab anyway, so there
            is no keyboard behavior missing here to add. */}
        {/* The anchor positions the suggestion list under the chip box
            without pushing the rest of the form down. */}
        <div className="task-suggest-anchor">
          <div
            className={`task-chipfield${error ? " has-error" : ""}`}
            onClick={() => inputRef.current?.focus()}
            role="presentation"
          >
            {value.map((e) => (
              <span key={e} className="task-chip" data-email={e}>
                {e}
                <button
                  type="button"
                  className="task-chip-delete"
                  aria-label={`Remove ${e}`}
                  onClick={() => onChange(value.filter((x) => x !== e))}
                >
                  <svg
                    width="10"
                    height="10"
                    viewBox="0 0 24 24"
                    fill="none"
                    stroke="currentColor"
                    strokeWidth="2.4"
                    strokeLinecap="round"
                    aria-hidden="true"
                  >
                    <path d="M18 6L6 18M6 6l12 12" />
                  </svg>
                </button>
              </span>
            ))}
            {/* A combobox over the remembered recipients. autoComplete is
                off because the browser's own autofill offered unrelated
                personal addresses over the list. */}
            <input
              id="emailChipInput"
              ref={inputRef}
              type="email"
              className="task-chipfield-input"
              placeholder={value.length === 0 ? "Add email — press Enter" : "Add email…"}
              maxLength={254}
              autoComplete="off"
              role="combobox"
              aria-autocomplete="list"
              aria-expanded={suggestionsShown}
              aria-controls="email-suggestions"
              aria-activedescendant={
                activeSuggestion >= 0 ? `email-suggestion-${activeSuggestion}` : undefined
              }
              aria-describedby={error ? "email-error email-help" : "email-help"}
              value={draft}
              onFocus={() => setSuggestOpen(true)}
              onChange={(e) => {
                onDraftChange(e.target.value);
                setSuggestOpen(true);
                setSuggestIndex(-1);
                if (error) onErrorChange("");
              }}
              onKeyDown={(e) => {
                if (e.key === "ArrowDown" || e.key === "ArrowUp") {
                  if (recipientSuggestions.length === 0) return;
                  e.preventDefault();
                  const n = recipientSuggestions.length;
                  setSuggestOpen(true);
                  setSuggestIndex((i) =>
                    e.key === "ArrowDown" ? (i + 1) % n : i <= 0 ? n - 1 : i - 1,
                  );
                } else if (e.key === "Escape" && suggestionsShown) {
                  // The dialog leaves Escape to an expanded combobox
                  // (useDialogA11y), so this closes only the list.
                  e.preventDefault();
                  setSuggestOpen(false);
                  setSuggestIndex(-1);
                } else if (e.key === "Enter" && activeSuggestion >= 0) {
                  e.preventDefault();
                  pickSuggestion(recipientSuggestions[activeSuggestion].email);
                } else if (e.key === "Enter" || e.key === ",") {
                  e.preventDefault();
                  commit();
                } else if (e.key === "Backspace" && draft === "" && value.length > 0) {
                  onChange(value.slice(0, -1));
                }
              }}
              onBlur={() => {
                setSuggestOpen(false);
                setSuggestIndex(-1);
                if (draft.trim()) commit();
              }}
            />
          </div>
          {suggestionsShown ? (
            <div
              id="email-suggestions"
              className="task-suggest"
              role="listbox"
              aria-label="Recipients used before"
              data-testid="email-suggestions"
            >
              {recipientSuggestions.map((r, i) => (
                <button
                  key={r.email}
                  id={`email-suggestion-${i}`}
                  type="button"
                  role="option"
                  tabIndex={-1}
                  aria-selected={i === activeSuggestion}
                  className={`task-suggest-option${i === activeSuggestion ? " is-active" : ""}`}
                  // Same as ModelPicker: mousedown is swallowed so the
                  // input keeps focus (a blur would commit the typed
                  // fragment first), and the click picks.
                  onMouseDown={(e) => e.preventDefault()}
                  onClick={() => pickSuggestion(r.email)}
                  onMouseEnter={() => setSuggestIndex(i)}
                >
                  <span className="task-suggest-email">{r.email}</span>
                  <span className="task-suggest-meta">
                    {plural(r.task_count, "task")}
                  </span>
                </button>
              ))}
            </div>
          ) : null}
        </div>
        {error ? (
          <div className="task-inline-error" id="email-error" data-testid="error-email">
            <svg
              width="13"
              height="13"
              viewBox="0 0 24 24"
              fill="none"
              stroke="currentColor"
              strokeWidth="2"
              strokeLinecap="round"
              aria-hidden="true"
            >
              <circle cx="12" cy="12" r="9" />
              <path d="M12 8v4M12 15.5v.5" />
            </svg>
            {error}
          </div>
        ) : (
          <p className="field-hint" id="email-help">
            The agent is instructed to email its report to these recipients after each run.
          </p>
        )}
      </section>
  );
}
