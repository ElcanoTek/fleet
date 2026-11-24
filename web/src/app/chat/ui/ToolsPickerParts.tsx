import { t as tr } from "@/app/lib/i18n";
import { useEffect, useLayoutEffect, useRef, useState, type KeyboardEvent } from "react";
import { createPortal } from "react-dom";
import { Icon } from "@/app/shared/ui/Icon";
import { Switch } from "@/components/ui/switch";
import {
  MAX_ACCOUNTS_PER_CONNECTOR,
  allowsSeveralAccounts,
  selectedAccounts,
  toggleAccount,
} from "./mcpAccounts";
import {
  highlightSegments,
  seatLabel,
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
    <Switch
      size={large ? "lg" : "default"}
      checked={on}
      aria-label={label}
      data-testid={testId}
      onCheckedChange={() => onToggle()}
      onClick={(event) => event.stopPropagation()}
      className="hit-area-touch"
    />
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
      <span title={tr("Always on in every chat.")} className="text-[var(--color-text-subtle)]">
        <Icon name="lock" className="size-3.5" />
      </span>
    );
  }
  if (status === "setup") {
    return (
      <span title={tr("Needs setup")} className="text-[var(--color-state-warn)]">
        <Icon name="alert-circle" className="size-[0.95rem]" />
      </span>
    );
  }
  if (status === "unavailable") {
    return (
      <span title={tr("Unavailable")} className="text-[var(--color-state-deny)]">
        <Icon name="alert-triangle" className="size-[0.95rem]" />
      </span>
    );
  }
  return null;
}

/**
 * Which accounts (credential seats) a connection uses in this chat. A styled
 * menu rather than a native select: the default seat reads as the deployment
 * ("Elcano") or the connection's named default, marked "Default". "compact"
 * is the pill on a popover row; "field" is the full-width control in the
 * library detail pane.
 *
 * The menu has one behaviour at a time:
 *
 *  - **Switch** (how it opens): a plain list with a check on the current
 *    account. Clicking an account switches to it and closes the menu.
 *  - **Several** (docs/MULTI-ACCOUNT-CHATS.md): a bundled connector's menu has
 *    a "Use several accounts" switch at its foot. On, the list is a checklist:
 *    each row ticks or unticks one account and the menu stays open. The last
 *    ticked account cannot be unticked (the connector's switch turns it off),
 *    and no more than MAX_ACCOUNTS_PER_CONNECTOR can be ticked. It is on, and
 *    cannot be turned off, while the chat already runs the connector as more
 *    than one account: turning it off would have to drop accounts nobody
 *    asked to drop.
 *
 * The trigger reads the account, or the first one and how many more
 * ("Raptive +3"). A hosted connection mounts one account per chat and has no
 * switch.
 *
 * The menu is portalled to <body> and positioned from the trigger, so the
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
  onChange: (accounts: string[]) => void;
  variant: "compact" | "field";
}) {
  const options = seatOptions(connector, appName);
  const canUseSeveral = allowsSeveralAccounts(connector);
  const chosen = selectedAccounts(connector);
  // The menu lists the user's default account once, as the "" option. A
  // choice that names that account outright (the server stores the name when
  // a list holds both) is the same row, so it reads as ticked and a click on
  // it acts on the name.
  const namedDefault = connector.default_account ?? "";
  const stored = (value: string) =>
    value === "" && namedDefault && !chosen.includes("") && chosen.includes(namedDefault) ? namedDefault : value;
  const isChosen = (value: string) => chosen.includes(stored(value));
  const selected = options.find((o) => isChosen(o.value)) ?? options[0];
  const triggerLabel = seatLabel(connector, appName);
  const atCap = chosen.length >= MAX_ACCOUNTS_PER_CONNECTOR;
  // "Use several accounts", as the person set it for this menu. A chat that
  // already runs several accounts is in that mode whatever this says.
  const [severalAsked, setSeveralAsked] = useState(false);
  const severalLocked = chosen.length > 1;
  const several = canUseSeveral && (severalAsked || severalLocked);
  const [open, setOpen] = useState(false);
  const [active, setActive] = useState(0);
  const [pos, setPos] = useState<{ left: number; top?: number; bottom?: number; width: number } | null>(null);
  const triggerRef = useRef<HTMLButtonElement | null>(null);
  const menuRef = useRef<HTMLDivElement | null>(null);
  const listRef = useRef<HTMLDivElement | null>(null);
  const severalRef = useRef<HTMLButtonElement | null>(null);
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
      if (triggerRef.current?.contains(t) || menuRef.current?.contains(t)) return;
      setOpen(false);
    };
    const onMove = () => setOpen(false);
    document.addEventListener("mousedown", onDown);
    window.addEventListener("resize", onMove);
    // Any scroll except the menu's own closes it rather than leaving it adrift.
    const onScroll = (event: Event) => {
      if (menuRef.current?.contains(event.target as Node)) return;
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
  const closeMenu = () => {
    setOpen(false);
    // Each opening starts as a switch list again, unless the chat itself
    // holds several accounts.
    setSeveralAsked(false);
    triggerRef.current?.focus();
  };
  // An option that a click would not change: the last ticked account, or an
  // unticked one once the cap is reached.
  const locked = (value: string) =>
    several && (isChosen(value) ? chosen.length === 1 : atCap);
  const choose = (option: string) => {
    const value = stored(option);
    if (several) {
      // The checklist stays open: ticking a second account is the point.
      const next = toggleAccount(chosen, value);
      if (next.length !== chosen.length) onChange(next);
      return;
    }
    closeMenu();
    if (!isChosen(option)) onChange([value]);
  };

  const onTriggerKey = (event: KeyboardEvent<HTMLButtonElement>) => {
    if (event.key === "ArrowDown" || event.key === "ArrowUp") {
      event.preventDefault();
      event.stopPropagation();
      openMenu();
    }
  };
  // Escape and Tab belong to the whole menu, the switch at its foot included.
  const onMenuKey = (event: KeyboardEvent<HTMLDivElement>) => {
    event.stopPropagation();
    if (event.key === "Escape") {
      event.preventDefault();
      closeMenu();
    } else if (event.key === "Tab") {
      setOpen(false);
      setSeveralAsked(false);
    }
  };
  const onListKey = (event: KeyboardEvent<HTMLDivElement>) => {
    if (event.key === "ArrowDown" && canUseSeveral && active === options.length - 1) {
      // Past the last account is the "Use several accounts" switch.
      event.preventDefault();
      severalRef.current?.focus();
    } else if (event.key === "ArrowDown" || event.key === "ArrowUp") {
      event.preventDefault();
      const d = event.key === "ArrowDown" ? 1 : -1;
      setActive((a) => (a + d + options.length) % options.length);
    } else if (event.key === "Home" || event.key === "End") {
      event.preventDefault();
      setActive(event.key === "Home" ? 0 : options.length - 1);
    } else if (event.key === "Enter" || event.key === " ") {
      event.preventDefault();
      choose(options[active].value);
    }
  };
  const onSeveralKey = (event: KeyboardEvent<HTMLButtonElement>) => {
    if (event.key === "ArrowUp") {
      event.preventDefault();
      setActive(options.length - 1);
      listRef.current?.focus();
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
        aria-label={
          chosen.length > 1
            ? tr("Accounts for {0}: {1}", [
                name,
                options.filter((o) => isChosen(o.value)).map((o) => o.label).join(", "),
              ])
            : tr("Account for {0}: {1}", [name, selected.label])
        }
        data-testid={`mcp-seat-${connector.name}`}
        onClick={(event) => {
          event.stopPropagation();
          if (open) closeMenu();
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
        <span className="min-w-0 flex-1 truncate">{triggerLabel}</span>
        {variant === "field" && chosen.length === 1 && selected.isDefault ? (
          <span className="shrink-0 text-[0.7rem] text-[var(--color-text-subtle)]">{tr("Default")}</span>
        ) : null}
        <Icon
          name="chevron-down"
          className={`shrink-0 transition-transform ${variant === "compact" ? "size-2.5" : "size-3.5"} ${open ? "rotate-180" : ""}`}
        />
      </button>
      {open && pos
        ? createPortal(
            // The menu is a pointer and key boundary only; its controls are
            // the listbox and the switch inside it.
            // oxlint-disable-next-line jsx-a11y/no-static-element-interactions
            <div
              ref={menuRef}
              data-seat-menu=""
              onKeyDown={onMenuKey}
              onClick={(event) => event.stopPropagation()}
              style={{ left: pos.left, top: pos.top, bottom: pos.bottom, minWidth: pos.width }}
              className="fixed z-[1300] flex max-h-72 max-w-[18rem] flex-col rounded-[var(--radius-md)] border border-[var(--color-selected-border)] bg-[var(--color-surface-2)] p-[0.3rem] shadow-[var(--shadow-popover)] motion-safe:animate-pop-up"
            >
              <div className="px-[0.55rem] pb-[0.2rem] pt-[0.15rem] font-[family-name:var(--font-code)] text-[0.62rem] uppercase tracking-[0.12em] text-[var(--color-text-subtle)]">
                {several ? tr("Accounts") : tr("Account")}
              </div>
              <div
                ref={listRef}
                id={listId}
                role="listbox"
                aria-multiselectable={several || undefined}
                tabIndex={-1}
                aria-label={several ? tr("Accounts for {0}", [name]) : tr("Account for {0}", [name])}
                aria-activedescendant={`${listId}-${active}`}
                onKeyDown={onListKey}
                className="grid min-h-0 gap-[0.1rem] overflow-y-auto outline-none"
              >
                {options.map((o, i) => {
                  const isSel = isChosen(o.value);
                  const isLocked = locked(o.value);
                  return (
                    <div
                      key={o.value || "__default"}
                      id={`${listId}-${i}`}
                      role="option"
                      aria-selected={isSel}
                      aria-disabled={isLocked || undefined}
                      tabIndex={-1}
                      onMouseEnter={() => setActive(i)}
                      onClick={() => choose(o.value)}
                      onKeyDown={() => undefined}
                      title={
                        !isLocked
                          ? undefined
                          : isSel
                            ? tr("At least one account stays on. Use the switch to turn the connector off.")
                            : tr("Up to {0} accounts at once.", [MAX_ACCOUNTS_PER_CONNECTOR])
                      }
                      className={`flex items-center gap-2 rounded-[0.45rem] px-[0.55rem] py-[0.35rem] text-[0.78rem] any-pointer-coarse:min-h-11 ${
                        isLocked && !isSel ? "cursor-not-allowed opacity-55" : "cursor-pointer"
                      } ${i === active ? "bg-[var(--color-selected-bg)]" : ""} ${
                        isSel ? "text-[var(--color-text-primary)]" : "text-[var(--color-text-secondary)]"
                      }`}
                    >
                      {several ? (
                        <span
                          aria-hidden="true"
                          className={`flex size-3.5 shrink-0 items-center justify-center rounded-[3px] border ${
                            isSel
                              ? "border-[var(--color-accent)] bg-[var(--color-status-running-bg)] text-[var(--color-accent)]"
                              : "border-[var(--color-pill-border)]"
                          }`}
                        >
                          {isSel ? <Icon name="check" className="size-3" /> : null}
                        </span>
                      ) : null}
                      <span className="min-w-0 flex-1 truncate">{o.label}</span>
                      {o.isDefault ? (
                        <span className="shrink-0 text-[0.66rem] text-[var(--color-text-subtle)]">{tr("Default")}</span>
                      ) : null}
                      {several ? null : (
                        <span className="flex size-3.5 shrink-0 items-center justify-center text-[var(--color-accent)]">
                          {isSel ? <Icon name="check" className="size-3.5" /> : null}
                        </span>
                      )}
                    </div>
                  );
                })}
              </div>
              {several && atCap ? (
                <div className="px-[0.55rem] pb-[0.15rem] pt-[0.3rem] text-[0.66rem] text-[var(--color-text-subtle)]">
                  {tr("Up to {0} accounts at once.", [MAX_ACCOUNTS_PER_CONNECTOR])}
                </div>
              ) : null}
              {canUseSeveral ? (
                <div
                  className="mt-[0.3rem] flex items-center gap-2 border-t border-[var(--color-divider)] px-[0.55rem] pb-[0.2rem] pt-[0.45rem] text-[0.72rem] text-[var(--color-text-secondary)] any-pointer-coarse:min-h-11"
                  title={severalLocked ? tr("On while this chat uses more than one account. Untick accounts down to one to turn it off.") : undefined}
                >
                  <span className="min-w-0 flex-1">{tr("Use several accounts")}</span>
                  <Switch
                    ref={severalRef}
                    checked={several}
                    aria-label={tr("Use several accounts")}
                    aria-disabled={severalLocked || undefined}
                    data-testid={`mcp-seat-several-${connector.name}`}
                    onKeyDown={onSeveralKey}
                    onCheckedChange={(next) => {
                      if (!severalLocked) setSeveralAsked(next);
                    }}
                    className={`hit-area-touch ${severalLocked ? "cursor-not-allowed opacity-60" : ""}`}
                  />
                </div>
              ) : null}
            </div>,
            document.body,
          )
        : null}
    </>
  );
}
