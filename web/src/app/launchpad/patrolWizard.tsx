import { lazy, Suspense, useEffect, useSyncExternalStore } from "react";
import { createPortal } from "react-dom";
import { refreshPatrols } from "@/app/shared/sidebar/targets";

// The Patrol Launchpad wizard, as something any part of the page can open
// (docs/PATROL-LAUNCHPAD.md): the sidebar's + menu ("New patrol"), the
// Patrols page ("Use guided setup", "Set up your first patrol"), and the
// one-shot `?launchpad=1` link the guides use. One host per page mounts it
// (the sidebar frame, NavRail), so there is one wizard however it was asked
// for. The wizard itself is lazy-loaded: it is not on the path to a first
// paint.

const PatrolLaunchpad = lazy(() => import("./PatrolLaunchpad"));

export const LAUNCHPAD_PARAM = "launchpad";

function readDeepLink(): boolean {
  if (typeof window === "undefined") return false;
  return new URLSearchParams(window.location.search).get(LAUNCHPAD_PARAM) === "1";
}

let open = false;
// What had the focus when the wizard was asked for. The dialog hands the
// focus back to its opener when it closes, but a menu item that opened it is
// gone by then; this is where the focus goes instead.
let opener: HTMLElement | null = null;
const listeners = new Set<() => void>();
const emit = () => {
  for (const l of listeners) l();
};

/** Opens the wizard. `returnFocusTo` is where the focus goes when it closes. */
export function openPatrolWizard(returnFocusTo?: HTMLElement | null) {
  opener = returnFocusTo ?? (document.activeElement instanceof HTMLElement ? document.activeElement : null);
  if (open) return;
  open = true;
  emit();
}

function closePatrolWizard() {
  if (!open) return;
  open = false;
  emit();
}

function subscribe(l: () => void) {
  listeners.add(l);
  return () => listeners.delete(l);
}

export function usePatrolWizardOpen(): boolean {
  return useSyncExternalStore(subscribe, () => open, () => false);
}

// onScreen: laid out, and at least partly inside the viewport (an element in
// a drawer translated off-canvas is neither).
function onScreen(el: Element): boolean {
  const r = el.getBoundingClientRect();
  return r.width > 0 && r.height > 0 && r.right > 0 && r.bottom > 0 && r.left < window.innerWidth && r.top < window.innerHeight;
}

/** Test hook. */
export function resetPatrolWizardForTests() {
  open = false;
  opener = null;
}

/**
 * PatrolWizardHost mounts the wizard while it is open. `onOpen` lets the
 * sidebar close its phone drawer underneath.
 */
export function PatrolWizardHost({ onOpen }: { onOpen?: () => void }) {
  const isOpen = usePatrolWizardOpen();

  // The one-shot deep link: open, and take the parameter back out of the
  // address bar so a reload or a shared URL does not reopen the wizard for
  // ever.
  useEffect(() => {
    if (!readDeepLink()) return;
    const url = new URL(window.location.href);
    url.searchParams.delete(LAUNCHPAD_PARAM);
    window.history.replaceState(window.history.state, "", url.pathname + url.search + url.hash);
    openPatrolWizard(null);
  }, []);

  useEffect(() => {
    if (isOpen) onOpen?.();
    // onOpen is the caller's "close the drawer"; only opening calls it.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [isOpen]);

  if (!isOpen || typeof document === "undefined") return null;
  // Portalled: the sidebar is a transformed drawer on phones, which would
  // otherwise become the dialog's containing block.
  return createPortal(
    <Suspense fallback={null}>
      <PatrolLaunchpad
        onClose={() => {
          const back = opener;
          opener = null;
          closePatrolWizard();
          // Hand the focus back to what opened the wizard. On a phone the
          // drawer it sits in was closed when the wizard opened, so it is now
          // off-screen (and invisible, so it cannot take focus at all): the
          // focus goes to the visible control that reopens the drawer.
          // Focus put anywhere else on purpose is left alone.
          requestAnimationFrame(() => {
            const active = document.activeElement;
            if (active && active !== back && active !== document.body) return;
            if (back && back.isConnected && onScreen(back)) {
              back.focus();
              return;
            }
            Array.from(document.querySelectorAll<HTMLButtonElement>("button[data-sidebar-toggle]"))
              .find(onScreen)
              ?.focus();
          });
          // The wizard may just have launched a patrol: every part of the
          // page that lists patrols reads them again.
          void refreshPatrols();
        }}
      />
    </Suspense>,
    document.body,
  );
}
