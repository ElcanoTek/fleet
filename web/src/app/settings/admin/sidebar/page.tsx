import { t as tr } from "@/app/lib/i18n";
// Settings → Admin → Sidebar (docs/SIDEBAR.md): the workspace's default
// sidebar. Everyone who has not customized their own rail sees this one, and
// "Reset to default" in a person's Customize panel returns them to it. It is
// stored on the server (GET/PUT/DELETE /api/admin/sidebar), not in the
// client-config bundle, so an admin changes it here without a deploy.
//
// The editor is the same one people use for their own rail; here it edits a
// draft, and nothing reaches anyone until Save.

import { useEffect, useState } from "react";
import { useRouter } from "@/app/lib/navigation";
import { NoticeBanner } from "@/app/shared/ui/NoticeBanner";
import { Button } from "@/components/ui/button";
import { SidebarEditor } from "@/app/shared/sidebar/SidebarEditor";
import {
  BUILTIN_LAYOUT,
  cloneLayout,
  isBuiltinLayout,
  normalizeLayout,
  sameLayout,
  type SidebarLayout,
} from "@/app/shared/sidebar/layout";
import { loadOwnSidebar } from "@/app/shared/sidebar/store";
import { responseErrorText } from "@/app/lib/responseError";
import { getGatedJson } from "@/app/settings/lib/getGated";
import { formatDate } from "@/app/shared/lib/format";
import { SetSection } from "../../ui/panels";
import { useIsAdmin } from "../../useIsAdmin";
import { AdminGateFallback } from "../../AdminGateFallback";

type WorkspaceSidebar = { layout: unknown; updated_by?: string; updated_at?: number };

export default function SidebarAdminPage() {
  const admin = useIsAdmin();
  const router = useRouter();
  useEffect(() => {
    if (admin === "member") router.replace("/settings");
  }, [admin, router]);
  if (admin !== "admin") return <AdminGateFallback state={admin} />;
  return <SidebarAdmin />;
}

function SidebarAdmin() {
  const [saved, setSaved] = useState<SidebarLayout | null | undefined>(undefined);
  const [meta, setMeta] = useState<{ by: string; at: number }>({ by: "", at: 0 });
  const [draft, setDraft] = useState<SidebarLayout>(BUILTIN_LAYOUT);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [notice, setNotice] = useState<string | null>(null);
  // The read failed: what is saved is unknown, so nothing may be saved over
  // it (a transient failure must not let an admin overwrite a layout they
  // never saw) until a reload succeeds. `saved` stays undefined meanwhile.
  const [loadFailed, setLoadFailed] = useState(false);
  const [loadNonce, setLoadNonce] = useState(0);

  const apply = (data: WorkspaceSidebar) => {
    const layout = data.layout ? normalizeLayout(data.layout) : null;
    setSaved(layout);
    setDraft(layout ?? cloneLayout(BUILTIN_LAYOUT));
    setMeta({ by: data.updated_by ?? "", at: data.updated_at ?? 0 });
  };

  useEffect(() => {
    let live = true;
    getGatedJson<WorkspaceSidebar>("/api/admin/sidebar", { label: tr("Workspace sidebar") })
      .then((data) => {
        if (live && data) {
          apply(data);
          setLoadFailed(false);
          setError(null);
        }
      })
      .catch((e: unknown) => {
        if (live) {
          setLoadFailed(true);
          setError(e instanceof Error ? e.message : tr("Couldn't load the workspace sidebar."));
        }
      });
    return () => {
      live = false;
    };
  }, [loadNonce]);

  const write = async (method: "PUT" | "DELETE") => {
    setBusy(true);
    setError(null);
    setNotice(null);
    try {
      const res = await fetch("/api/admin/sidebar", {
        method,
        headers: method === "PUT" ? { "Content-Type": "application/json" } : undefined,
        body: method === "PUT" ? JSON.stringify(draft) : undefined,
      });
      if (!res.ok) throw new Error(await responseErrorText(res, tr("Couldn't save the workspace sidebar.")));
      apply((await res.json()) as WorkspaceSidebar);
      setNotice(
        method === "PUT"
          ? tr("Saved. Everyone who hasn't customized their sidebar sees it on their next page load.")
          : tr("Reset. Everyone who hasn't customized their sidebar sees fleet's default again."),
      );
    } catch (e) {
      setError(e instanceof Error ? e.message : tr("Couldn't save the workspace sidebar."));
    } finally {
      setBusy(false);
    }
  };

  // A workspace sidebar cannot pin a background task (only its owner and
  // admins can open one) or the admin-only Analytics page; the server
  // refuses both, so a copy leaves them out.
  // Only a layout that really loaded is copied: when the admin's own read
  // failed, the store holds the built-in stand-in, and copying that would
  // replace the draft with something the admin never chose.
  const copyMine = async () => {
    setError(null);
    const own = await loadOwnSidebar();
    if (own.status !== "ready") {
      setError(tr("Couldn't load your sidebar, so nothing was copied. Try again."));
      return;
    }
    const mine = cloneLayout(own.layout);
    setDraft(
      normalizeLayout({
        ...mine,
        items: mine.items.filter((i) => i.kind !== "task" && !(i.kind === "page" && i.ref === "analytics")),
      }),
    );
  };

  const loaded = saved !== undefined && !loadFailed;
  const dirty = loaded ? !sameLayout(draft, saved ?? BUILTIN_LAYOUT) : false;

  return (
    <SetSection
      title={tr("Sidebar")}
      intro={tr(
        "The sidebar everyone starts with. Pin the dashboards, patrols and projects your team lives in, hide what it doesn't use, and choose where fleet opens. People can still make their own; this is what they get until they do, and what “Reset to default” brings back.",
      )}
    >
      {error ? <NoticeBanner tone="danger">{error}</NoticeBanner> : null}
      {notice ? <NoticeBanner tone="success">{notice}</NoticeBanner> : null}
      <div className="mb-4 flex flex-wrap items-center gap-2" data-testid="admin-sidebar-actions">
        <Button size="sm" disabled={!dirty || busy} onClick={() => void write("PUT")} data-testid="admin-sidebar-save">
          {tr("Save for everyone")}
        </Button>
        <Button
          size="sm"
          variant="outline"
          disabled={!dirty || busy}
          onClick={() => setDraft(saved ?? cloneLayout(BUILTIN_LAYOUT))}
        >
          {tr("Discard changes")}
        </Button>
        {loadFailed ? (
          <Button size="sm" variant="outline" onClick={() => setLoadNonce((n) => n + 1)} data-testid="admin-sidebar-retry">
            {tr("Try again")}
          </Button>
        ) : null}
        <Button size="sm" variant="ghost" disabled={busy || !loaded} onClick={() => void copyMine()} data-testid="admin-sidebar-copy-mine">
          {tr("Start from my sidebar")}
        </Button>
        <Button
          size="sm"
          variant="ghost"
          disabled={busy || !loaded || saved === null}
          onClick={() => void write("DELETE")}
          data-testid="admin-sidebar-reset"
        >
          {tr("Reset to fleet's default")}
        </Button>
        <span className="ml-auto text-[0.75rem] text-[var(--color-text-muted)]" data-testid="admin-sidebar-provenance">
          {loadFailed
            ? tr("Couldn't load the workspace sidebar.")
            : saved === undefined
            ? tr("Loading…")
            : saved === null || isBuiltinLayout(saved)
              ? tr("Using fleet's default")
              : meta.by
                ? tr("Customized by {0} · {1}", [meta.by, formatDate(new Date(meta.at * 1000).toISOString())])
                : tr("Customized")}
        </span>
      </div>
      <div className="max-w-[34rem] rounded-[var(--radius-lg)] border border-[var(--color-border)] bg-[var(--color-surface-1)] p-4">
        {/* Disabled until the saved default has loaded: an admin edits what is
            actually there, never a stand-in. */}
        <fieldset disabled={!loaded || busy} className="m-0 min-w-0 border-0 p-0" data-testid="admin-sidebar-editor">
          <SidebarEditor layout={draft} onChange={loaded && !busy ? setDraft : () => {}} mode="workspace" isAdmin />
        </fieldset>
      </div>
    </SetSection>
  );
}
