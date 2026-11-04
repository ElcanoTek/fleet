"use client";

import { lazy, Suspense, useCallback, useEffect, useRef, useState, type ReactNode } from "react";
import { createPortal } from "react-dom";
import { formatCompactCronCadence } from "@/app/shared/lib/patrolSchedule";
import { Icon } from "@/app/shared/ui/Icon";
import { Menu, MenuItem, MenuSeparator } from "@/app/shared/ui/Menu";

// LaunchpadButton — the rail's "Patrols" item (NavRail), between Chat and
// Background tasks: a patrol is a chat that checks in on a schedule.
//
// Patrols LIVE IN CHAT; this item is their index and their front door, never
// a second home for them. With no patrols yet it opens the Patrol Launchpad
// wizard straight away. Once you have some, it opens a short menu: your live
// patrols (each opens its chat) and "New patrol" (the wizard). Until someone
// has opened it once, a small pulsing dot draws the eye; `?launchpad=1` on any
// page opens the wizard directly (the guides and docs link it that way).
//
// The wizard itself is lazy-loaded: it is not on the path to a first paint.

const PatrolLaunchpad = lazy(() => import("./PatrolLaunchpad"));

export const LAUNCHPAD_SEEN_ITEM = "fleet.launchpad.seen";
export const LAUNCHPAD_PARAM = "launchpad";

/** One live patrol, as GET /api/patrols lists it. */
export type PatrolSummary = {
  id: string;
  conversation_id: string;
  title: string;
  conversation_title: string;
  cron?: string;
  timezone?: string;
  status: "active" | "paused" | string;
};

function readSeen(): boolean {
  try {
    return window.localStorage.getItem(LAUNCHPAD_SEEN_ITEM) === "1";
  } catch {
    return true; // No storage: never nag.
  }
}

function readDeepLink(): boolean {
  if (typeof window === "undefined") return false;
  return new URLSearchParams(window.location.search).get(LAUNCHPAD_PARAM) === "1";
}

/**
 * cadenceWithZone names a patrol's rhythm, with its time zone when the
 * schedule fires at a clock time ("Weekdays 08:00 · America/New_York") —
 * an interval ("Every 2h") reads the same in every zone.
 */
export function cadenceWithZone(cron: string, timezone: string): string {
  const cadence = formatCompactCronCadence(cron);
  const interval = /^(\*|0)\s+(\*|\*\/\d+)\s/.test(cron.trim()) || /^\*\/\d+\s/.test(cron.trim());
  const tz = timezone.trim();
  return tz && !interval ? `${cadence} · ${tz}` : cadence;
}

/**
 * fetchPatrols reads the index. A failure is null, never [] — "could not
 * load" must not read as "you have no patrols", or a hiccup would hide every
 * patrol behind the first-time wizard.
 */
async function fetchPatrols(): Promise<PatrolSummary[] | null> {
  try {
    const res = await fetch("/api/patrols", { cache: "no-store" });
    if (!res.ok) return null;
    const data = (await res.json()) as { patrols?: PatrolSummary[] };
    return Array.isArray(data.patrols) ? data.patrols : null;
  } catch {
    return null;
  }
}

export type LaunchpadButtonProps = {
  // Rail styling, supplied by NavRail so the item matches Chat and Background tasks
  // exactly (row, icon tile, label, collapsed tooltip).
  className?: string;
  tileClassName?: string;
  // The item's label; NavRail passes the name with its subtitle.
  label?: ReactNode;
  dataTip?: string;
  // Collapsed rail: the menu opens at a fixed width beside the icon strip.
  collapsed?: boolean;
  // Lets the rail close its phone drawer when the wizard or a chat opens.
  onOpen?: () => void;
};

export function LaunchpadButton({
  className,
  tileClassName,
  label,
  dataTip,
  collapsed = false,
  onOpen,
}: LaunchpadButtonProps = {}) {
  const [wizardOpen, setWizardOpen] = useState(readDeepLink);
  const [menuOpen, setMenuOpen] = useState(false);
  const [patrols, setPatrols] = useState<PatrolSummary[] | null>(null);
  const [loadFailed, setLoadFailed] = useState(false);
  const [seen, setSeen] = useState(() => readSeen() || readDeepLink());
  const anchorRef = useRef<HTMLButtonElement | null>(null);
  // Every read of the index takes a generation and only the latest one's
  // answer lands, so a slow mount read cannot overwrite a click's fresher one.
  const readGen = useRef(0);
  const readPatrols = useCallback(async (): Promise<PatrolSummary[] | null | undefined> => {
    const gen = ++readGen.current;
    const list = await fetchPatrols();
    return gen === readGen.current ? list : undefined; // undefined: superseded
  }, []);

  // Refresh the index without opening anything, so the button can say what it
  // opens before the next press (a menu when there are patrols, the wizard
  // when there are none). Until that is known it declares no popup type
  // rather than a wrong one. Clicks still re-read it, since patrols change in
  // other tabs.
  const refreshIndex = useCallback(async () => {
    const list = await readPatrols();
    if (list === undefined) return;
    setLoadFailed(list === null);
    setPatrols(list);
  }, [readPatrols]);

  useEffect(() => {
    let cancelled = false;
    const gen = ++readGen.current;
    void fetchPatrols().then((list) => {
      if (cancelled || gen !== readGen.current) return;
      setLoadFailed(list === null);
      setPatrols(list);
    });
    return () => {
      cancelled = true;
    };
  }, []);

  // Consume the one-shot deep link so a reload or a shared URL does not
  // reopen the wizard forever.
  useEffect(() => {
    if (!readDeepLink()) return;
    const url = new URL(window.location.href);
    url.searchParams.delete(LAUNCHPAD_PARAM);
    window.history.replaceState(window.history.state, "", url.pathname + url.search + url.hash);
  }, []);

  // Opened (by click or deep link) counts as seen: the dot is for discovery.
  useEffect(() => {
    if (!wizardOpen && !menuOpen) return;
    try {
      window.localStorage.setItem(LAUNCHPAD_SEEN_ITEM, "1");
    } catch {
      // Storage unavailable: the dot simply stays.
    }
  }, [wizardOpen, menuOpen]);

  const openWizard = useCallback(() => {
    // Opened from the menu, the focused item unmounts in the same commit as
    // the dialog mounts, so the dialog would record the page body as its
    // opener and lose the focus on close. Put the focus on the rail item
    // first: closing the wizard then hands it back there.
    anchorRef.current?.focus();
    setMenuOpen(false);
    setWizardOpen(true);
    onOpen?.();
  }, [onOpen]);

  // A click asks the server what exists right now (a patrol launched or
  // stopped in another tab counts): none → the wizard, some → the menu.
  // A failed read opens the menu with the error, Retry and New patrol.
  const onClick = async () => {
    setSeen(true);
    const list = await readPatrols();
    if (list === undefined) return; // a newer read (another click) wins
    setLoadFailed(list === null);
    setPatrols(list ?? []);
    if (list !== null && list.length === 0) openWizard();
    else setMenuOpen(true);
  };

  const openChat = (conversationId: string) => {
    setMenuOpen(false);
    onOpen?.();
    window.location.assign(`/chat?c=${encodeURIComponent(conversationId)}`);
  };

  return (
    <>
      <button
        ref={anchorRef}
        type="button"
        data-testid="launchpad-button"
        aria-haspopup={
          loadFailed || (patrols !== null && patrols.length > 0) ? "menu" : patrols !== null ? "dialog" : undefined
        }
        aria-expanded={menuOpen || undefined}
        data-tip={dataTip}
        title={dataTip ? undefined : "Your patrols, and the Launchpad to set up a new one"}
        onClick={() => void onClick()}
        className={className}
      >
        <span className={`relative ${tileClassName ?? ""}`} aria-hidden="true">
          <Icon name="sparkles" className="size-[1rem]" />
          {!seen ? (
            <span className="absolute -right-1 -top-1 flex size-2.5" data-testid="launchpad-new-dot">
              <span className="absolute inline-flex size-full rounded-full bg-[var(--color-primary-fill)] opacity-70 motion-safe:animate-ping" />
              <span className="relative inline-flex size-2.5 rounded-full bg-[var(--color-primary-fill)]" />
            </span>
          ) : null}
        </span>
        {label ?? <span>Patrols</span>}
      </button>

      <Menu
        open={menuOpen}
        onClose={() => setMenuOpen(false)}
        anchorRef={anchorRef}
        placement={collapsed ? "top-start" : "bottom-end"}
        label="Patrols"
        className={collapsed ? "" : "min-w-[17rem]"}
      >
        {loadFailed ? (
          <>
            <p role="alert" className="px-2 pb-1 pt-1.5 text-[0.75rem] leading-snug text-[var(--color-danger)]" data-testid="patrols-menu-error">
              Couldn’t load your patrols. They are safe in Chat.
            </p>
            <MenuItem
              icon={<Icon name="refresh" className="size-4" />}
              testId="patrols-menu-retry"
              onClick={() => {
                setMenuOpen(false);
                void onClick();
              }}
            >
              Try again
            </MenuItem>
          </>
        ) : (
          <p className="px-2 pb-1 pt-1.5 text-[0.72rem] leading-snug text-[var(--color-text-secondary)]" data-testid="patrols-menu-hint">
            Each patrol is a chat that wakes on its schedule. Pick one to open it in Chat.
          </p>
        )}
        {(patrols ?? []).map((p) => (
          <MenuItem
            key={p.id}
            icon={<Icon name={p.status === "paused" ? "clock" : "activity"} className="size-4" />}
            description={[
              p.conversation_title && p.conversation_title !== p.title ? p.conversation_title : "",
              cadenceWithZone(p.cron ?? "", p.timezone ?? ""),
              p.status === "paused" ? "Paused" : "",
            ]
              .filter(Boolean)
              .join(" · ")}
            testId={`patrols-menu-item-${p.id}`}
            onClick={() => openChat(p.conversation_id)}
          >
            {p.title || p.conversation_title || "Patrol"}
          </MenuItem>
        ))}
        <MenuSeparator />
        <MenuItem
          icon={<Icon name="sparkles" className="size-4" />}
          description="Set one up with the Patrol Launchpad"
          testId="patrols-menu-new"
          onClick={openWizard}
        >
          New patrol
        </MenuItem>
      </Menu>

      {/* Portalled: the rail is a transformed drawer on phones, which would
          otherwise become the dialog's containing block. */}
      {wizardOpen && typeof document !== "undefined"
        ? createPortal(
            <Suspense fallback={null}>
              <PatrolLaunchpad
                onClose={() => {
                  setWizardOpen(false);
                  // The wizard may just have launched the first patrol: re-read
                  // so the button now announces the menu, not the wizard.
                  void refreshIndex();
                }}
              />
            </Suspense>,
            document.body,
          )
        : null}
    </>
  );
}
