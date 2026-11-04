"use client";

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { navigate } from "@/app/lib/navigation";
import {
  orchestratorApi,
  type PromptLibraryItem,
  type PromptLibraryWrite,
} from "@/app/shared/lib/orchestratorApi";
import {
  isFormPrompt,
  PromptLibraryForm,
} from "@/app/shared/ui/PromptLibraryForm";
import {
  btnClass,
  ClampText,
  ConnBadge,
  InlineConfirmButton,
  RevealButton,
  Segmented,
  SETTINGS_INPUT,
} from "../ui/atoms";
import {
  ConnEmpty,
  ConnField,
  ConnPanel,
  ConnPanelHead,
  ConnPanelSub,
  ConnRows,
  DirSearch,
  SetSection,
} from "../ui/panels";

const ROW =
  "grid gap-[0.45rem] border-b border-[var(--color-border-subtle)] px-[0.1rem] py-3 last:border-b-0";
const ROW_HEAD = "flex flex-wrap items-center justify-between gap-2";
const DESC = "text-[0.76rem]! leading-[1.55]!";
const MD =
  "mt-[0.15rem] max-h-[23rem] overflow-y-auto whitespace-pre-wrap [overflow-wrap:anywhere] rounded-[var(--radius-md)] border border-[var(--color-border)] bg-[var(--color-overlay-soft)] px-[0.95rem] py-[0.8rem] font-[family-name:var(--font-code)] text-[0.74rem] leading-[1.75] text-[var(--color-text-secondary)]";
const LOADING =
  "m-0 py-3 text-center text-[0.79rem] text-[var(--color-text-muted)]";

type DraftState = PromptLibraryWrite & {
  id: string;
};

const EMPTY_DRAFT: DraftState = {
  id: "",
  name: "",
  description: "",
  content: "",
  visibility: "private",
};

export default function PromptsPage() {
  const [items, setItems] = useState<PromptLibraryItem[] | null>(null);
  const [query, setQuery] = useState("");
  const [scope, setScope] = useState<"all" | "yours" | "workspace" | "deployment">("all");
  const [openId, setOpenId] = useState<string | null>(null);
  const [draft, setDraft] = useState<DraftState | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const editorRef = useRef<HTMLDivElement | null>(null);

  const loadPrompts = useCallback(async () => {
    try {
      const next = await orchestratorApi.prompts();
      setItems(next);
    } catch (err) {
      setError(
        err instanceof Error ? err.message : "Could not load prompt library.",
      );
      setItems((cur) => cur ?? []);
    }
  }, []);

  useEffect(() => {
    let cancelled = false;
    queueMicrotask(() => {
      if (!cancelled) void loadPrompts();
    });
    return () => {
      cancelled = true;
    };
  }, [loadPrompts]);

  const startNew = () => {
    setError(null);
    setNotice(null);
    setDraft(EMPTY_DRAFT);
  };

  const startEdit = (item: PromptLibraryItem) => {
    if (item.read_only || !item.owned_by_caller) return;
    setError(null);
    setNotice(null);
    setDraft({
      id: item.id,
      name: item.name,
      description: item.description ?? "",
      content: item.content,
      visibility: item.visibility,
    });
    queueMicrotask(() =>
      editorRef.current?.scrollIntoView?.({ behavior: "smooth", block: "nearest" }),
    );
  };

  const startClone = (item: PromptLibraryItem) => {
    setError(null);
    setNotice(null);
    setDraft({
      id: "",
      name:
        item.read_only || !item.owned_by_caller
          ? `${item.name} (Custom)`
          : `${item.name} (Copy)`,
      description: item.description ?? "",
      content: item.content,
      visibility: "private",
    });
    queueMicrotask(() =>
      editorRef.current?.scrollIntoView?.({ behavior: "smooth", block: "nearest" }),
    );
  };

  const handleSave = async () => {
    if (!draft || !draft.name.trim() || !draft.content.trim() || busy) return;
    setBusy(true);
    setError(null);
    setNotice(null);
    try {
      const payload: PromptLibraryWrite = {
        name: draft.name.trim(),
        description: draft.description?.trim() ?? "",
        content: draft.content,
        visibility: draft.visibility,
      };
      if (draft.id) {
        await orchestratorApi.updatePrompt(draft.id, payload);
        setNotice(`Saved changes to “${payload.name}”.`);
      } else {
        await orchestratorApi.createPrompt(payload);
        setNotice(`Created prompt “${payload.name}”.`);
      }
      setDraft(null);
      await loadPrompts();
    } catch (err) {
      setError(err instanceof Error ? err.message : "Could not save prompt.");
    } finally {
      setBusy(false);
    }
  };

  const handleDelete = async (item: PromptLibraryItem) => {
    if (item.read_only || !item.owned_by_caller) return;
    setError(null);
    setNotice(null);
    try {
      await orchestratorApi.deletePrompt(item.id);
      if (draft?.id === item.id) setDraft(null);
      setNotice(`Deleted “${item.name}”.`);
      await loadPrompts();
    } catch (err) {
      setError(err instanceof Error ? err.message : "Could not delete prompt.");
    }
  };

  const filtered = useMemo(() => {
    const all = items ?? [];
    const q = query.trim().toLowerCase();
    return all.filter((p) => {
      if (scope === "yours" && (!p.owned_by_caller || p.read_only)) return false;
      if (scope === "workspace" && (p.read_only || p.visibility !== "workspace"))
        return false;
      if (scope === "deployment" && !p.read_only) return false;
      if (!q) return true;
      return `${p.name}\n${p.description ?? ""}\n${p.path ?? ""}\n${p.content}`
        .toLowerCase()
        .includes(q);
    });
  }, [items, query, scope]);

  const editablePrompts = filtered.filter((p) => !p.read_only);
  const deploymentPrompts = filtered.filter((p) => p.read_only);

  return (
    <SetSection
      title="Prompts"
      intro="Reusable prompts and form templates shared between Chat and the Operations Center. Create personal or workspace prompts, customize deployment templates, or open any prompt directly in a new chat."
    >
      {error ? (
        <p
          role="alert"
          className="mb-3 rounded-[var(--radius-md)] border border-[color-mix(in_srgb,var(--color-danger)_45%,transparent)] bg-[color-mix(in_srgb,var(--color-danger)_10%,transparent)] px-3 py-2 text-[0.78rem] text-[var(--color-danger)]"
        >
          {error}
        </p>
      ) : null}
      {notice ? (
        <p
          role="status"
          className="mb-3 rounded-[var(--radius-md)] border border-[color-mix(in_srgb,var(--color-accent)_40%,transparent)] bg-[color-mix(in_srgb,var(--color-accent)_10%,transparent)] px-3 py-2 text-[0.78rem] text-[var(--color-text-primary)]"
        >
          {notice}
        </p>
      ) : null}

      <div className="mb-3 flex flex-wrap items-center justify-between gap-2">
        <Segmented
          label="Prompt filter"
          value={scope}
          options={[
            { value: "all", label: "All" },
            { value: "yours", label: "Yours" },
            { value: "workspace", label: "Workspace" },
            { value: "deployment", label: "Deployment" },
          ]}
          onChange={setScope}
        />
      </div>

      <DirSearch
        label="Search prompts"
        value={query}
        onChange={setQuery}
        placeholder="Search prompts by name, description, or content…"
      />

      {scope !== "deployment" ? (
        <ConnPanel>
          <div ref={editorRef}>
            <ConnPanelHead title="Your & workspace prompts">
              <RevealButton
                open={draft !== null}
                closedLabel="New prompt"
                onClick={() => (draft ? setDraft(null) : startNew())}
                testId="new-prompt-toggle"
              />
            </ConnPanelHead>
            <ConnPanelSub>
              Private prompts are visible only to you; workspace prompts are shared with everyone on this deployment.
            </ConnPanelSub>

            {draft ? (
              <div className="mb-4 grid gap-3 rounded-[var(--radius-md)] border border-[var(--color-border)] bg-[var(--color-overlay-soft)] p-3.5">
                <ConnField label="Name">
                  <input
                    aria-label="Prompt name"
                    placeholder="e.g. Morning deal pacing check"
                    value={draft.name}
                    onChange={(e) =>
                      setDraft({ ...draft, name: e.target.value })
                    }
                    className={SETTINGS_INPUT}
                  />
                </ConnField>
                <ConnField label="Description (optional — helps teammates find it)">
                  <input
                    aria-label="Prompt description"
                    placeholder="Short summary of what this prompt produces"
                    value={draft.description ?? ""}
                    onChange={(e) =>
                      setDraft({ ...draft, description: e.target.value })
                    }
                    className={SETTINGS_INPUT}
                  />
                </ConnField>
                <ConnField label="Prompt text">
                  <textarea
                    aria-label="Prompt text"
                    rows={7}
                    placeholder="Write the instructions or template…"
                    value={draft.content}
                    onChange={(e) =>
                      setDraft({ ...draft, content: e.target.value })
                    }
                    className={`${SETTINGS_INPUT} font-[family-name:var(--font-code)] text-[0.82rem]!`}
                  />
                </ConnField>
                <div className="flex flex-wrap items-center justify-between gap-2">
                  <label className="flex cursor-pointer items-center gap-2 text-[0.78rem] text-[var(--color-text-secondary)]">
                    <input
                      type="checkbox"
                      checked={draft.visibility === "workspace"}
                      onChange={(e) =>
                        setDraft({
                          ...draft,
                          visibility: e.target.checked ? "workspace" : "private",
                        })
                      }
                    />
                    <span>Share with this workspace</span>
                  </label>
                  <div className="flex items-center gap-2">
                    <button
                      type="button"
                      className={btnClass({ sm: true, reveal: true })}
                      onClick={() => setDraft(null)}
                    >
                      Cancel
                    </button>
                    <button
                      type="button"
                      disabled={
                        !draft.name.trim() || !draft.content.trim() || busy
                      }
                      onClick={() => void handleSave()}
                      className={btnClass({ variant: "primary", sm: true })}
                    >
                      {busy
                        ? "Saving…"
                        : draft.id
                          ? "Save changes"
                          : "Create prompt"}
                    </button>
                  </div>
                </div>
              </div>
            ) : null}

            {items === null ? (
              <p className={LOADING}>Loading prompts…</p>
            ) : editablePrompts.length === 0 ? (
              <ConnEmpty>
                {query
                  ? "No personal or workspace prompts match your search."
                  : "No personal or workspace prompts yet. Click New prompt above or save a workflow from any chat."}
              </ConnEmpty>
            ) : (
              <ConnRows>
                {editablePrompts.map((p) => {
                  const isOpen = openId === p.id;
                  return (
                    <div
                      key={p.id}
                      data-testid="settings-prompt-row"
                      className={ROW}
                    >
                      <div className={ROW_HEAD}>
                        <div className="flex flex-wrap items-center gap-2">
                          <span className="text-[0.86rem] font-semibold text-[var(--color-text-primary)]">
                            {p.name}
                          </span>
                          <ConnBadge
                            variant={
                              p.visibility === "workspace"
                                ? "overridden"
                                : "neutral"
                            }
                          >
                            {p.visibility === "workspace"
                              ? "Workspace"
                              : "Private"}
                          </ConnBadge>
                          {isFormPrompt(p) ? (
                            <ConnBadge variant="neutral">Form</ConnBadge>
                          ) : null}
                        </div>
                        <div className="flex flex-wrap items-center gap-1.5">
                          <button
                            type="button"
                            className={btnClass({
                              variant: "primary",
                              sm: true,
                            })}
                            onClick={() => {
                              if (isFormPrompt(p)) {
                                setOpenId(p.id);
                                return;
                              }
                              navigate(
                                `/chat?draft=${encodeURIComponent(p.content)}`,
                              );
                            }}
                          >
                            Open in chat
                          </button>
                          <button
                            type="button"
                            className={btnClass({ sm: true })}
                            onClick={() =>
                              setOpenId(isOpen ? null : p.id)
                            }
                          >
                            {isOpen ? "Hide" : "View"}
                          </button>
                          {p.owned_by_caller ? (
                            <>
                              <button
                                type="button"
                                className={btnClass({ sm: true })}
                                onClick={() => startEdit(p)}
                              >
                                Edit
                              </button>
                              <button
                                type="button"
                                className={btnClass({ sm: true })}
                                onClick={() => startClone(p)}
                              >
                                Duplicate
                              </button>
                              <InlineConfirmButton
                                label="Delete"
                                onConfirm={() => void handleDelete(p)}
                              />
                            </>
                          ) : (
                            <button
                              type="button"
                              className={btnClass({ sm: true })}
                              onClick={() => startClone(p)}
                            >
                              Clone & customize
                            </button>
                          )}
                        </div>
                      </div>
                      {p.description ? (
                        <ClampText text={p.description} className={DESC} />
                      ) : null}
                      {isOpen ? (
                        isFormPrompt(p) ? (
                          <div className="mt-[0.15rem] rounded-[var(--radius-md)] border border-[var(--color-border)] bg-[var(--color-overlay-soft)] p-3">
                            <PromptLibraryForm
                              key={p.id}
                              prompt={p}
                              note={
                                p.path
                                  ? `Tracked in ${p.path}`
                                  : "Fill out the form fields to open in chat"
                              }
                              onUse={(text) =>
                                navigate(
                                  `/chat?draft=${encodeURIComponent(text)}`,
                                )
                              }
                            />
                          </div>
                        ) : (
                          <pre className={MD}>{p.content}</pre>
                        )
                      ) : null}
                    </div>
                  );
                })}
              </ConnRows>
            )}
          </div>
        </ConnPanel>
      ) : null}

      {scope !== "yours" && scope !== "workspace" ? (
        <ConnPanel>
          <ConnPanelHead title="Deployment prompts" />
          <ConnPanelSub>
            Read-only prompts shipped with your deployment bundle. Open any prompt in chat or clone it to customize your own version.
          </ConnPanelSub>
          {items === null ? (
            <p className={LOADING}>Loading deployment prompts…</p>
          ) : deploymentPrompts.length === 0 ? (
            <ConnEmpty>
              {query
                ? "No deployment prompts match your search."
                : "No deployment prompts configured on this server."}
            </ConnEmpty>
          ) : (
            <ConnRows>
              {deploymentPrompts.map((p) => {
                const isOpen = openId === p.id;
                return (
                  <div
                    key={p.id}
                    data-testid="settings-deployment-prompt-row"
                    className={ROW}
                  >
                    <div className={ROW_HEAD}>
                      <div className="flex flex-wrap items-center gap-2">
                        <span className="text-[0.86rem] font-semibold text-[var(--color-text-primary)]">
                          {p.name}
                        </span>
                        <ConnBadge variant="neutral">Git</ConnBadge>
                        {isFormPrompt(p) ? (
                          <ConnBadge variant="neutral">Form</ConnBadge>
                        ) : null}
                      </div>
                      <div className="flex flex-wrap items-center gap-1.5">
                        <button
                          type="button"
                          className={btnClass({ variant: "primary", sm: true })}
                          onClick={() => {
                            if (isFormPrompt(p)) {
                              setOpenId(p.id);
                              return;
                            }
                            navigate(
                              `/chat?draft=${encodeURIComponent(p.content)}`,
                            );
                          }}
                        >
                          Open in chat
                        </button>
                        <button
                          type="button"
                          className={btnClass({ sm: true })}
                          onClick={() => setOpenId(isOpen ? null : p.id)}
                        >
                          {isOpen ? "Hide" : "View"}
                        </button>
                        <button
                          type="button"
                          className={btnClass({ sm: true })}
                          onClick={() => startClone(p)}
                        >
                          Clone & customize
                        </button>
                      </div>
                    </div>
                    {p.description ? (
                      <ClampText text={p.description} className={DESC} />
                    ) : null}
                    {isOpen ? (
                      isFormPrompt(p) ? (
                        <div className="mt-[0.15rem] rounded-[var(--radius-md)] border border-[var(--color-border)] bg-[var(--color-overlay-soft)] p-3">
                          <PromptLibraryForm
                            key={p.id}
                            prompt={p}
                            note={
                              p.path
                                ? `Tracked in ${p.path}`
                                : "Fill out the form fields to open in chat"
                            }
                            onUse={(text) =>
                              navigate(
                                `/chat?draft=${encodeURIComponent(text)}`,
                              )
                            }
                          />
                        </div>
                      ) : (
                        <pre className={MD}>{p.content}</pre>
                      )
                    ) : null}
                  </div>
                );
              })}
            </ConnRows>
          )}
        </ConnPanel>
      ) : null}
    </SetSection>
  );
}
