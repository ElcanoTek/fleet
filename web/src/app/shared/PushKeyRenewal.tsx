"use client";

import { useEffect } from "react";
import { ensureCurrentPushKey } from "./pushKey";

// PushKeyRenewal renews this browser's push subscription after a VAPID key
// rotation (see pushKey.ts). It sits in the root layout so the renewal runs on
// whatever page the person opens next, not only on Settings, which nobody has
// a reason to visit after a rotation. Renders nothing; does nothing without a
// subscription, notification permission, or a session, and is skipped on the
// signed-out pages.
export function PushKeyRenewal() {
  useEffect(() => {
    // Signed-out pages have no session to renew with: the key read would only
    // answer 401.
    const path = window.location.pathname;
    if (path === "/login" || path.startsWith("/shared/")) return;
    void ensureCurrentPushKey();
  }, []);
  return null;
}
