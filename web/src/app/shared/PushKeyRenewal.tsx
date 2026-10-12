"use client";

import { useEffect } from "react";
import { ensureCurrentPushKey } from "./pushKey";

// PushKeyRenewal renews this browser's push subscription after a VAPID key
// rotation (see pushKey.ts). It sits in the root layout so the renewal runs on
// whatever page the person opens next, not only on Settings, which nobody has
// a reason to visit after a rotation. Renders nothing; does nothing without a
// subscription, notification permission, or a session.
export function PushKeyRenewal() {
  useEffect(() => {
    void ensureCurrentPushKey();
  }, []);
  return null;
}
