"use client";

import { useEffect, useMemo, useRef, useState } from "react";
import { createPortal } from "react-dom";
import { CloseButton } from "@/app/shared/ui/CloseButton";
import { Icon } from "@/app/shared/ui/Icon";
import { useDialogA11y } from "@/app/shared/ui/useDialogA11y";
import {
  categoryName,
  categoryOf,
  connectorLabel,
  connectorStatus,
  countOn,
  groupConnectors,
  inCategory,
  isOn,
  isTouchPrimary,
  isToggleable,
  LIBRARY_STATUSES,
  libraryBulkScope,
  matchAll,
  matchesStatus,
  statusKicker,
  statusText,
  toolCountLabel,
  type LibraryStatusFilter,
  type PickerCategory,
  type PickerConnector,
} from "./toolsPicker";
import { Highlight, SeatMenu, StatusDot, StatusIcon, StatusKicker, ToolSwitch } from "./ToolsPickerParts";

// The Tools library: every connection, opened from the popover's expand icon
// or its "Open library" link. Modelled on the Prompt library — the same
// portalled sheet, header and rail/list/detail split — with a search and
// category + status filters on the left, the list in the middle, and the
// selected connection on the right. The switch here is the same per-chat
// switch as the popover; which connections start on in new chats lives in
// Settings › Connections, linked from the detail pane.
//
// Like PromptLibrary it is not on DialogShell: it is the full-bleed sheet
// shape (fills the viewport below sm:) and traps focus via useDialogA11y.
//
// Below lg the list and the detail cannot sit side by side, so they take
// turns: tapping a connection opens its detail as its own view, with a back
// button (Escape goes back too). Below md the filters scroll sideways as one
// row of chips each, so the list starts near the top of a phone screen.

export type ToolsLibraryProps = {
  servers: PickerConnector[];
  categories: PickerCategory[];
  appName: string;
  isAdmin: boolean;
  /** Open on this connection (a dimmed popover row, "Set up ›"). */
  initialSelected?: string;
  onClose: () => void;
  onToggle: (name: string) => void;
  onSetEnabled: (names: string[], enabled: boolean) => void;
  onSetAccount: (name: string, account: string) => void;
};

// The rail's headings; below md the chips speak for themselves (each nav keeps
// its aria-label), so the headings are hidden to save two rows.
const RAIL_LABEL =
  "hidden px-2 pb-1.5 md:block font-[family-name:var(--font-code)] text-[0.6875rem] uppercase tracking-[0.12em] text-[var(--color-text-subtle)]";
// Below md each filter set is one sideways-scrolling row of chips, bled to the
// sheet's edges; from md they stack as the design's rail.
const RAIL_NAV =
  "-mx-2.5 flex gap-1.5 overflow-x-auto px-2.5 [scrollbar-width:none] md:mx-0 md:flex-col md:gap-0.5 md:overflow-visible md:px-0 [&::-webkit-scrollbar]:hidden";
// Below lg the list and detail stack; this query decides whether Escape steps
// back from the detail view before closing.
const STACKED = "(max-width: 1023.98px)";
const GROUP_LABEL =
  "px-3 pb-1 pt-3 font-[family-name:var(--font-code)] text-[0.6875rem] uppercase tracking-[0.12em] text-[var(--color-text-subtle)]";
const SMALL_PILL =
  "hit-area-touch h-6 shrink-0 whitespace-nowrap rounded-[var(--radius-pill)] border border-[var(--color-pill-border)] px-2.5 text-[0.75rem] text-[var(--color-text-secondary)] transition hover:bg-[var(--color-pill-hover-bg)] hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] aria-disabled:cursor-not-allowed aria-disabled:opacity-40";
const LINK =
  "inline-flex items-center gap-1 text-[var(--color-link)] hover:underline hover:underline-offset-[3px] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]";

const settingsHref = (name?: string) =>
  name ? `/settings/connections#connector-${encodeURIComponent(name)}` : "/settings/connections";

export function ToolsLibrary({
  servers,
  categories,
  appName,
  isAdmin,
  initialSelected,
  onClose,
  onToggle,
  onSetEnabled,
  onSetAccount,
}: ToolsLibraryProps) {
  const [query, setQuery] = useState("");
  const [cat, setCat] = useState<string>("all");
  const [status, setStatus] = useState<LibraryStatusFilter | null>(null);
  const [selected, setSelected] = useState<string | null>(initialSelected ?? null);
  const [toolsOpen, setToolsOpen] = useState(false);
  // The stacked layout's detail view. Opening on a connection ("Set up ›")
  // starts there; it has no effect from lg, where both panes show.
  const [detailOpen, setDetailOpen] = useState(!!initialSelected);
  const panelRef = useRef<HTMLDivElement | null>(null);
  const searchRef = useRef<HTMLInputElement | null>(null);
  const bodyRef = useRef<HTMLDivElement | null>(null);
  const backRef = useRef<HTMLButtonElement | null>(null);
  const queryRef = useRef(query);
  const detailOpenRef = useRef(detailOpen);
  useEffect(() => {
    queryRef.current = query;
    detailOpenRef.current = detailOpen;
  }, [query, detailOpen]);


  const q = query.trim();
  const matched = useMemo(() => matchAll(servers, q, categories), [servers, q, categories]);
  const list = matched.filter((r) => inCategory(r.connector, cat, categories) && matchesStatus(r.connector, status));
  const groups = groupConnectors(list, categories, !!q);
  const flat = groups.flatMap((g) => g.rows.map((r) => r.connector));
  const current = flat.find((c) => c.name === selected) ?? flat[0] ?? null;

  const presentCats = categories.filter((c) => servers.some((s) => categoryOf(s, categories) === c.id));
  const catObj = cat === "all" ? null : categories.find((c) => c.id === cat) ?? null;
  const statusObj = LIBRARY_STATUSES.find((s) => s.id === status) ?? null;
  const toggleable = flat.filter(isToggleable);
  const toggleableOn = toggleable.filter((c) => c.enabled).length;
  const bulkNames = toggleable.map((c) => c.name);

  const stacked = () => typeof window !== "undefined" && window.matchMedia?.(STACKED).matches;
  // Each view starts at its top, and focus follows the view change so a
  // keyboard or screen-reader user lands in it.
  const openDetail = (name: string) => {
    setSelected(name);
    setToolsOpen(false);
    if (!stacked()) return;
    setDetailOpen(true);
    requestAnimationFrame(() => {
      bodyRef.current?.scrollTo({ top: 0 });
      bodyRef.current?.querySelector<HTMLElement>("[data-library-column]")?.scrollTo({ top: 0 });
      backRef.current?.focus();
    });
  };
  // Back to the list, focusing the row whose detail was showing (current, not
  // selected: a filter may have hidden the selected row).
  const closeDetail = () => {
    const shown = current?.name;
    setDetailOpen(false);
    requestAnimationFrame(() => {
      panelRef.current
        ?.querySelector<HTMLElement>(`[data-testid="chat-tools-library-row-${CSS.escape(shown ?? "")}"] button`)
        ?.focus();
    });
  };
  // A filter change from the tablet rail (visible beside a stacked detail)
  // returns to the list it just changed.
  const filterBy = (next: () => void) => {
    next();
    setDetailOpen(false);
  };

  // Escape steps back from a stacked detail view, then clears the search, then
  // closes — and never closes while an account menu is open (that Escape
  // closes the menu).
  useDialogA11y(
    true,
    panelRef,
    () => {
      if (document.activeElement?.closest("[data-seat-menu]")) return;
      if (detailOpenRef.current && stacked()) {
        closeDetail();
        return;
      }
      if (queryRef.current) {
        setQuery("");
        return;
      }
      onClose();
    },
    // Opening straight into a stacked detail view focuses its back button (the
    // search field is hidden there). Otherwise, on a touch screen the sheet
    // itself takes focus instead of the search field, so the keyboard does not
    // open over the list; the trap treats the container as the wrap point, so
    // Shift+Tab from there stays inside.
    { initialFocusRef: detailOpen && stacked() ? backRef : isTouchPrimary() ? panelRef : searchRef },
  );

  return createPortal(
    <div
      className="fixed inset-0 z-[1200] flex items-center justify-center bg-[var(--color-scrim)] p-0 backdrop-blur-[3px] sm:p-4"
      role="dialog"
      aria-modal="true"
      aria-label="Tools and connections"
    >
      <div
        ref={panelRef}
        tabIndex={-1}
        data-testid="chat-tools-library"
        className="flex h-[100dvh] outline-none w-full flex-col overflow-hidden border border-[var(--color-selected-border)] bg-[var(--color-surface-1)] shadow-[var(--shadow-popover)] sm:h-[min(43.75rem,92vh)] sm:w-[min(70rem,96vw)] sm:rounded-[var(--radius-xl)]"
      >
        {/* The close button is its own column, so it stays top-right however
            the title block wraps; the admin link drops under the title on a
            phone and sits beside the close button from md. */}
        <header className="flex items-start gap-3 border-b border-[var(--color-divider)] px-4 pb-3.5 pt-[max(0.875rem,env(safe-area-inset-top))] sm:pt-3.5">
          <span
            aria-hidden="true"
            className="flex size-8 shrink-0 items-center justify-center rounded-[var(--radius-sm)] bg-[var(--color-icon-tile)] text-[var(--color-accent)]"
          >
            <Icon name="tools" className="size-[1.06rem]" />
          </span>
          <div className="flex min-h-8 min-w-0 flex-1 flex-wrap items-center gap-x-3 gap-y-0.5">
            <h2 className="m-0 text-[1.0625rem] font-semibold text-[var(--color-text-primary)]">Tools &amp; connections</h2>
            <span className="text-[0.8rem] tabular-nums text-[var(--color-text-subtle)]">
              {countOn(servers)} of {servers.length} on
            </span>
            {isAdmin ? (
              <a
                href={settingsHref()}
                title="Settings › Connections"
                className={`${LINK} hit-area-touch basis-full text-[0.8rem] md:ml-auto md:mr-1 md:basis-auto`}
              >
                Manage connections in Settings
                <Icon name="external-link" className="size-3" />
              </a>
            ) : null}
          </div>
          <CloseButton label="Close tools library" onClick={onClose} className="pointer-coarse:size-11" />
        </header>

        <div
          ref={bodyRef}
          className="grid min-h-0 flex-1 grid-cols-1 content-start overflow-y-auto overscroll-contain md:grid-cols-[14.5rem_minmax(0,1fr)] md:content-stretch md:overflow-hidden"
        >
          <aside
            className={`flex flex-col gap-2.5 border-b border-[var(--color-divider)] bg-[var(--color-rail-bg)] px-2.5 py-3 md:min-h-0 md:gap-3.5 md:overflow-y-auto md:border-b-0 md:border-r ${
              detailOpen ? "max-md:hidden" : ""
            }`}
          >
            <div className="relative flex items-center">
              <Icon
                name="search"
                className="pointer-events-none absolute left-2.5 size-[0.9rem] text-[var(--color-text-subtle)]"
              />
              <input
                ref={searchRef}
                type="search"
                value={query}
                onChange={(event) => setQuery(event.target.value)}
                placeholder="Search tools…"
                aria-label="Search tools"
                className="h-9 w-full rounded-[var(--radius-sm)] border border-[var(--color-selected-border)] bg-[var(--color-bg)] pl-8 pr-2.5 pointer-coarse:h-11 text-[0.875rem] text-[var(--color-text-primary)] outline-none transition placeholder:text-[var(--color-text-subtle)] focus:border-[var(--color-accent)] focus-visible:shadow-[var(--focus-ring)]"
              />
            </div>
            <nav aria-label="Categories" className={RAIL_NAV}>
              <div className={RAIL_LABEL}>Categories</div>
              {[{ id: "all", name: "All" }, ...presentCats].map((c) => (
                <RailButton
                  key={c.id}
                  active={cat === c.id}
                  label={c.name}
                  count={matched.filter((r) => inCategory(r.connector, c.id, categories) && matchesStatus(r.connector, status)).length}
                  onClick={() => filterBy(() => setCat(c.id))}
                />
              ))}
            </nav>
            <nav aria-label="Status" className={RAIL_NAV}>
              <div className={RAIL_LABEL}>Status</div>
              {LIBRARY_STATUSES.map((s) => (
                <RailButton
                  key={s.id}
                  active={status === s.id}
                  label={s.label}
                  dot={s.id}
                  count={matched.filter((r) => inCategory(r.connector, cat, categories) && matchesStatus(r.connector, s.id)).length}
                  onClick={() => filterBy(() => setStatus(status === s.id ? null : s.id))}
                />
              ))}
            </nav>
          </aside>

          <div
            data-library-column
            className="grid grid-cols-1 content-start md:min-h-0 md:overflow-y-auto md:overscroll-contain lg:grid-cols-[minmax(0,1fr)_23.25rem] lg:content-stretch lg:overflow-hidden"
          >
            <section
              aria-label="Connections"
              className={`flex flex-col pb-[env(safe-area-inset-bottom)] lg:min-h-0 lg:border-r lg:border-[var(--color-divider)] lg:pb-0 ${
                detailOpen ? "max-lg:hidden" : ""
              }`}
            >
              <div className="px-4 pb-2.5 pt-3.5">
                <div className="text-base font-semibold text-[var(--color-text-primary)]">
                  {(catObj ? catObj.name : "All connections") + (statusObj ? ` · ${statusObj.label}` : "")}
                </div>
                <div className="mt-0.5 text-[0.78rem] text-[var(--color-text-subtle)]">
                  {`${flat.length} ${flat.length === 1 ? "connection" : "connections"} · ${flat.filter(isOn).length} on`}
                  {q ? ` · matching ‘${q}’` : ""}
                </div>
              </div>
              {toggleable.length > 0 ? (
                <div
                  className="mx-4 mb-2.5 flex items-center gap-1.5 rounded-[var(--radius-sm)] border border-[var(--color-divider)] bg-[var(--color-rail-bg)] py-1.5 pl-3 pr-1.5 pointer-coarse:min-h-11 pointer-coarse:py-0"
                  data-testid="chat-tools-library-bulk"
                >
                  <span className="min-w-0 flex-1 text-[0.78rem] text-[var(--color-text-subtle)]">
                    {libraryBulkScope(toggleable.length, {
                      filtered: !!q || !!status,
                      categoryName: catObj?.name,
                    })}
                  </span>
                  <button
                    type="button"
                    className={SMALL_PILL}
                    aria-disabled={toggleableOn === toggleable.length || undefined}
                    onClick={() => {
                      if (toggleableOn !== toggleable.length) onSetEnabled(bulkNames, true);
                    }}
                  >
                    All on
                  </button>
                  <button
                    type="button"
                    className={SMALL_PILL}
                    aria-disabled={toggleableOn === 0 || undefined}
                    onClick={() => {
                      if (toggleableOn !== 0) onSetEnabled(bulkNames, false);
                    }}
                  >
                    All off
                  </button>
                </div>
              ) : null}
              <div className="px-2.5 pb-3 [scrollbar-width:thin] lg:min-h-0 lg:flex-1 lg:overflow-y-auto">
                {groups.map((g) => (
                  <div key={g.category.id} className="flex flex-col gap-0.5" role="group" aria-label={g.category.name}>
                    {cat === "all" ? <div className={GROUP_LABEL}>{g.category.name}</div> : null}
                    {g.rows.map(({ connector: c }) => (
                      <LibraryRow
                        key={c.name}
                        connector={c}
                        query={q}
                        selected={current?.name === c.name}
                        onSelect={() => openDetail(c.name)}
                        onToggle={() => onToggle(c.name)}
                      />
                    ))}
                  </div>
                ))}
                {flat.length === 0 ? (
                  <div className="px-5 py-12 text-center text-[0.875rem] text-[var(--color-text-subtle)]">
                    {q ? `No tools match ‘${q}’` : "Nothing here with this filter"}
                  </div>
                ) : null}
              </div>
            </section>

            <section
              aria-label="Connection details"
              className={`px-5 pb-[max(1.5rem,env(safe-area-inset-bottom))] pt-3 lg:min-h-0 lg:overflow-y-auto lg:pb-6 lg:pt-4.5 ${
                detailOpen ? "" : "max-lg:hidden"
              }`}
            >
              <button
                ref={backRef}
                type="button"
                onClick={closeDetail}
                className="-ml-2 mb-1 inline-flex h-11 items-center gap-1 rounded-[var(--radius-sm)] px-2 text-[0.875rem] text-[var(--color-link)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] lg:hidden"
              >
                <Icon name="chevron-right" className="size-4 rotate-180" />
                {catObj ? catObj.name : "All connections"}
              </button>
              {current ? (
                <Detail
                  connector={current}
                  categories={categories}
                  appName={appName}
                  isAdmin={isAdmin}
                  query={q}
                  toolsOpen={toolsOpen}
                  onToggleTools={() => setToolsOpen((v) => !v)}
                  onToggle={() => onToggle(current.name)}
                  onSetAccount={(account) => onSetAccount(current.name, account)}
                />
              ) : null}
            </section>
          </div>
        </div>
      </div>
    </div>,
    document.body,
  );
}

function RailButton({
  active,
  label,
  count,
  dot,
  onClick,
}: {
  active: boolean;
  label: string;
  count: number;
  dot?: LibraryStatusFilter;
  onClick: () => void;
}) {
  return (
    <button
      type="button"
      aria-pressed={active}
      onClick={onClick}
      className={`flex h-9 shrink-0 items-center gap-2 whitespace-nowrap rounded-[var(--radius-pill)] max-md:pointer-coarse:h-11 border border-[var(--color-pill-border)] px-3 text-left text-[0.84rem] transition md:h-8 md:rounded-[var(--radius-sm)] md:border-transparent md:px-2.5 hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] ${
        active
          ? "bg-[var(--color-selected-bg)] text-[var(--color-text-primary)] shadow-[inset_0_0_0_1px_var(--color-selected-border)]"
          : "text-[var(--color-text-secondary)]"
      }`}
    >
      {dot ? <StatusDot status={dot === "on" ? "on" : dot} /> : null}
      <span className="md:flex-1 md:truncate">{label}</span>
      <span className="text-[0.75rem] tabular-nums text-[var(--color-text-subtle)]">{count}</span>
    </button>
  );
}

function LibraryRow({
  connector: c,
  query,
  selected,
  onSelect,
  onToggle,
}: {
  connector: PickerConnector;
  query: string;
  selected: boolean;
  onSelect: () => void;
  onToggle: () => void;
}) {
  const status = connectorStatus(c);
  const name = connectorLabel(c);
  return (
    <div
      className={`flex items-center gap-3 rounded-[var(--radius-md)] pl-3 pr-2.5 transition hover:bg-[var(--color-pill-hover-bg)] pointer-coarse:min-h-11 ${
        selected ? "bg-[var(--color-selected-bg)] shadow-[inset_0_0_0_1px_var(--color-selected-border)]" : ""
      } ${status === "unavailable" ? "opacity-60" : ""}`}
      data-testid={`chat-tools-library-row-${c.name}`}
    >
      <button
        type="button"
        onClick={onSelect}
        aria-current={selected || undefined}
        className="flex min-w-0 flex-1 flex-col justify-center gap-0.5 self-stretch py-2 text-left focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]"
      >
        <span className="flex min-w-0 items-baseline gap-2">
          <span className="truncate text-[0.875rem] font-semibold text-[var(--color-text-primary)]">
            <Highlight text={name} query={query} />
          </span>
          <StatusKicker status={status} label={statusKicker(status)} />
        </span>
        {c.description ? (
          <span className="line-clamp-2 text-[0.78rem] text-[var(--color-text-subtle)] sm:line-clamp-none sm:truncate">
            {c.description}
          </span>
        ) : null}
      </button>
      {c.tool_count > 0 ? (
        <span className="hidden shrink-0 whitespace-nowrap font-[family-name:var(--font-code)] text-[0.6875rem] text-[var(--color-text-subtle)] sm:inline">
          {toolCountLabel(c.tool_count)}
        </span>
      ) : null}
      <div className="flex w-[2.125rem] shrink-0 justify-end pr-1">
        {status === "on" || status === "off" ? (
          <ToolSwitch on={status === "on"} label={name} onToggle={onToggle} />
        ) : (
          <StatusIcon status={status} />
        )}
      </div>
    </div>
  );
}

function Detail({
  connector: c,
  categories,
  appName,
  isAdmin,
  query,
  toolsOpen,
  onToggleTools,
  onToggle,
  onSetAccount,
}: {
  connector: PickerConnector;
  categories: PickerCategory[];
  appName: string;
  isAdmin: boolean;
  query: string;
  toolsOpen: boolean;
  onToggleTools: () => void;
  onToggle: () => void;
  onSetAccount: (account: string) => void;
}) {
  const status = connectorStatus(c);
  const name = connectorLabel(c);
  const toggleable = status === "on" || status === "off";
  const tools = c.tools ?? [];
  const hasSeats = (c.accounts?.length ?? 0) > 0 && (toggleable || status === "always");
  return (
    <div className="flex flex-col gap-4" data-testid="chat-tools-library-detail">
      <div className="flex flex-col gap-1.5">
        <div className="font-[family-name:var(--font-code)] text-[0.6875rem] uppercase tracking-[0.12em] text-[var(--color-text-subtle)]">
          {categoryName(categoryOf(c, categories), categories)}
        </div>
        <div className="flex items-center gap-3">
          <h3 className="m-0 flex-1 text-[1.3rem] font-semibold leading-tight text-[var(--color-text-primary)]">{name}</h3>
          {toggleable ? <ToolSwitch on={status === "on"} label={name} onToggle={onToggle} large /> : null}
        </div>
        <div className="flex items-center gap-2 text-[0.78rem] text-[var(--color-text-subtle)]">
          <StatusDot status={status} />
          <span className="text-[var(--color-text-secondary)]">{statusText(status)}</span>
          {c.tool_count > 0 ? (
            <>
              <span aria-hidden="true">·</span>
              <span className="font-[family-name:var(--font-code)] text-[0.72rem]">{toolCountLabel(c.tool_count)}</span>
            </>
          ) : null}
        </div>
      </div>
      {c.description ? (
        <p className="m-0 text-[0.875rem] leading-[1.55] text-[var(--color-text-secondary)]">{c.description}</p>
      ) : null}
      {toggleable ? (
        <a href={settingsHref(c.name)} className={`${LINK} hit-area-touch -mt-1.5 self-start text-[0.78rem]`}>
          Manage in Settings
          <Icon name="external-link" className="size-3" />
        </a>
      ) : null}
      {hasSeats ? (
        <div className="flex flex-col gap-1.5">
          <div className="text-[0.75rem] text-[var(--color-text-subtle)]">Account for this chat</div>
          <SeatMenu connector={c} appName={appName} variant="field" onChange={onSetAccount} />
        </div>
      ) : null}
      {status === "setup" ? (
        <div className="flex flex-col items-start gap-1.5 rounded-[var(--radius-md)] border border-[var(--color-panel-warn-border)] bg-[var(--color-panel-warn-bg)] px-3.5 py-2.5 text-[0.8rem] leading-[1.5] text-[var(--color-text-secondary)]">
          {isAdmin ? (
            <>
              <span>Needs setup before it can be used.</span>
              <a href={settingsHref(c.name)} className={LINK}>
                Set up in Settings
                <Icon name="external-link" className="size-3" />
              </a>
            </>
          ) : (
            <span>An admin needs to set this up.</span>
          )}
        </div>
      ) : null}
      {status === "always" ? (
        <div className="rounded-[var(--radius-md)] border border-[var(--color-panel-ok-border)] bg-[var(--color-panel-ok-bg)] px-3.5 py-2.5 text-[0.8rem] leading-[1.5] text-[var(--color-text-secondary)]">
          Always on in every chat.
        </div>
      ) : null}
      {status === "unavailable" ? (
        <div className="rounded-[var(--radius-md)] border border-[var(--color-panel-deny-border)] bg-[var(--color-panel-deny-bg)] px-3.5 py-2.5 text-[0.8rem] leading-[1.5] text-[var(--color-text-secondary)]">
          Unavailable right now. Try again later.
        </div>
      ) : null}
      {tools.length > 0 ? (
        <div className="flex flex-col gap-1">
          <button
            type="button"
            aria-expanded={toolsOpen}
            onClick={onToggleTools}
            className="hit-area-touch inline-flex items-center gap-1 self-start text-[0.78rem] text-[var(--color-text-subtle)] transition hover:text-[var(--color-text-secondary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]"
          >
            {toolsOpen ? "Hide tools" : `Show ${tools.length} ${tools.length === 1 ? "tool" : "tools"}`}
            <Icon name="chevron-down" className={`size-3 transition-transform ${toolsOpen ? "rotate-180" : ""}`} />
          </button>
          {toolsOpen ? (
            <ul className="m-0 flex list-none flex-col p-0 pl-0.5 pt-1">
              {tools.map((t) => (
                <li
                  key={t}
                  className="flex h-6 items-center font-[family-name:var(--font-code)] text-[0.75rem] text-[var(--color-text-subtle)]"
                >
                  <Highlight text={t} query={query} />
                </li>
              ))}
            </ul>
          ) : null}
        </div>
      ) : null}
    </div>
  );
}
