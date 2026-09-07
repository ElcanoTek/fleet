"use client";

import { useState } from "react";

import { retryAdminProbe, type AdminState } from "./useIsAdmin";
import { btnClass } from "./ui/atoms";
import { NoticeBanner } from "@/app/shared/ui/NoticeBanner";

// AdminGateFallback — what an /settings/admin/* page renders while it is NOT
// showing its content. `unknown` (probe in flight) and `member` (about to be
// redirected to /settings) stay blank, as before. `unavailable` — the probe
// settled without an answer — used to be blank too, permanently: the page
// rendered null and nothing ever re-asked. It now says what happened and
// offers the one action that helps.
//
// Usage, at the gate every admin page already has:
//
//   if (admin !== "admin") return <AdminGateFallback state={admin} />;
export function AdminGateFallback({ state }: { state: AdminState }) {
  const [retrying, setRetrying] = useState(false);
  if (state !== "unavailable") return null;
  return (
    <NoticeBanner
      tone="warning"
      role="alert"
      data-testid="admin-gate-unavailable"
      className="flex flex-wrap items-center justify-between gap-3"
    >
      <span>Couldn&rsquo;t check your permissions — the server didn&rsquo;t answer.</span>
      <button
        type="button"
        className={btnClass({ sm: true })}
        disabled={retrying}
        onClick={() => {
          setRetrying(true);
          void retryAdminProbe().finally(() => setRetrying(false));
        }}
      >
        {retrying ? "Checking…" : "Retry"}
      </button>
    </NoticeBanner>
  );
}
