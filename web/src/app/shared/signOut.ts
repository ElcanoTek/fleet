import { dropPushSubscription } from "./pushSubscription";

// signOut is the one way every Fleet surface signs a user out: a top-level
// form POST to /api/auth/logout. A navigation (not a fetch) is what lets the
// route clear cookies and, for a session that came from central Auth, send
// the browser on to the provider's RP-initiated logout so the person is
// signed out of every application, not just Fleet. Chat, settings, help and
// the orchestrator all call this; none of them may "sign out" by clearing
// local state alone. Before the form posts, this browser's push subscription
// is let go (bounded, like any signOutAfter cleanup), so the next person to
// use the browser does not receive the signed-out account's notifications.
export function signOut(): Promise<void> {
  return signOutAfter(Promise.resolve());
}

function submitLogoutForm(): void {
  const form = document.createElement("form");
  form.method = "post";
  form.action = "/api/auth/logout";
  document.body.appendChild(form);
  form.submit();
}

// signOutAfter runs a best-effort cleanup first (the orchestrator's own
// logout, say), alongside dropping this browser's push subscription, and then
// signs out regardless of whether they succeeded, failed or hung: a surface's
// local tidy-up must never hold the real sign-out.
export function signOutAfter(
  attempt: Promise<unknown>,
  timeoutMs = 1500,
): Promise<void> {
  let timer: ReturnType<typeof setTimeout> | undefined;
  const bounded = new Promise<void>((resolve) => {
    timer = setTimeout(resolve, timeoutMs);
  });
  return Promise.race([
    Promise.allSettled([attempt, dropPushSubscription()]).then(() => undefined),
    bounded,
  ]).then(() => {
    if (timer !== undefined) clearTimeout(timer);
    submitLogoutForm();
  });
}
