"use client";

import { ToastProvider } from "@/app/shared/ui/Toast";
import { NoticeBanner } from "@/app/shared/ui/NoticeBanner";
import { useMcpServers } from "@/app/shared/hooks/useMcpServers";
import { CredentialAccountAdmin } from "./CredentialAccountAdmin";
import { SettingsShell } from "./SettingsShell";

// Settings → General. These controls used to live in an Operations
// Center-only modal while Connections and Skills were standalone pages — three
// different chromes for what users read as one "settings" area. Now every
// section is a page inside the shared SettingsShell, reachable from the
// account menu on both surfaces.
//
// The concurrency-cap card that used to sit here was removed: it called
// GET/PUT /concurrency, which fleet's orchestrator never served (a moc-era
// leftover), so it could only ever error. The cap is FLEET_MAX_CONCURRENT_AGENTS
// in the env file (boot-bound: it sizes the admission semaphore + sandbox warm
// pool). Live-tunable workspace settings live in Settings → Admin → Feature
// settings.
//
// The credential card talks to the orchestrator backend through
// /api/orchestrator/*, which resolves the same session cookie the rest of the
// app uses; authorization is enforced upstream at :8000 exactly as it was for
// the modal.

export default function GeneralSettingsPage() {
  return (
    <ToastProvider>
      <GeneralSettings />
    </ToastProvider>
  );
}

function GeneralSettings() {
  const { servers, error, reload } = useMcpServers(true);
  return (
    <SettingsShell
      title="General"
      description="The credential accounts your MCP connectors sign in with. Workspace-wide feature settings live under Admin."
    >
      {error ? (
        <NoticeBanner tone="danger" className="mb-4">
          Couldn&apos;t load Operations Center settings ({error}). Your account
          may not be provisioned for the Operations Center — ask an
          administrator if you expected access.
        </NoticeBanner>
      ) : null}

      <section className="rounded-[1rem] border border-[var(--color-border)] bg-[var(--gradient-surface-panel)] p-4">
        <CredentialAccountAdmin servers={servers} onChanged={() => void reload()} />
      </section>
    </SettingsShell>
  );
}
