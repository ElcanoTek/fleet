"use client";

import { useEffect, useLayoutEffect, useMemo, useRef, useState, type KeyboardEvent } from "react";
import { Icon } from "@/app/shared/ui/Icon";
import {
  bulkScope,
  connectorLabel,
  connectorStatus,
  countOn,
  isToggleable,
  openedByTouch,
  onSnapshot,
  popoverSections,
  statusKicker,
  toolCountLabel,
  type PickerCategory,
  type PickerConnector,
} from "./toolsPicker";
import { Highlight, SeatMenu, StatusKicker, ToolSwitch } from "./ToolsPickerParts";

// The composer's Tools popover (the wrench). Search pinned at the top and
// auto-focused, like the model picker; an "On" section pinned first while
// browsing, then static category headings; search keeps the headings and
// adds connections that need setup or are unavailable, dimmed. The full list
// lives in the library (ToolsLibrary), opened from the expand icon or the
// footer link.

export type ToolsPopoverProps = {
  servers: PickerConnector[];
  categories: PickerCategory[];
  appName: string;
  isAdmin: boolean;
  loading: boolean;
  onToggle: (name: string) => void;
  onSetEnabled: (names: string[], enabled: boolean) => void;
  onSetAccount: (name: string, account: string) => void;
  /** Escape with an empty search: close and return focus to the wrench. */
  onClose: () => void;
  /** Open the library, optionally on one connection. */
  onOpenLibrary: (name?: string) => void;
};

// The design's small pill button (All on / All off). When the action would
// change nothing it is marked aria-disabled and dimmed, NOT natively
// disabled: the button just activated is the one that becomes a no-op, and a
// natively disabled element drops focus, which would break Escape-closes /
// focus-returns-to-the-wrench for a keyboard user mid-gesture.
const SMALL_PILL =
  "hit-area-touch h-[1.375rem] shrink-0 whitespace-nowrap rounded-[var(--radius-pill)] border border-[var(--color-pill-border)] px-[0.55rem] text-[0.75rem] text-[var(--color-text-secondary)] transition hover:bg-[var(--color-pill-hover-bg)] hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] aria-disabled:cursor-not-allowed aria-disabled:opacity-40";

const GROUP_LABEL =
  "font-[family-name:var(--font-code)] text-[0.6875rem] uppercase tracking-[0.12em] text-[var(--color-text-subtle)]";

export function ToolsPopover({
  servers,
  categories,
  appName,
  isAdmin,
  loading,
  onToggle,
  onSetEnabled,
  onSetAccount,
  onClose,
  onOpenLibrary,
}: ToolsPopoverProps) {
  const [query, setQuery] = useState("");
  // The active row for the keyboard: -1 = none. `nav` is true once the arrow
  // keys moved it, so Space types a space until then.
  const [active, setActive] = useState(-1);
  const [nav, setNav] = useState(false);
  // What was on when the popover opened (or when its catalog finished
  // loading). Rows stay where they started while you toggle them.
  const [pinned, setPinned] = useState<Set<string> | null>(() => (loading ? null : onSnapshot(servers)));
  if (pinned === null && !loading) setPinned(onSnapshot(servers));
  const inputRef = useRef<HTMLInputElement | null>(null);
  const listRef = useRef<HTMLDivElement | null>(null);

  useEffect(() => {
    if (!openedByTouch()) inputRef.current?.focus({ preventScroll: true });
  }, []);

  // From `sm` the popover hangs above the wrench inside the chat column,
  // which clips its overflow and starts below the chat header. The CSS cap
  // (100dvh - 7rem) assumes the whole window is free above it, so a short
  // window cut off the search row. Cap it to the room actually left between
  // its bottom and the top of the nearest clipping ancestor. Measured from
  // layout offsets, not the bounding box, so the entrance animation's rise
  // does not skew it. Below `sm` it is fixed to the viewport and the CSS
  // cap holds.
  const rootRef = useRef<HTMLDivElement | null>(null);
  const [room, setRoom] = useState<number | null>(null);
  useLayoutEffect(() => {
    const fit = () => {
      const el = rootRef.current;
      const parent = el?.offsetParent;
      if (!el || !parent || getComputedStyle(el).position !== "absolute") {
        setRoom(null);
        return;
      }
      const bottom = parent.getBoundingClientRect().top + el.offsetTop + el.offsetHeight;
      let top = 0;
      for (let a = el.parentElement; a; a = a.parentElement) {
        if (getComputedStyle(a).overflowY !== "visible") {
          top = Math.max(0, a.getBoundingClientRect().top);
          break;
        }
      }
      setRoom(Math.floor(bottom - top - 8));
    };
    fit();
    window.addEventListener("resize", fit);
    return () => window.removeEventListener("resize", fit);
  }, []);

  const q = query.trim();
  const sections = useMemo(
    () => popoverSections(servers, q, categories, pinned ?? new Set()),
    [servers, q, categories, pinned],
  );
  const flat = useMemo(() => sections.flatMap((s) => s.rows.map((r) => r.connector)), [sections]);
  // Each section's first row index in `flat`, for the keyboard highlight.
  const offsets = useMemo(() => {
    const out: number[] = [];
    let n = 0;
    for (const s of sections) {
      out.push(n);
      n += s.rows.length;
    }
    return out;
  }, [sections]);
  const resultCount = new Set(flat.map((c) => c.name)).size;
  const onCount = countOn(servers);
  const scope = bulkScope(sections);
  const scopeRows = servers.filter((s) => scope.includes(s.name));
  const scopeOn = scopeRows.filter((s) => s.enabled).length;
  const n = scope.length;
  const bulkTitle = q
    ? `Applies to ${n} ${n === 1 ? "connection" : "connections"} shown`
    : `Applies to ${n} ${n === 1 ? "connection" : "connections"}`;

  // Keep the active row in view as the arrows move it.
  useEffect(() => {
    if (active < 0) return;
    listRef.current
      ?.querySelector<HTMLElement>(`[data-row-index="${active}"]`)
      ?.scrollIntoView?.({ block: "nearest" });
  }, [active]);

  // A dimmed row (needs setup, unavailable) has no switch: activating it
  // opens that connection in the library, where its status is explained.
  // Always-on rows have nothing to do.
  const activate = (c: PickerConnector) => {
    const s = connectorStatus(c);
    if (isToggleable(c)) onToggle(c.name);
    else if (s === "setup" || s === "unavailable") onOpenLibrary(c.name);
  };

  const onKey = (event: KeyboardEvent<HTMLInputElement>) => {
    if (event.key === "ArrowDown" || event.key === "ArrowUp") {
      event.preventDefault();
      if (flat.length === 0) return;
      const d = event.key === "ArrowDown" ? 1 : -1;
      setActive((a) => {
        const next = a + d;
        if (next < 0) return flat.length - 1;
        if (next >= flat.length) return 0;
        return next;
      });
      setNav(true);
    } else if ((event.key === " " && nav) || event.key === "Enter") {
      const row = flat[active];
      if (row) {
        event.preventDefault();
        activate(row);
      }
    } else if (event.key === "Escape") {
      // Clear first, then close. Stop here so the composer's own Escape
      // handler does not close the popover on the first press.
      event.preventDefault();
      event.stopPropagation();
      if (query) {
        setQuery("");
        setActive(-1);
        setNav(false);
      } else {
        onClose();
      }
    }
  };

  const clearQuery = () => {
    setQuery("");
    setActive(-1);
    setNav(false);
    inputRef.current?.focus();
  };

  return (
    <div
      ref={rootRef}
      style={room === null ? undefined : { maxHeight: `min(32.5rem, ${room}px)` }}
      className="fixed inset-x-2 bottom-[calc(env(safe-area-inset-bottom,0px)+5rem)] z-30 flex max-h-[min(32.5rem,calc(100dvh-7rem))] flex-col overflow-hidden rounded-[var(--radius-lg)] border border-[var(--color-selected-border)] bg-[var(--color-surface-2)] text-left shadow-[var(--shadow-popover)] motion-safe:animate-pop-up sm:absolute sm:inset-x-auto sm:bottom-[calc(100%+0.55rem)] sm:left-[-10rem] sm:w-[21.75rem]"
      data-testid="chat-tools-popover"
    >
      <div className="flex items-center gap-1.5 px-2 pb-1.5 pt-2">
        <div className="relative flex min-w-0 flex-1 items-center">
          <Icon
            name="search"
            className="pointer-events-none absolute left-2.5 size-[0.9rem] text-[var(--color-text-subtle)]"
          />
          <input
            ref={inputRef}
            value={query}
            onChange={(event) => {
              setQuery(event.target.value);
              setActive(event.target.value.trim() ? 0 : -1);
              setNav(false);
            }}
            onKeyDown={onKey}
            placeholder="Search tools…"
            aria-label="Search tools"
            className="h-[2.125rem] w-full rounded-[var(--radius-sm)] border border-[var(--color-selected-border)] bg-[var(--color-bg)] pl-8 pr-8 text-[0.875rem] any-pointer-coarse:h-11 any-pointer-coarse:pr-11 text-[var(--color-text-primary)] outline-none transition placeholder:text-[var(--color-text-subtle)] focus:border-[var(--color-accent)] focus-visible:shadow-[var(--focus-ring)]"
          />
          {query ? (
            <button
              type="button"
              aria-label="Clear search"
              onClick={clearQuery}
              className="absolute right-1.5 inline-flex size-[1.375rem] items-center any-pointer-coarse:right-0 any-pointer-coarse:size-11 justify-center rounded-[var(--radius-sm)] text-[var(--color-text-subtle)] transition hover:bg-[var(--color-pill-hover-bg)] hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]"
            >
              <Icon name="close" className="size-3" />
            </button>
          ) : null}
        </div>
        <button
          type="button"
          title="Open tool library"
          aria-label="Open tool library"
          onClick={() => onOpenLibrary()}
          className="inline-flex size-[2.125rem] shrink-0 items-center justify-center rounded-[var(--radius-sm)] border border-[var(--color-pill-border)] any-pointer-coarse:size-11 text-[var(--color-text-subtle)] transition hover:border-[var(--color-selected-border)] hover:bg-[var(--color-pill-hover-bg)] hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]"
        >
          <Icon name="maximize" className="size-[0.9rem]" />
        </button>
      </div>

      <div
        className="flex items-center gap-1.5 border-b border-[var(--color-divider)] pb-1.5 pl-3 pr-2.5 pt-0.5 any-pointer-coarse:min-h-11 any-pointer-coarse:py-0"
        data-testid="chat-mcp-all-actions"
      >
        <span className="min-w-0 flex-1 truncate text-[0.75rem] tabular-nums text-[var(--color-text-subtle)]">
          {q ? `${resultCount} ${resultCount === 1 ? "result" : "results"} · ${onCount} on` : `${onCount} on`}
        </span>
        {n > 0 ? (
          <>
            <button
              type="button"
              title={bulkTitle}
              className={SMALL_PILL}
              aria-disabled={scopeOn === n || undefined}
              onClick={() => {
                if (scopeOn !== n) onSetEnabled(scope, true);
              }}
            >
              {q ? `All ${n} on` : "All on"}
            </button>
            <button
              type="button"
              title={bulkTitle}
              className={SMALL_PILL}
              aria-disabled={scopeOn === 0 || undefined}
              onClick={() => {
                if (scopeOn !== 0) onSetEnabled(scope, false);
              }}
            >
              {q ? `All ${n} off` : "All off"}
            </button>
          </>
        ) : null}
      </div>

      <div
        ref={listRef}
        className="flex min-h-0 flex-1 flex-col overflow-y-auto pb-1.5 pl-1.5 pr-1 pt-1 [scrollbar-gutter:stable] [scrollbar-width:thin]"
      >
        {loading && servers.length === 0 ? (
          <div className="px-2.5 py-2 text-[0.75rem] text-[var(--color-text-subtle)]">Loading…</div>
        ) : null}
        {sections.map((section, si) => (
          <div key={section.key} className="flex flex-col" role="group" aria-label={section.label}>
            <div className="flex min-h-6 items-center gap-1.5 px-1.5 pb-1 pt-2">
              <span className={GROUP_LABEL}>{section.label}</span>
              <span className="ml-auto pr-1 text-[0.75rem] tabular-nums text-[var(--color-text-subtle)]">
                {section.count}
              </span>
            </div>
            <div className="flex flex-col gap-px">
              {section.rows.map(({ connector: c, match }, ri) => {
                const idx = offsets[si] + ri;
                return (
                  <PopoverRow
                    key={`${section.key}:${c.name}`}
                    index={idx}
                    connector={c}
                    toolHits={match.toolHits}
                    query={q}
                    appName={appName}
                    isAdmin={isAdmin}
                    active={idx === active}
                    ring={idx === active && nav}
                    onHover={() => {
                      if (active !== idx || nav) {
                        setActive(idx);
                        setNav(false);
                      }
                    }}
                    onActivate={() => activate(c)}
                    onToggle={() => onToggle(c.name)}
                    onOpenLibrary={() => onOpenLibrary(c.name)}
                    onSetAccount={(account) => onSetAccount(c.name, account)}
                  />
                );
              })}
            </div>
          </div>
        ))}
        {!loading && flat.length === 0 ? (
          <div className="flex flex-col items-center gap-2.5 px-5 pb-10 pt-9 text-center">
            <div className="text-[0.875rem] text-[var(--color-text-primary)]">
              {q ? `No tools match ‘${q}’` : "No tools in this chat"}
            </div>
            <div className="max-w-60 text-[0.78rem] text-[var(--color-text-subtle)]">
              Try another word, or browse everything in the library.
            </div>
            <div className="flex gap-3.5 text-[0.8rem]">
              <button
                type="button"
                onClick={() => onOpenLibrary()}
                className="hit-area-touch text-[var(--color-link)] underline underline-offset-[3px] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]"
              >
                Open the full library
              </button>
              {q ? (
                <button
                  type="button"
                  onClick={clearQuery}
                  className="hit-area-touch text-[var(--color-text-subtle)] transition hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]"
                >
                  Clear search
                </button>
              ) : null}
            </div>
          </div>
        ) : null}
      </div>

      {/* 44px tall on a touch screen, so the link's 2.75rem touch box is not
          clipped by the popover's overflow. */}
      <div className="flex items-center justify-end border-t border-[var(--color-divider)] px-3 py-1.5 any-pointer-coarse:min-h-11 any-pointer-coarse:py-0">
        <button
          type="button"
          onClick={() => onOpenLibrary()}
          className="hit-area-touch inline-flex items-center gap-1.5 text-[0.75rem] text-[var(--color-link)] hover:underline focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]"
        >
          Open library
          <Icon name="maximize" className="size-[0.7rem]" />
        </button>
      </div>
    </div>
  );
}

function PopoverRow({
  index,
  connector: c,
  toolHits,
  query,
  appName,
  isAdmin,
  active,
  ring,
  onHover,
  onActivate,
  onToggle,
  onOpenLibrary,
  onSetAccount,
}: {
  index: number;
  connector: PickerConnector;
  toolHits: string[] | null;
  query: string;
  appName: string;
  isAdmin: boolean;
  active: boolean;
  ring: boolean;
  onHover: () => void;
  onActivate: () => void;
  onToggle: () => void;
  onOpenLibrary: () => void;
  onSetAccount: (account: string) => void;
}) {
  const status = connectorStatus(c);
  const name = connectorLabel(c);
  const dimmed = status === "setup" || status === "unavailable";
  const clickable = status !== "always";
  const hasSeats = (c.accounts?.length ?? 0) > 0 && !dimmed;
  return (
    // The row is a pointer target only; its switch, account menu and setup
    // link are the keyboard-reachable controls, and the search field's
    // arrow keys move the highlight.
    // oxlint-disable-next-line jsx-a11y/click-events-have-key-events, jsx-a11y/no-static-element-interactions
    <div
      data-row-index={index}
      data-testid={`chat-tools-row-${c.name}`}
      onMouseEnter={onHover}
      onClick={() => {
        if (clickable) onActivate();
      }}
      className={`flex items-start gap-2.5 rounded-[var(--radius-sm)] py-[0.45rem] pl-2.5 pr-2 transition any-pointer-coarse:min-h-11 ${
        clickable ? "cursor-pointer" : "cursor-default"
      } ${active ? "bg-[var(--color-selected-bg)]" : ""} ${
        ring ? "shadow-[inset_0_0_0_1px_var(--color-selected-border)]" : ""
      }`}
    >
      <div className={`flex min-w-0 flex-1 flex-col gap-0.5 ${dimmed ? "opacity-55" : ""}`}>
        <div className="flex min-w-0 items-baseline gap-2">
          <span className="truncate text-[0.875rem] font-semibold text-[var(--color-text-primary)]">
            <Highlight text={name} query={query} />
          </span>
          <StatusKicker status={status} label={status === "always" ? statusKicker(status) : ""} />
          {c.beta ? (
            <span
              className="shrink-0 rounded-sm border border-[var(--color-pill-border)] px-1 font-[family-name:var(--font-code)] text-[0.55rem] font-semibold uppercase tracking-wider text-[var(--color-text-subtle)]"
              title="This connector is in beta — it works but still has rough edges."
            >
              beta
            </span>
          ) : null}
        </div>
        {c.description ? (
          <div className="line-clamp-2 text-[0.78rem] leading-[1.4] text-[var(--color-text-subtle)]">
            <Highlight text={c.description} query={query} />
          </div>
        ) : null}
        <div className="flex min-w-0 items-center gap-2 text-[0.6875rem] text-[var(--color-text-subtle)]">
          {c.tool_count > 0 ? (
            <span className="shrink-0 whitespace-nowrap font-[family-name:var(--font-code)]">
              {toolCountLabel(c.tool_count)}
            </span>
          ) : null}
          {hasSeats ? <SeatMenu connector={c} appName={appName} variant="compact" onChange={onSetAccount} /> : null}
          {toolHits ? (
            <span className="min-w-0 truncate whitespace-nowrap font-[family-name:var(--font-code)]">
              ↳ <Highlight text={toolHits[0]} query={query} />
              {toolHits.length > 1 ? `  +${toolHits.length - 1}` : ""}
            </span>
          ) : null}
        </div>
      </div>
      <div className="flex shrink-0 items-center pt-0.5">
        {status === "on" || status === "off" ? (
          <ToolSwitch on={status === "on"} label={name} onToggle={onToggle} />
        ) : null}
        {status === "always" ? (
          <span
            title="Always on in every chat."
            className="flex h-[1.125rem] w-[1.875rem] items-center justify-center text-[var(--color-text-subtle)]"
            data-testid={`chat-mcp-always-on-${c.name}`}
          >
            <Icon name="lock" className="size-3.5" />
          </span>
        ) : null}
        {status === "setup" && isAdmin ? (
          <button
            type="button"
            onClick={(event) => {
              event.stopPropagation();
              onOpenLibrary();
            }}
            className="hit-area-touch inline-flex items-center gap-0.5 whitespace-nowrap text-[0.75rem] text-[var(--color-link)] hover:underline focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]"
          >
            Set up
            <Icon name="chevron-right" className="size-3" />
          </button>
        ) : null}
        {status === "setup" && !isAdmin ? (
          <span className="whitespace-nowrap text-[0.75rem] text-[var(--color-text-subtle)]">Needs admin setup</span>
        ) : null}
        {status === "unavailable" ? (
          <span className="whitespace-nowrap text-[0.75rem] text-[var(--color-text-subtle)]">Unavailable</span>
        ) : null}
      </div>
    </div>
  );
}
