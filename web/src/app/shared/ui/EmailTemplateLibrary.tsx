import { t as tr } from "@/app/lib/i18n";
import { useEffect, useMemo, useRef, useState, type RefObject } from "react";
import { createPortal } from "react-dom";
import { CloseButton } from "./CloseButton";
import { Icon } from "./Icon";
import { orchestratorApi, type EmailTemplate } from "@/app/shared/lib/orchestratorApi";
import { blockLabel, limitPhrases, neverSayLabel } from "@/app/shared/lib/emailTemplateRules";
import { PromptLibraryForm, type FormPromptItem } from "./PromptLibraryForm";
import { previewRecolorTable, recolorPreview, type PreviewStyles } from "@/app/shared/lib/emailPreviewStyle";
import type { PillValues } from "@/app/chat/ui/protocolPills";
import { useDialogA11y } from "./useDialogA11y";
import { errorMessage } from "@/app/shared/lib/errorMessage";

// The Email templates library (docs/EMAIL-TEMPLATES.md): the client bundle's
// templates for emails a team sends again and again. Each one is shown as the
// email it produces (the bundle's own rendered sample) beside the rules for
// every section, because the two together are what answer "what will this
// look like, and how long may it be?".
//
// Like the Prompt library it sends nothing: "Use this template" opens the
// template's form (when it has one) and "Use prompt" puts a prompt naming
// the template in the draft. The template is enforced later, by the bundle's
// mail connector when the assistant renders the email; fleet only shows it.
//
// The same full-bleed sheet shape as PromptLibrary (see the note there), a
// little wider so the preview and the rules fit side by side.

type Props = {
  onInsert: (content: string) => void;
  // The trigger lives in the composer's overflow menu; the dialog stays
  // mounted here so it survives that menu closing.
  renderTrigger: (openLibrary: () => void) => React.ReactNode;
  returnFocusRef?: RefObject<HTMLElement | null>;
};

const FIELD =
  "w-full rounded-md border border-[var(--color-border)] bg-[var(--color-surface-2)] px-3 py-2 text-sm text-[var(--color-text-primary)] outline-none placeholder:text-[var(--color-text-muted)] focus:border-[var(--color-accent)]";
const KICKER = "m-0 text-[0.66rem] font-semibold uppercase tracking-[0.1em] text-[var(--color-text-muted)]";
const CHIP =
  "inline-flex items-center rounded-full border border-[var(--color-border)] bg-[var(--color-surface-2)] px-2 py-0.5 text-[0.7rem] text-[var(--color-text-secondary)]";

/** What "Use prompt" inserts for a template that declares no form. */
export function defaultTemplatePrompt(template: EmailTemplate): string {
  return template.prompt_template?.trim()
    ? template.prompt_template
    : tr('Write the email with the email template "{0}". Show me the preview before anything is sent.', [template.id]);
}

/** What "New template" puts in the composer: a request the person finishes
 *  and sends. The assistant writes the template from an email in the chat and
 *  saves it through the bundle's mail connector, which asks for approval. */
export function newTemplatePrompt(): string {
  return tr(
    "Save an email as a team email template, so it comes out the same way next time.\nName: \nUse it when: \nBase it on: the email we just built in this chat\nTake the sections, their order and their length from that email, check the template, then save it for my approval.",
  );
}

/** Where a template comes from, for the line under its name. */
export function templateProvenance(template: EmailTemplate): string {
  if (template.source !== "team") return tr("Tracked in {0}", [template.path]);
  const day = template.created_at ? new Date(template.created_at) : null;
  const date = day && !Number.isNaN(day.getTime()) ? day.toLocaleDateString() : "";
  if (template.created_by && date) return tr("Added by {0} on {1}", [template.created_by, date]);
  if (template.created_by) return tr("Added by {0}", [template.created_by]);
  return tr("Added by your team");
}

// Whether this workspace has any email template, the bundle's or the team's.
// A workspace without them (the generic bundle, for a start) should not show a
// menu entry that opens an empty library, so the composer asks when its menu
// opens. Every composer on the page shares a "yes"; a "no" is asked again on
// the next open. On a workspace with none yet, the first template is added by
// asking the assistant in chat, and the entry then appears.
let availability: Promise<boolean> | null = null;

export function resetEmailTemplateAvailabilityForTests() {
  availability = null;
}

export function useHasEmailTemplates(ask: boolean): boolean {
  const [has, setHas] = useState(false);
  useEffect(() => {
    if (!ask) return;
    const check = (availability ??= orchestratorApi
      .emailTemplates()
      .then((list) => {
        // Only "yes" is kept. An empty library is asked about again the next
        // time the menu opens, so the entry appears as soon as a team saves
        // its first template from chat, without a reload.
        if (list.length === 0 && availability === check) availability = null;
        return list.length > 0;
      })
      .catch(() => {
        // Not cached: a failed check is asked again on the next open.
        if (availability === check) availability = null;
        return false;
      }));
    let live = true;
    void check.then((value) => {
      if (live) setHas(value);
    });
    return () => {
      live = false;
    };
  }, [ask]);
  return has;
}

function asFormPrompt(template: EmailTemplate): FormPromptItem | null {
  if (!template.fields?.length || !template.prompt_template?.trim()) return null;
  return {
    id: template.id,
    name: template.name,
    content: "",
    source: "git",
    visibility: "workspace",
    read_only: true,
    owned_by_caller: false,
    fields: template.fields,
    prompt_template: template.prompt_template,
  };
}

function audienceLabel(audience: string | undefined): string | null {
  if (audience === "client") return tr("For clients");
  if (audience === "internal") return tr("Internal only");
  return null;
}

export function EmailTemplateLibrary({ onInsert, renderTrigger, returnFocusRef }: Props) {
  const [open, setOpen] = useState(false);
  const [items, setItems] = useState<EmailTemplate[]>([]);
  const [selectedID, setSelectedID] = useState<string | null>(null);
  const [query, setQuery] = useState("");
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  // The rules are what the library is for, so they are what a template opens
  // on; its form takes their place only once someone chooses to use it.
  const [using, setUsing] = useState(false);
  // Previews by template id, read one at a time for the template on screen:
  // the list never carries them, so opening the menu or the library does not
  // download every example. null = the read failed or found none.
  const [previews, setPreviews] = useState<Record<string, string | null>>({});
  // The recolor table that came with each preview, and what the open form
  // holds, so the example shows the style being picked.
  const [previewStyles, setPreviewStyles] = useState<Record<string, PreviewStyles | null>>({});
  const [formValues, setFormValues] = useState<PillValues | null>(null);
  const modalRef = useRef<HTMLDivElement | null>(null);
  const searchRef = useRef<HTMLInputElement | null>(null);

  useDialogA11y(open, modalRef, () => setOpen(false), {
    initialFocusRef: searchRef,
    returnFocusRef,
  });

  const load = async () => {
    setLoading(true);
    setError(null);
    try {
      const next = await orchestratorApi.emailTemplates();
      setItems(next);
      // A reopened library reads its examples again: the bundle may have changed.
      setPreviews({});
      setPreviewStyles({});
      setSelectedID((id) => (id && next.some((t) => t.id === id) ? id : (next[0]?.id ?? null)));
    } catch (err) {
      setError(errorMessage(err, tr("Could not load email templates")));
    } finally {
      setLoading(false);
    }
  };

  const visible = useMemo(() => {
    const q = query.trim().toLowerCase();
    if (!q) return items;
    return items.filter((t) =>
      `${t.name}\n${t.description ?? ""}\n${t.use_when ?? ""}\n${t.id}`.toLowerCase().includes(q),
    );
  }, [items, query]);
  const selected = items.find((t) => t.id === selectedID) ?? null;
  const form = selected ? asFormPrompt(selected) : null;

  const wantPreview = open && selected?.has_preview ? selected.id : null;
  useEffect(() => {
    if (!wantPreview || wantPreview in previews) return;
    let live = true;
    void orchestratorApi
      .emailTemplate(wantPreview)
      .then((t) => ({ html: t.preview_html || null, styles: t.preview_styles ?? null }))
      .catch(() => ({ html: null, styles: null }))
      .then(({ html, styles }) => {
        if (!live) return;
        setPreviewStyles((prev) => ({ ...prev, [wantPreview]: styles }));
        setPreviews((prev) => ({ ...prev, [wantPreview]: html }));
      });
    return () => {
      live = false;
    };
  }, [wantPreview, previews]);
  const basePreview = selected ? previews[selected.id] : undefined;
  // The form's values count only while it is open on this template.
  const styled = using && selected ? formValues : null;
  const styles = selected ? previewStyles[selected.id] : null;
  const preview = useMemo(
    () =>
      basePreview
        ? recolorPreview(basePreview, previewRecolorTable(styles, selected?.fields, styled))
        : basePreview,
    [basePreview, styles, selected?.fields, styled],
  );

  const use = (text: string) => {
    onInsert(text);
    setUsing(false);
    setOpen(false);
  };

  const openLibrary = () => {
    setOpen(true);
    void load();
  };

  return (
    <>
      {renderTrigger(openLibrary)}
      {open
        ? createPortal(
            <div
              className="fixed inset-0 z-[1200] flex items-center justify-center bg-[var(--color-overlay-strong)] p-0 sm:p-4"
              role="dialog"
              aria-modal="true"
              aria-label={tr("Email templates")}
            >
              <div
                ref={modalRef}
                className="flex h-[100dvh] w-full flex-col overflow-hidden border border-[var(--color-border-strong)] bg-[var(--color-surface-1)] shadow-2xl sm:h-[min(52rem,94vh)] sm:w-[min(88rem,97vw)] sm:rounded-xl"
              >
                <header className="flex items-center gap-3 border-b border-[var(--color-border)] bg-[var(--gradient-surface-panel)] px-4 py-3">
                  <span
                    aria-hidden="true"
                    className="flex h-9 w-9 shrink-0 items-center justify-center rounded-[var(--radius-md)] bg-[color-mix(in_srgb,var(--color-accent)_16%,transparent)] text-[var(--color-accent)]"
                  >
                    <Icon name="mail" className="size-4.5" />
                  </span>
                  <div className="min-w-0 flex-1">
                    <h2 className="m-0 truncate text-base font-semibold text-[var(--color-text-primary)]">
                      {tr("Email templates")}
                    </h2>
                    <p className="m-0 truncate text-xs text-[var(--color-text-muted)]">
                      {tr("The emails we send every week, with the same sections and length each time")}
                    </p>
                  </div>
                  <CloseButton label={tr("Close email templates")} onClick={() => setOpen(false)} />
                </header>

                {error ? (
                  <div className="mx-4 mt-3 rounded-md bg-[color-mix(in_srgb,var(--color-danger)_12%,transparent)] px-3 py-2 text-sm text-[var(--color-danger)]">
                    {error}
                  </div>
                ) : null}

                <div className="grid min-h-0 flex-1 grid-cols-1 grid-rows-[minmax(9rem,30%)_minmax(0,1fr)] md:grid-cols-[17rem_minmax(0,1fr)] md:grid-rows-1">
                  <aside className="flex min-h-0 flex-col border-b border-[var(--color-border)] p-3 md:border-b-0 md:border-r">
                    <input
                      ref={searchRef}
                      type="search"
                      aria-label={tr("Search email templates")}
                      placeholder={tr("Search templates…")}
                      className={`${FIELD} mb-2`}
                      value={query}
                      onChange={(e) => setQuery(e.target.value)}
                    />
                    <div className="min-h-0 flex-1 overflow-y-auto">
                      {loading ? <p className="p-2 text-sm text-[var(--color-text-muted)]">{tr("Loading…")}</p> : null}
                      {!loading && !error && items.length === 0 ? (
                        <p className="p-2 text-sm text-[var(--color-text-muted)]">
                          {tr("This workspace has no email templates yet.")}
                        </p>
                      ) : null}
                      {!loading && items.length > 0 && visible.length === 0 ? (
                        <p className="p-2 text-sm text-[var(--color-text-muted)]">{tr("No matching templates.")}</p>
                      ) : null}
                      {visible.map((t) => (
                        <button
                          key={t.id}
                          type="button"
                          aria-current={selectedID === t.id ? "true" : undefined}
                          className={`mb-1 flex w-full items-start gap-2.5 rounded-lg border px-3 py-2 text-left transition ${selectedID === t.id ? "border-[var(--color-accent)] bg-[color-mix(in_srgb,var(--color-accent)_10%,transparent)]" : "border-transparent hover:bg-[var(--color-surface-2)]"}`}
                          onClick={() => {
                            setSelectedID(t.id);
                            setUsing(false);
                          }}
                        >
                          <span
                            aria-hidden="true"
                            className={`mt-0.5 flex h-6 w-6 shrink-0 items-center justify-center rounded-md ${t.audience === "internal" ? "bg-[var(--color-overlay-soft)] text-[var(--color-text-muted)]" : "bg-[color-mix(in_srgb,var(--color-accent)_16%,transparent)] text-[var(--color-accent)]"}`}
                          >
                            <Icon name={t.audience === "internal" ? "briefcase" : "mail"} className="size-3.5" />
                          </span>
                          <span className="grid min-w-0 gap-0.5">
                            <span className="flex flex-wrap items-center gap-1.5 text-sm font-medium text-[var(--color-text-primary)]">
                              {t.name}
                              {t.source === "team" ? <span className={CHIP}>{tr("Team")}</span> : null}
                            </span>
                            {t.description ? (
                              <span className="line-clamp-2 text-xs text-[var(--color-text-muted)]">{t.description}</span>
                            ) : null}
                          </span>
                        </button>
                      ))}
                    </div>
                    {!loading && !error ? (
                      <div className="mt-2 grid gap-1 border-t border-[var(--color-border)] pt-2">
                        <button
                          type="button"
                          className="btn btn-secondary w-full justify-center"
                          onClick={() => use(newTemplatePrompt())}
                        >
                          <Icon name="plus" className="mr-1.5 size-3.5" />
                          {tr("New template")}
                        </button>
                        <span className="px-0.5 text-[0.7rem] text-[var(--color-text-muted)]">
                          {tr("Puts a request in your draft. The assistant builds the template from an email in the chat and asks you to approve it.")}
                        </span>
                      </div>
                    ) : null}
                  </aside>

                  {selected ? (
                    <main
                      key={selected.id}
                      className="grid min-h-0 grid-cols-1 overflow-y-auto xl:grid-cols-[minmax(0,1fr)_26rem] xl:overflow-hidden"
                    >
                      <section
                        aria-label={tr("Example email")}
                        className="flex min-h-[26rem] flex-col border-b border-[var(--color-border)] p-4 xl:min-h-0 xl:border-b-0 xl:border-r"
                      >
                        <div className="mb-2 flex flex-wrap items-baseline justify-between gap-2">
                          <h3 className={KICKER}>{tr("Example email")}</h3>
                          <span className="text-xs text-[var(--color-text-muted)]">
                            {tr("Sample data. Your email uses the campaign's real numbers.")}
                          </span>
                        </div>
                        {selected.has_preview && preview === undefined ? (
                          <p className="m-0 p-2 text-sm text-[var(--color-text-muted)]">{tr("Loading…")}</p>
                        ) : preview ? (
                          <iframe
                            title={tr("Example of {0}", [selected.name])}
                            /* The bundle's own rendered sample. An empty sandbox:
                               no scripts, no same-origin, no navigation. */
                            sandbox=""
                            srcDoc={preview}
                            className="min-h-0 w-full flex-1 rounded-lg border border-[var(--color-border)] bg-white"
                          />
                        ) : (
                          <p className="m-0 rounded-lg border border-dashed border-[var(--color-border)] p-4 text-sm text-[var(--color-text-muted)]">
                            {tr("This template has no example to show. Its sections and rules are listed beside it.")}
                          </p>
                        )}
                      </section>

                      <section className="min-h-0 p-4 xl:overflow-y-auto">
                        <div className="flex flex-wrap items-center gap-2">
                          <h3 className="m-0 text-lg font-semibold text-[var(--color-text-primary)]">{selected.name}</h3>
                          {audienceLabel(selected.audience) ? (
                            <span className={CHIP}>{audienceLabel(selected.audience)}</span>
                          ) : null}
                          {selected.source === "team" ? <span className={CHIP}>{tr("Team")}</span> : null}
                        </div>
                        {selected.use_when ? (
                          <p className="mt-1 text-sm text-[var(--color-text-secondary)]">
                            <span className="font-medium text-[var(--color-text-primary)]">{tr("Use it when:")}</span>{" "}
                            {selected.use_when}
                          </p>
                        ) : selected.description ? (
                          <p className="mt-1 text-sm text-[var(--color-text-secondary)]">{selected.description}</p>
                        ) : null}

                        {using && form ? (
                          <>
                            <button
                              type="button"
                              className="btn btn-secondary btn-small mt-3"
                              onClick={() => setUsing(false)}
                            >
                              {tr("Back to the rules")}
                            </button>
                            <PromptLibraryForm
                              prompt={form}
                              note={templateProvenance(selected)}
                              onUse={use}
                              onValuesChange={setFormValues}
                            />
                          </>
                        ) : (
                          <>
                            <div className="mt-3 flex flex-wrap items-center justify-between gap-3">
                              <span className="text-xs text-[var(--color-text-muted)]">
                                {templateProvenance(selected)}
                              </span>
                              <button
                                type="button"
                                className="btn btn-primary"
                                onClick={() => {
                                  if (!form) return use(defaultTemplatePrompt(selected));
                                  // The form reports its own values once it is open.
                                  setFormValues(null);
                                  setUsing(true);
                                }}
                              >
                                <Icon name="arrow-up" className="mr-1.5 size-3.5 rotate-90" />
                                {tr("Use this template")}
                              </button>
                            </div>

                        <h4 className={`${KICKER} mt-5`}>{tr("Sections, in order")}</h4>
                        <ol className="m-0 mt-2 grid list-none gap-2 p-0" data-testid="email-template-sections">
                          {selected.sections.map((s, i) => (
                            <li
                              key={s.id}
                              className="rounded-lg border border-[var(--color-border)] bg-[var(--color-surface-2)] p-3"
                            >
                              <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
                                <span className="text-sm font-semibold text-[var(--color-text-primary)]">
                                  {i + 1}. {s.title}
                                </span>
                                <span className={CHIP}>{blockLabel(s.block)}</span>
                                {!s.required ? <span className={CHIP}>{tr("Optional")}</span> : null}
                              </div>
                              {s.instruction ? (
                                <p className="m-0 mt-1.5 text-xs leading-relaxed text-[var(--color-text-secondary)]">
                                  {s.instruction}
                                </p>
                              ) : null}
                              {limitPhrases(s.limits).length > 0 ? (
                                <p className="m-0 mt-1.5 text-xs font-medium text-[var(--color-text-primary)]">
                                  {limitPhrases(s.limits).join(" · ")}
                                </p>
                              ) : null}
                            </li>
                          ))}
                        </ol>

                        <h4 className={`${KICKER} mt-5`}>{tr("Whole email")}</h4>
                        <ul className="m-0 mt-2 grid list-disc gap-1 pl-5 text-xs leading-relaxed text-[var(--color-text-secondary)]">
                          {selected.max_words ? (
                            <li>{tr("Up to {0} words of prose. Numbers in cards and tables are not counted.", [selected.max_words])}</li>
                          ) : null}
                          {selected.subject.instruction ? (
                            <li>
                              {selected.subject.max_chars
                                ? tr("Subject, up to {0} characters: {1}", [selected.subject.max_chars, selected.subject.instruction])
                                : tr("Subject: {0}", [selected.subject.instruction])}
                            </li>
                          ) : null}
                          {(selected.rules ?? []).map((rule) => (
                            <li key={rule}>{rule}</li>
                          ))}
                        </ul>

                        {(selected.never_say ?? []).length > 0 ? (
                          <>
                            <h4 className={`${KICKER} mt-5`}>{tr("Never says")}</h4>
                            <p className="m-0 mt-2 flex flex-wrap gap-1.5">
                              {(selected.never_say ?? []).map((entry) =>
                                neverSayLabel(entry) ? (
                                  <span key={entry} className={CHIP}>
                                    {neverSayLabel(entry)}
                                  </span>
                                ) : null,
                              )}
                              {(selected.never_say ?? []).some((entry) => !neverSayLabel(entry)) ? (
                                <span className={CHIP}>{tr("internal codes and IDs")}</span>
                              ) : null}
                            </p>
                          </>
                        ) : null}

                          </>
                        )}

                        <p className="mt-5 text-xs text-[var(--color-text-muted)]">
                          {tr("An email that breaks these rules is not built: the assistant is told what to fix and tries again.")}
                        </p>
                      </section>
                    </main>
                  ) : (
                    <main className="p-4 text-sm text-[var(--color-text-muted)]">
                      {loading || error || items.length === 0 ? null : tr("Select a template.")}
                    </main>
                  )}
                </div>
              </div>
            </div>,
            document.body,
          )
        : null}
    </>
  );
}
