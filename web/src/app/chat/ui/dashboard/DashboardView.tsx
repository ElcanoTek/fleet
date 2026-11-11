import { rich } from "@/app/lib/i18n/rich";
import { getLanguage } from "@/app/lib/i18n";
import { t as tr } from "@/app/lib/i18n";
// Dashboard view (docs/DASHBOARD-VIEW.md): the chat as a live board. The HUD
// bar replaces the page header, the board of panels fills the screen, and the
// ordinary chat (transcript + composer, approvals and forms inline) narrows
// to a rail beside it. Nothing here talks to the server: panels are the
// conversation's show_ui cards (board.ts), and every send goes through the
// same submitPrompt the transcript uses.

import {
  lazy,
  Suspense,
  useCallback,
  useEffect,
  useId,
  useRef,
  useState,
  type DragEvent as ReactDragEvent,
} from "react";
import { Icon } from "../Icon";
import { humanToolLabel } from "../history";
import type { GenerativeCardProps } from "../genui/GenerativeCard";
import type { Submission, Reply } from "../genui/model";
import { panelDomId } from "./context";
import { dropKey, moveKey, type BoardPanel, type BoardPrefs, type BoardStatus } from "./board";

const GenerativeCard = lazy(() => import("../genui/GenerativeCard"));

type SubmitFn = NonNullable<GenerativeCardProps["onSubmit"]>;

// ───────────────────────────── HUD ─────────────────────────────

function useClock(): string {
  const [now, setNow] = useState(() => new Date());
  useEffect(() => {
    const id = window.setInterval(() => setNow(new Date()), 1000);
    return () => window.clearInterval(id);
  }, []);
  return now.toLocaleTimeString(getLanguage(), { hour: "2-digit", minute: "2-digit", second: "2-digit" });
}

function StatusLight({ status, onApprovals }: { status: BoardStatus; onApprovals: () => void }) {
  if (status.kind === "approval") {
    return (
      <button
        type="button"
        data-testid="dashboard-status"
        data-status="approval"
        onClick={onApprovals}
        className="dash-status dash-status-warn"
      >
        <span className="dash-led dash-led-pulse" aria-hidden="true" />
        <span aria-hidden="true" className="sm:hidden">
          {status.count}
        </span>
        <span className="max-sm:sr-only">
          {tr(status.count === 1 ? "{0} approval waiting" : "{0} approvals waiting", [status.count])}
        </span>
      </button>
    );
  }
  if (status.kind === "working") {
    return (
      <span data-testid="dashboard-status" data-status="working" className="dash-status dash-status-live" role="status">
        <span className="dash-led dash-led-pulse" aria-hidden="true" />
        <span className="truncate max-sm:sr-only">{status.label ? humanToolLabel(status.label) : tr("Working")}</span>
      </span>
    );
  }
  return (
    <span data-testid="dashboard-status" data-status="standby" className="dash-status dash-status-ok" role="status">
      <span className="dash-led" aria-hidden="true" />
      <span className="max-sm:sr-only">{tr("Standby")}</span>
    </span>
  );
}

function HelpPopover({ onClose, onEscape }: { onClose: () => void; onEscape: () => void }) {
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => {
    ref.current?.focus();
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") {
        e.stopPropagation();
        onEscape();
      }
    };
    // "Outside" means outside the popover AND its trigger (the wrapper they
    // share): a press on the trigger is its own toggle, and closing here too
    // would let that click reopen it.
    const onDown = (e: MouseEvent) => {
      const boundary = ref.current?.parentElement ?? ref.current;
      if (boundary && !boundary.contains(e.target as Node)) onClose();
    };
    window.addEventListener("keydown", onKey, true);
    window.addEventListener("mousedown", onDown);
    return () => {
      window.removeEventListener("keydown", onKey, true);
      window.removeEventListener("mousedown", onDown);
    };
  }, [onClose, onEscape]);
  return (
    <div
      ref={ref}
      tabIndex={-1}
      role="dialog"
      aria-label={tr("How dashboard view works")}
      data-testid="dashboard-help"
      className="dash-popover absolute right-0 top-[calc(100%+0.5rem)] z-40 w-[min(22rem,calc(100vw-2rem))] p-4 text-[0.8125rem] leading-[1.55] outline-none"
    >
      <p className="dash-overline mb-2">{tr("How it works")}</p>
      <ul className="grid gap-2 text-[var(--color-text-secondary)]">
        <li>
        {rich("{0} The assistant builds panels — numbers, charts, tables — and updates them in place when things change.", <strong className="text-[var(--color-text-primary)]">{tr("Ask for what you want to watch.")}</strong>)}
      </li>
        <li>
          {tr("Try “turn this into a dashboard”, “add a panel for…”, or “refresh the revenue panel”.")}
        </li>
        <li>
          {tr("Approvals, emails and forms still appear in the chat rail. Nothing runs without your OK.")}
        </li>
        <li>
          {tr("Drag a panel by its header to reorder it. Use the panel buttons to widen it, focus it, or take it off the board (its card goes back to the chat).")}
        </li>
        <li>{tr("Pin any card from the chat with “Pin to board”. Your layout is saved in this browser.")}</li>
      </ul>
      <p className="mt-3 flex flex-wrap items-center gap-1.5 text-[0.75rem] text-[var(--color-text-muted)]">
        {rich("{0} toggles dashboard view · {1} exits", <kbd className="dash-kbd">{tr("D")}</kbd>, <kbd className="dash-kbd">{tr("Esc")}</kbd>)}
      </p>
    </div>
  );
}

export function DashboardHud({
  title,
  status,
  panelCount,
  mobilePane,
  setMobilePane,
  onExit,
  onApprovals,
}: {
  title: string;
  status: BoardStatus;
  panelCount: number;
  mobilePane: "board" | "chat";
  setMobilePane: (pane: "board" | "chat") => void;
  onExit: () => void;
  onApprovals: () => void;
}) {
  const clock = useClock();
  const [helpOpen, setHelpOpen] = useState(false);
  const closeHelp = useCallback(() => setHelpOpen(false), []);
  // Esc closes the popover from inside it (it holds the focus): hand the
  // focus back to the button that opened it rather than dropping it.
  const helpButtonRef = useRef<HTMLButtonElement>(null);
  const escapeHelp = useCallback(() => {
    setHelpOpen(false);
    helpButtonRef.current?.focus();
  }, []);
  return (
    <header data-testid="dashboard-hud" className="dash-hud">
      <div className="flex min-w-0 flex-1 items-center gap-3 sm:flex-none">
        <span className="dash-hud-mark max-sm:hidden" aria-hidden="true">
          <Icon name="target" className="size-4" />
        </span>
        <div className="min-w-0">
          <p className="dash-overline">{tr("Dashboard")}</p>
          <h1 className="truncate text-[0.95rem] font-semibold text-[var(--color-text-primary)]">{title}</h1>
        </div>
      </div>
      <div className="flex min-w-0 items-center justify-center sm:flex-1">
        <StatusLight status={status} onApprovals={onApprovals} />
      </div>
      <div className="flex shrink-0 items-center gap-1.5 sm:gap-2">
        <div className="dash-segment lg:hidden" role="group" aria-label={tr("Show")}>
          {(["board", "chat"] as const).map((pane) => (
            <button
              key={pane}
              type="button"
              aria-pressed={mobilePane === pane}
              onClick={() => setMobilePane(pane)}
              className="dash-segment-btn"
            >
              {pane === "board" ? tr("Board") : tr("Chat")}
            </button>
          ))}
        </div>
        <span className="dash-readout hidden md:inline-flex" title={tr("Panels on the board")}>
          <Icon name="grid" className="size-3.5" />
          <span data-testid="dashboard-panel-count">{panelCount}</span>
        </span>
        <span className="dash-readout hidden xl:inline-flex" aria-hidden="true">
          {clock}
        </span>
        <div className="relative">
          <button
            type="button"
            aria-label={tr("How dashboard view works")}
            aria-expanded={helpOpen}
            ref={helpButtonRef}
            data-testid="dashboard-help-button"
            className="dash-icon-btn"
            onClick={() => setHelpOpen((o) => !o)}
          >
            <Icon name="info" className="size-4" />
          </button>
          {helpOpen ? <HelpPopover onClose={closeHelp} onEscape={escapeHelp} /> : null}
        </div>
        <button type="button" data-testid="dashboard-exit" className="dash-exit" onClick={onExit}>
          <span>{tr("Exit")}</span>
          <kbd className="dash-kbd hidden sm:inline-flex" aria-hidden="true">
            {tr("Esc")}
          </kbd>
        </button>
      </div>
    </header>
  );
}

// ───────────────────────────── board ─────────────────────────────

const STARTERS = [
  tr("Turn what we've covered so far into dashboard panels"),
  tr("Build me a dashboard: the 3–4 numbers I should watch, plus a trend chart"),
  tr("Put the options we discussed side by side in a comparison panel"),
];

function EmptyBoard({ onStarter }: { onStarter: (text: string) => void }) {
  return (
    <div data-testid="dashboard-empty" className="flex min-h-full flex-col items-center justify-center gap-6 px-6 py-12 text-center">
      <div className="dash-radar" aria-hidden="true">
        <span />
        <span />
        <span />
      </div>
      <div className="max-w-[30rem]">
        <h2 className="text-[1.25rem] font-semibold text-[var(--color-text-primary)]">{tr("Your board is clear")}</h2>
        <p className="mt-2 text-[0.875rem] leading-[1.6] text-[var(--color-text-secondary)]">
          {tr("Ask for anything you want to keep an eye on. The assistant pins live panels here — numbers, trends, tables — and refreshes them as the conversation moves.")}
        </p>
      </div>
      <div className="flex max-w-[40rem] flex-wrap justify-center gap-2">
        {STARTERS.map((s) => (
          <button key={s} type="button" className="dash-starter" onClick={() => onStarter(s)}>
            <Icon name="sparkles" className="size-3.5 shrink-0" />
            <span>{s}</span>
          </button>
        ))}
      </div>
    </div>
  );
}

type PanelProps = {
  panel: BoardPanel;
  index: number;
  count: number;
  live: boolean;
  retired: boolean;
  storageScope: string;
  submission: Submission | null;
  reply: Reply | null;
  onSubmit: SubmitFn;
  onResumeHeld?: GenerativeCardProps["onResumeHeld"];
  onMove: (key: string, delta: number) => void;
  onToggleWide: (key: string, wide: boolean) => void;
  onRemove: (key: string) => void;
  onMaximize: (key: string) => void;
  maximized?: boolean;
  dragging: string | null;
  setDragging: (key: string | null) => void;
  onDropOn: (target: string) => void;
};

function PanelMenu({
  panel,
  index,
  count,
  onMove,
  onRemove,
}: Pick<PanelProps, "panel" | "index" | "count" | "onMove" | "onRemove">) {
  const [open, setOpen] = useState(false);
  const menuId = useId();
  const wrapRef = useRef<HTMLDivElement>(null);
  const triggerRef = useRef<HTMLButtonElement>(null);
  const menuRef = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (!open) return;
    // The menu keyboard contract: focus goes to the first enabled item;
    // ArrowUp/Down move (wrapping), Home/End jump, Tab leaves and closes.
    const items = () =>
      Array.from(menuRef.current?.querySelectorAll<HTMLButtonElement>('[role="menuitem"]:not(:disabled)') ?? []);
    items()[0]?.focus();
    const onDown = (e: MouseEvent) => {
      if (wrapRef.current && !wrapRef.current.contains(e.target as Node)) setOpen(false);
    };
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") {
        e.stopPropagation();
        setOpen(false);
        // Back to the menu's own button, not to <body>.
        triggerRef.current?.focus();
        return;
      }
      if (e.key === "Tab") {
        setOpen(false);
        return;
      }
      const list = items();
      if (list.length === 0) return;
      const at = list.indexOf(document.activeElement as HTMLButtonElement);
      let next = -1;
      if (e.key === "ArrowDown") next = (at + 1) % list.length;
      else if (e.key === "ArrowUp") next = (at - 1 + list.length) % list.length;
      else if (e.key === "Home") next = 0;
      else if (e.key === "End") next = list.length - 1;
      if (next < 0) return;
      e.preventDefault();
      e.stopPropagation();
      list[next].focus();
    };
    window.addEventListener("mousedown", onDown);
    window.addEventListener("keydown", onKey, true);
    return () => {
      window.removeEventListener("mousedown", onDown);
      window.removeEventListener("keydown", onKey, true);
    };
  }, [open]);
  // After an action the focused item unmounts: hand the focus to the
  // panel's More button (it moves with the panel), or to the board when the
  // panel itself is gone.
  const item = (label: string, action: () => void, disabled = false, removes = false) => (
    <button
      type="button"
      role="menuitem"
      disabled={disabled}
      className="dash-menu-item"
      onClick={() => {
        setOpen(false);
        action();
        window.setTimeout(() => {
          if (removes) document.querySelector<HTMLElement>('[data-testid="dashboard-board"]')?.focus();
          else triggerRef.current?.focus();
        }, 0);
      }}
    >
      {label}
    </button>
  );
  return (
    <div ref={wrapRef} className="relative">
      <button
        ref={triggerRef}
        type="button"
        className="dash-icon-btn"
        aria-label={tr("More for {0}", [panel.spec.title])}
        aria-haspopup="menu"
        aria-expanded={open}
        aria-controls={open ? menuId : undefined}
        onClick={() => setOpen((o) => !o)}
      >
        <Icon name="dots" className="size-3.5" />
      </button>
      {open ? (
        <div ref={menuRef} id={menuId} role="menu" className="dash-popover absolute right-0 top-[calc(100%+0.25rem)] z-30 grid min-w-[11rem] p-1">
          {item(tr("Move earlier"), () => onMove(panel.key, -1), index === 0)}
          {item(tr("Move later"), () => onMove(panel.key, 1), index === count - 1)}
          {item(tr("Remove from board"), () => onRemove(panel.key), false, true)}
        </div>
      ) : null}
    </div>
  );
}

function Panel(props: PanelProps) {
  const { panel, live, retired, maximized } = props;
  // The focus layer mounts a new instance of the panel: move the keyboard
  // focus into it (to its close button) so it is not dropped on <body>.
  const restoreRef = useRef<HTMLButtonElement>(null);
  useEffect(() => {
    if (maximized) restoreRef.current?.focus();
  }, [maximized]);
  // A new card in this slot (the agent refreshed it): flash the frame once.
  const [flash, setFlash] = useState(false);
  const version = `${panel.cardId}#${panel.occurrence}`;
  const seen = useRef(version);
  useEffect(() => {
    if (seen.current === version) return;
    seen.current = version;
    setFlash(true);
    const t = window.setTimeout(() => setFlash(false), 1600);
    return () => window.clearTimeout(t);
  }, [version]);
  const onDragStart = (e: ReactDragEvent) => {
    e.dataTransfer.effectAllowed = "move";
    e.dataTransfer.setData("text/plain", panel.key);
    props.setDragging(panel.key);
  };
  const dropTarget = props.dragging !== null && props.dragging !== panel.key;
  // A long conversation can hold dozens of display cards, each up to
  // thousands of rows: a panel mounts its card only once it nears the
  // viewport (and keeps it), so opening the board never builds them all at
  // once. Without IntersectionObserver (old browsers, jsdom) it mounts now.
  const sectionRef = useRef<HTMLElement>(null);
  const [nearView, setNearView] = useState(
    () => maximized === true || typeof IntersectionObserver === "undefined",
  );
  useEffect(() => {
    if (nearView) return;
    const el = sectionRef.current;
    if (!el) return;
    const io = new IntersectionObserver(
      (entries) => {
        if (entries.some((e) => e.isIntersecting)) {
          setNearView(true);
          io.disconnect();
        }
      },
      { rootMargin: "800px 0px" },
    );
    io.observe(el);
    return () => io.disconnect();
  }, [nearView]);
  return (
    // The slot is the grid cell and the drop target; the panel is the frame.
    <div
      className={["dash-slot", panel.wide && !maximized ? "dash-panel-wide" : ""].join(" ")}
      onDragOver={
        dropTarget
          ? (e) => {
              e.preventDefault();
              e.dataTransfer.dropEffect = "move";
            }
          : undefined
      }
      onDrop={
        dropTarget
          ? (e) => {
              e.preventDefault();
              props.onDropOn(panel.key);
            }
          : undefined
      }
    >
    <section
      ref={sectionRef}
      id={maximized ? undefined : panelDomId(panel.key)}
      data-testid="dashboard-panel"
      data-panel-key={panel.key}
      data-card-id={panel.cardId}
      aria-label={panel.spec.title}
      tabIndex={-1}
      className={[
        "dash-panel",
        maximized ? "dash-panel-max" : "",
        flash ? "dash-panel-flash" : "",
        live ? "dash-panel-live" : "",
        props.dragging === panel.key ? "opacity-40" : "",
      ].join(" ")}
    >
      <header className="dash-panel-head">
        {!maximized ? (
          <button
            type="button"
            className="dash-grip"
            draggable
            aria-label={tr("Reorder {0}: drag, or use the arrow keys", [panel.spec.title])}
            data-testid="dashboard-panel-grip"
            onDragStart={onDragStart}
            onDragEnd={() => props.setDragging(null)}
            onKeyDown={(e) => {
              const delta = e.key === "ArrowLeft" || e.key === "ArrowUp" ? -1 : e.key === "ArrowRight" || e.key === "ArrowDown" ? 1 : 0;
              if (delta === 0) return;
              e.preventDefault();
              props.onMove(panel.key, delta);
              // Keep the focus on this panel's grip after it moves.
              const id = panelDomId(panel.key);
              window.setTimeout(() => document.getElementById(id)?.querySelector<HTMLElement>(".dash-grip")?.focus(), 0);
            }}
          >
            <Icon name="menu" className="size-3" />
          </button>
        ) : null}
        <span
          className={["dash-led", live ? "dash-led-pulse" : "", panel.interactive ? "dash-led-accent" : ""].join(" ")}
          aria-hidden="true"
        />
        <h3 className="min-w-0 flex-1 truncate text-[0.8125rem] font-semibold text-[var(--color-text-primary)]">
          {panel.spec.title}
        </h3>
        {panel.version > 1 ? (
          <span className="dash-tag" title={tr(panel.version === 2 ? "Updated {0} time" : "Updated {0} times", [panel.version - 1])}>
            {tr("v")}{panel.version}
          </span>
        ) : null}
        {retired ? (
          <span className="dash-tag" title={tr("From before this conversation was summarized")}>
            {tr("archived")}
          </span>
        ) : null}
        <div className="flex shrink-0 items-center">
          {!maximized ? (
            <button
              type="button"
              className="dash-icon-btn hidden sm:inline-flex"
              aria-label={panel.wide ? tr("Make {0} narrow", [panel.spec.title]) : tr("Make {0} wide", [panel.spec.title])}
              aria-pressed={panel.wide}
              onClick={() => props.onToggleWide(panel.key, !panel.wide)}
            >
              <Icon name={panel.wide ? "compress" : "expand"} className="size-3.5" />
            </button>
          ) : null}
          <button
            ref={maximized ? restoreRef : undefined}
            type="button"
            className="dash-icon-btn"
            aria-label={maximized ? tr("Close focused {0}", [panel.spec.title]) : tr("Focus {0}", [panel.spec.title])}
            data-testid={maximized ? "dashboard-panel-restore" : "dashboard-panel-focus"}
            onClick={() => props.onMaximize(maximized ? "" : panel.key)}
          >
            <Icon name={maximized ? "close" : "maximize"} className="size-3.5" />
          </button>
          {!maximized ? (
            <PanelMenu panel={panel} index={props.index} count={props.count} onMove={props.onMove} onRemove={props.onRemove} />
          ) : null}
        </div>
      </header>
      <div className="dash-panel-body">
        {!nearView ? (
          <div className="min-h-[9rem]" data-testid="dashboard-panel-deferred" aria-hidden="true" />
        ) : (
        <Suspense fallback={<div className="px-3.5 py-3 text-[0.75rem] text-[var(--color-text-muted)]">{tr("Loading…")}</div>}>
          <GenerativeCard
            key={`${panel.cardId}#${panel.occurrence}`}
            variant="panel"
            cardId={panel.cardId}
            occurrence={panel.occurrence}
            spec={panel.spec}
            submission={props.submission}
            reply={props.reply}
            retired={retired}
            storageScope={props.storageScope}
            onSubmit={props.onSubmit}
            onResumeHeld={props.onResumeHeld}
          />
        </Suspense>
        )}
      </div>
    </section>
      {dropTarget ? <div className="dash-drop-hint" aria-hidden="true" /> : null}
    </div>
  );
}

export type DashboardBoardProps = {
  panels: BoardPanel[];
  /** Index of the first message the summary did not compact (-1: none). */
  summaryIndex: number;
  /** Index of the message the current turn is writing (-1 when idle). */
  liveMessageIndex: number;
  storageScope: string;
  submissions: Map<string, Submission>;
  replies: Map<string, Reply>;
  onSubmit: SubmitFn;
  onResumeHeld?: GenerativeCardProps["onResumeHeld"];
  updatePrefs: (fn: (prev: BoardPrefs) => BoardPrefs) => void;
  onStarter: (text: string) => void;
  maximized: string;
  setMaximized: (key: string) => void;
  className?: string;
};

export function DashboardBoard({
  panels,
  summaryIndex,
  liveMessageIndex,
  storageScope,
  submissions,
  replies,
  onSubmit,
  onResumeHeld,
  updatePrefs,
  onStarter,
  maximized,
  setMaximized,
  className,
}: DashboardBoardProps) {
  const [dragging, setDragging] = useState<string | null>(null);
  // Closing the focus layer (button or Esc) returns the keyboard focus to the
  // Focus button of the panel it showed, now back on the grid.
  const lastMaximized = useRef(maximized);
  useEffect(() => {
    const was = lastMaximized.current;
    lastMaximized.current = maximized;
    if (!was || maximized) return;
    document
      .getElementById(panelDomId(was))
      ?.querySelector<HTMLElement>('[data-testid="dashboard-panel-focus"]')
      ?.focus();
  }, [maximized]);
  const keys = panels.map((p) => p.key);
  const setOrder = (next: string[]) => updatePrefs((p) => ({ ...p, order: next }));
  const onMove = (key: string, delta: number) => setOrder(moveKey(keys, key, delta));
  const onDropOn = (target: string) => {
    if (dragging) setOrder(dropKey(keys, dragging, target));
    setDragging(null);
  };
  // Rebuilt from entries rather than written by computed key: a slot key is
  // derived from transcript data.
  // The new choice replaces any made under the panel's earlier keys, which
  // would otherwise win back after a refresh moves the slot to one of them.
  const onToggleWide = (key: string, wide: boolean) => {
    const stale = new Set([key, ...(panels.find((p) => p.key === key)?.aliases ?? [])]);
    updatePrefs((p) => ({
      ...p,
      wide: Object.fromEntries([...Object.entries(p.wide).filter(([k]) => !stale.has(k)), [key, wide]]),
    }));
  };
  const onRemove = (key: string) => {
    if (maximized === key) setMaximized("");
    updatePrefs((p) => ({
      ...p,
      removed: [...p.removed.filter((k) => k !== key), key],
      order: p.order.filter((k) => k !== key),
      // The pin stays: it is what gives a refreshed pinned card's lineage its
      // slot, and the removal hides that slot. Unpinning would let a
      // display-only refresh fall back to a slot of its own and reappear.
    }));
  };
  // The focused panel by its key, or by a key it had before a refresh moved
  // its slot (a pinned card that gained a panel key).
  const max = maximized ? panels.find((p) => p.key === maximized || p.aliases.includes(maximized)) : undefined;
  // Keep the focus layer's key current, and close it properly (focus back
  // to the board) if its panel left the board.
  useEffect(() => {
    if (!maximized) return;
    if (max && max.key !== maximized) setMaximized(max.key);
    else if (!max) {
      setMaximized("");
      document.querySelector<HTMLElement>('[data-testid="dashboard-board"]')?.focus();
    }
  }, [maximized, max, setMaximized]);
  const panelProps = (panel: BoardPanel, index: number) => ({
    panel,
    index,
    count: panels.length,
    live: liveMessageIndex >= 0 && panel.messageIndex === liveMessageIndex,
    retired: summaryIndex >= 0 && panel.messageIndex < summaryIndex,
    storageScope,
    submission: submissions.get(panel.cardId) ?? null,
    reply: replies.get(panel.cardId) ?? null,
    onSubmit,
    onResumeHeld,
    onMove,
    onToggleWide,
    onRemove,
    onMaximize: setMaximized,
    dragging,
    setDragging,
    onDropOn,
  });
  return (
    <div data-testid="dashboard-board" tabIndex={-1} className={["dash-board outline-none", className ?? ""].join(" ")}>
      <div className="dash-board-scroll">
        {panels.length === 0 ? (
          <EmptyBoard onStarter={onStarter} />
        ) : (
          <div className="dash-grid" aria-hidden={max ? true : undefined} inert={max ? true : undefined}>
            {panels.map((panel, i) =>
              // The focused panel is drawn once, in the focus layer: two live
              // copies of one card would edit the same answer side by side.
              panel === max ? (
                <div key={panel.key} className={["dash-panel dash-panel-ghost", panel.wide ? "dash-panel-wide" : ""].join(" ")} />
              ) : (
                <Panel key={panel.key} {...panelProps(panel, i)} />
              ),
            )}
          </div>
        )}
      </div>
      {max ? (
        <div className="dash-max-layer" data-testid="dashboard-focus-layer">
          <Panel key={`max-${max.key}-${max.cardId}`} {...panelProps(max, panels.indexOf(max))} maximized />
        </div>
      ) : null}
    </div>
  );
}
