"use client";

import { useEffect, useLayoutEffect, useRef, useState, type KeyboardEvent } from "react";
import { createPortal } from "react-dom";
import { Icon } from "@/app/shared/ui/Icon";
import {
  highlightSegments,
  seatOptions,
  type ConnectorStatus,
  type PickerConnector,
} from "./toolsPicker";

// Small pieces the Tools popover and the library share: the switch, match
// highlight, status kicker, status icon and the account menu. The design
// lists these as new to the design system (switch, match highlight, status
// kicker) and they are built from the app's existing tokens.

/** Matched text from a search, highlighted inline. */
export function Highlight({ text, query }: { text: string; query: string }) {
  return (
    <>
      {highlightSegments(text, query).map((seg, i) =>
        seg.match ? (
          <mark
            key={i}
            className="rounded-[3px] bg-[var(--color-match-bg)] text-[var(--color-text-primary)]"
          >
            {seg.text}
          </mark>
        ) : (
          <span key={i}>{seg.text}</span>
        ),
      )}
    </>
  );
}

/**
 * The on/off switch: 30×18, or 38×22 in the library detail pane. A real
 * button with role="switch", so it is reachable by Tab and announced as on
 * or off.
 */
export function ToolSwitch({
  on,
  label,
  onToggle,
  large = false,
  testId,
}: {
  on: boolean;
  label: string;
  onToggle: () => void;
  large?: boolean;
  testId?: string;
}) {
  return (
    <button
      type="button"
      role="switch"
      aria-checked={on}
      aria-label={label}
      data-testid={testId}
      onClick={(event) => {
        event.stopPropagation();
        onToggle();
      }}
      className={`hit-area-touch relative shrink-0 rounded-full transition focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] ${
        large ? "h-[1.375rem] w-[2.375rem]" : "h-[1.125rem] w-[1.875rem]"
      } ${on ? "bg-[var(--color-switch-track-on)]" : "bg-[var(--color-switch-track-off)]"}`}
    >
      <span
        aria-hidden="true"
        className={`absolute rounded-full transition-transform ${
          large ? "left-[3px] top-[3px] size-4" : "left-[2px] top-[2px] size-[0.875rem]"
        } shadow-[var(--shadow-switch-knob)] ${on ? `bg-[var(--color-switch-knob-on)] ${large ? "translate-x-4" : "translate-x-3"}` : "bg-[var(--color-switch-knob)]"}`}
      />
    </button>
  );
}

const KICKER_COLOR: Partial<Record<ConnectorStatus, string>> = {
  always: "text-[var(--color-state-ok)]",
  setup: "text-[var(--color-state-warn)]",
  unavailable: "text-[var(--color-state-deny)]",
};

/** Mono status label beside a name: ALWAYS ON / NEEDS SETUP / UNAVAILABLE. */
export function StatusKicker({ status, label }: { status: ConnectorStatus; label: string }) {
  if (!label) return null;
  return (
    <span
      className={`shrink-0 whitespace-nowrap font-[family-name:var(--font-code)] text-[0.625rem] font-bold uppercase tracking-[0.08em] ${KICKER_COLOR[status] ?? ""}`}
    >
      {label}
    </span>
  );
}

const DOT_COLOR: Record<ConnectorStatus, string> = {
  on: "bg-[var(--color-accent)]",
  off: "bg-[var(--color-text-subtle)]",
  always: "bg-[var(--color-state-ok)]",
  setup: "bg-[var(--color-state-warn)]",
  unavailable: "bg-[var(--color-state-deny)]",
};

export function StatusDot({ status }: { status: ConnectorStatus }) {
  return <span aria-hidden="true" className={`size-[7px] shrink-0 rounded-full ${DOT_COLOR[status]}`} />;
}

/** The library row's right-hand icon for connections without a switch. */
export function StatusIcon({ status }: { status: ConnectorStatus }) {
  if (status === "always") {
    return (
      <span title="Always on in every chat." className="text-[var(--color-text-subtle)]">
        <Icon name="lock" className="size-3.5" />
      </span>
    );
  }
  if (status === "setup") {
    return (
      <span title="Needs setup" className="text-[var(--color-state-warn)]">
        <Icon name="alert-circle" className="size-[0.95rem]" />
      </span>
    );
  }
  if (status === "unavailable") {
    return (
      <span title="Unavailable" className="text-[var(--color-state-deny)]">
        <Icon name="alert-triangle" className="size-[0.95rem]" />
      </span>
    );
  }
  return null;
}

/**
 * Which account (credential seat) a connection uses in this chat. A styled
 * menu rather than a native select: the default seat reads as the deployment
 * ("Elcano") or the connection's named default, marked "Default", with a check
 * on the current choice. "compact" is the pill on a popover row; "field" is
 * the full-width control in the library detail pane.
 *
 * The list is portalled to <body> and positioned from the trigger, so the
 * popover's scrolling list never clips it. Clicks and keys stop at the menu,
 * so choosing an account never toggles the row it sits on.
 */
export function SeatMenu({
  connector,
  appName,
  onChange,
  variant,
}: {
  connector: PickerConnector;
  appName: string;
  onChange: (account: string) => void;
  variant: "compact" | "field";
}) {
  const options = seatOptions(connector, appName);
  const current = connector.account ?? "";
  const selected = options.find((o) => o.value === current) ?? options[0];
  const [open, setOpen] = useState(false);
  const [active, setActive] = useState(0);
  const [pos, setPos] = useState<{ left: number; top?: number; bottom?: number; width: number } | null>(null);
  const triggerRef = useRef<HTMLButtonElement | null>(null);
  const listRef = useRef<HTMLDivElement | null>(null);
  const name = connector.display_name || connector.name;
  const listId = `seat-menu-${connector.name}`;

  const place = () => {
    const el = triggerRef.current;
    if (!el) return;
    const r = el.getBoundingClientRect();
    const width = Math.max(r.width, 176);
    const left = Math.min(r.left, window.innerWidth - width - 8);
    // Open upward when there is no room below (the composer sits at the bottom).
    const below = window.innerHeight - r.bottom;
    setPos(
      below < 220
        ? { left, bottom: window.innerHeight - r.top + 4, width }
        : { left, top: r.bottom + 4, width },
    );
  };

  useLayoutEffect(() => {
    if (open) place();
  }, [open]);

  useEffect(() => {
    if (!open) return;
    listRef.current?.focus();
    const onDown = (event: MouseEvent) => {
      const t = event.target as Node;
      if (triggerRef.current?.contains(t) || listRef.current?.contains(t)) return;
      setOpen(false);
    };
    const onMove = () => setOpen(false);
    document.addEventListener("mousedown", onDown);
    window.addEventListener("resize", onMove);
    // Any scroll except the menu's own closes it rather than leaving it adrift.
    const onScroll = (event: Event) => {
      if (listRef.current?.contains(event.target as Node)) return;
      setOpen(false);
    };
    window.addEventListener("scroll", onScroll, true);
    return () => {
      document.removeEventListener("mousedown", onDown);
      window.removeEventListener("resize", onMove);
      window.removeEventListener("scroll", onScroll, true);
    };
  }, [open]);

  const openMenu = () => {
    setActive(Math.max(0, options.indexOf(selected)));
    setOpen(true);
  };
  const choose = (value: string) => {
    setOpen(false);
    triggerRef.current?.focus();
    if (value !== current) onChange(value);
  };

  const onTriggerKey = (event: KeyboardEvent<HTMLButtonElement>) => {
    if (event.key === "ArrowDown" || event.key === "ArrowUp") {
      event.preventDefault();
      event.stopPropagation();
      openMenu();
    }
  };
  const onListKey = (event: KeyboardEvent<HTMLDivElement>) => {
    event.stopPropagation();
    if (event.key === "ArrowDown" || event.key === "ArrowUp") {
      event.preventDefault();
      const d = event.key === "ArrowDown" ? 1 : -1;
      setActive((a) => (a + d + options.length) % options.length);
    } else if (event.key === "Home" || event.key === "End") {
      event.preventDefault();
      setActive(event.key === "Home" ? 0 : options.length - 1);
    } else if (event.key === "Enter" || event.key === " ") {
      event.preventDefault();
      choose(options[active].value);
    } else if (event.key === "Escape") {
      event.preventDefault();
      setOpen(false);
      triggerRef.current?.focus();
    } else if (event.key === "Tab") {
      setOpen(false);
    }
  };

  return (
    <>
      <button
        ref={triggerRef}
        type="button"
        aria-haspopup="listbox"
        aria-expanded={open}
        aria-controls={open ? listId : undefined}
        aria-label={`Account for ${name}: ${selected.label}`}
        data-testid={`mcp-seat-${connector.name}`}
        onClick={(event) => {
          event.stopPropagation();
          if (open) setOpen(false);
          else openMenu();
        }}
        onKeyDown={onTriggerKey}
        className={
          variant === "compact"
            ? "hit-area-touch inline-flex h-[1.125rem] min-w-0 max-w-[10rem] items-center gap-1 rounded-[var(--radius-pill)] border border-[var(--color-pill-border)] px-[0.45rem] font-[family-name:var(--font-sans)] text-[0.66rem] text-[var(--color-text-secondary)] transition hover:bg-[var(--color-pill-hover-bg)] hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]"
            : "flex h-9 w-full items-center gap-2 rounded-[var(--radius-sm)] any-pointer-coarse:h-11 border border-[var(--color-selected-border)] bg-[var(--color-bg)] px-3 text-left text-[0.82rem] text-[var(--color-text-primary)] transition hover:border-[var(--color-accent)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]"
        }
      >
        {variant === "field" ? (
          <Icon name="persona" className="size-3.5 shrink-0 text-[var(--color-text-subtle)]" />
        ) : null}
        <span className="min-w-0 flex-1 truncate">{selected.label}</span>
        {variant === "field" && selected.isDefault ? (
          <span className="shrink-0 text-[0.7rem] text-[var(--color-text-subtle)]">Default</span>
        ) : null}
        <Icon
          name="chevron-down"
          className={`shrink-0 transition-transform ${variant === "compact" ? "size-2.5" : "size-3.5"} ${open ? "rotate-180" : ""}`}
        />
      </button>
      {open && pos
        ? createPortal(
            <div
              ref={listRef}
              id={listId}
              role="listbox"
              data-seat-menu=""
              tabIndex={-1}
              aria-label={`Account for ${name}`}
              aria-activedescendant={`${listId}-${active}`}
              onKeyDown={onListKey}
              onClick={(event) => event.stopPropagation()}
              style={{ left: pos.left, top: pos.top, bottom: pos.bottom, minWidth: pos.width }}
              className="fixed z-[1300] grid max-h-64 max-w-[18rem] gap-[0.1rem] overflow-y-auto rounded-[var(--radius-md)] border border-[var(--color-selected-border)] bg-[var(--color-surface-2)] p-[0.3rem] shadow-[var(--shadow-popover)] outline-none motion-safe:animate-pop-up"
            >
              <div className="px-[0.55rem] pb-[0.2rem] pt-[0.15rem] font-[family-name:var(--font-code)] text-[0.62rem] uppercase tracking-[0.12em] text-[var(--color-text-subtle)]">
                Account
              </div>
              {options.map((o, i) => {
                const isSel = o.value === current;
                return (
                  <div
                    key={o.value || "__default"}
                    id={`${listId}-${i}`}
                    role="option"
                    aria-selected={isSel}
                    tabIndex={-1}
                    onMouseEnter={() => setActive(i)}
                    onClick={() => choose(o.value)}
                    onKeyDown={() => undefined}
                    className={`flex cursor-pointer items-center gap-2 rounded-[0.45rem] px-[0.55rem] py-[0.35rem] text-[0.78rem] ${
                      i === active ? "bg-[var(--color-selected-bg)]" : ""
                    } ${isSel ? "text-[var(--color-text-primary)]" : "text-[var(--color-text-secondary)]"}`}
                  >
                    <span className="min-w-0 flex-1 truncate">{o.label}</span>
                    {o.isDefault ? (
                      <span className="shrink-0 text-[0.66rem] text-[var(--color-text-subtle)]">Default</span>
                    ) : null}
                    <span className="flex size-3.5 shrink-0 items-center justify-center text-[var(--color-accent)]">
                      {isSel ? <Icon name="check" className="size-3.5" /> : null}
                    </span>
                  </div>
                );
              })}
            </div>,
            document.body,
          )
        : null}
    </>
  );
}
