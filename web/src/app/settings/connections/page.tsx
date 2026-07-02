"use client";

import Image from "next/image";
import Link from "next/link";
import { useEffect, useState } from "react";

import NotificationsCard from "./NotificationsCard";
import {
  authHint,
  categoriesOf,
  consentRequired,
  filterCatalog,
  groupByCategory,
  needsTenantURL,
  provenanceBadge,
  type CatalogResponse,
  type CatalogThirdParty,
} from "./catalog";
import { NoticeBanner } from "@/app/shared/ui/NoticeBanner";
import { StatusChip, type StatusTone } from "@/app/shared/ui/StatusChip";

// Per-user remote (hosted) MCP connections (#443). Users add a hosted MCP server
// by URL, then log in to it via the OAuth handshake (the backend handles
// discovery + dynamic client registration + PKCE). Connected servers' tools
// become available in chat turns and the user's scheduled tasks. Local stdio MCP
// servers are operator-configured and not managed here.

type RemoteServer = {
  id: string;
  name: string;
  url: string;
  transport: string;
  status: string;
  status_detail?: string;
  created_at: number;
  updated_at: number;
};

// A server another user shared with you: usable in your chats and scheduled
// tasks (tool calls authenticate with the OWNER's login, host-side).
type SharedServer = RemoteServer & { owner: string };

type ListResponse = {
  servers: RemoteServer[];
  // Grants on YOUR servers, keyed by server id ("*" = everyone on this box).
  shares?: Record<string, string[]>;
  shared_with_me?: SharedServer[];
};

// The trust-labeled MCP directory (#538, expanded into a categorized,
// searchable connector directory). Bundled entries are the operator's
// sandboxed connectors (informational here — they're toggled per conversation
// in the Tools picker); third-party entries are hosted servers the user can
// add to the connect flow below. Types + the grouping/search/provenance
// helpers live in ./catalog (unit-tested there).

const STATUS_LABEL: Record<string, string> = {
  login_required: "Login required",
  connected: "Connected",
  needs_reauth: "Reconnect needed",
  error: "Error",
};

function statusTone(status: string): StatusTone {
  switch (status) {
    case "connected":
      return "success";
    case "needs_reauth":
    case "error":
      return "warning";
    default:
      return "neutral";
  }
}

async function fetchServers(): Promise<ListResponse | null> {
  const res = await fetch("/api/remote-mcp-servers", { cache: "no-store" });
  if (res.status === 401) {
    window.location.href = "/login";
    return null;
  }
  if (res.status === 503) {
    throw new Error(
      "Remote MCP OAuth is not configured on this server (set FLEET_MCP_OAUTH_ENCRYPTION_KEY and FLEET_PUBLIC_BASE_URL).",
    );
  }
  if (!res.ok) {
    throw new Error(`Failed to load connections: ${res.status}`);
  }
  return (await res.json()) as ListResponse;
}

// granteeLabel renders the everyone wildcard readably.
function granteeLabel(g: string): string {
  return g === "*" ? "Everyone on this box" : g;
}

function errMessage(err: unknown): string {
  return err instanceof Error ? err.message : "Something went wrong.";
}

// readCallbackBanner derives the one-shot notice/error from the OAuth callback's
// ?connected / ?error query params. Computed lazily during render (not in an
// effect) so it doesn't trip react-hooks/set-state-in-effect; guarded for SSR.
function readCallbackBanner(): { notice: string | null; error: string | null } {
  if (typeof window === "undefined") return { notice: null, error: null };
  const params = new URLSearchParams(window.location.search);
  if (params.get("connected")) {
    const n = params.get("connected");
    return {
      notice: n && n !== "1" ? `Connected ${n}.` : "Connected.",
      error: null,
    };
  }
  if (params.get("error")) {
    return {
      notice: null,
      error: `Authorization failed: ${params.get("error")}`,
    };
  }
  return { notice: null, error: null };
}

async function fetchCatalog(): Promise<CatalogResponse | null> {
  const res = await fetch("/api/mcp-catalog", { cache: "no-store" });
  if (!res.ok) return null; // the directory is optional decoration — never block the page
  return (await res.json()) as CatalogResponse;
}

export default function ConnectionsPage() {
  const [initialBanner] = useState(readCallbackBanner);
  const [servers, setServers] = useState<RemoteServer[] | null>(null);
  const [shares, setShares] = useState<Record<string, string[]>>({});
  const [sharedWithMe, setSharedWithMe] = useState<SharedServer[]>([]);
  // Which of your servers has its share panel open, and the pending grantee.
  const [shareOpenFor, setShareOpenFor] = useState<string | null>(null);
  const [shareGrantee, setShareGrantee] = useState("");
  const [catalog, setCatalog] = useState<CatalogResponse | null>(null);
  // The directory is the page's main discovery surface — open by default,
  // collapsible for users who only manage existing connections.
  const [catalogOpen, setCatalogOpen] = useState(true);
  const [catalogQuery, setCatalogQuery] = useState("");
  const [catalogCategory, setCatalogCategory] = useState("");
  // A non-official (aggregator/community) entry awaiting the user's explicit,
  // operator-named consent before it is added.
  const [consentFor, setConsentFor] = useState<CatalogThirdParty | null>(null);
  const [error, setError] = useState<string | null>(initialBanner.error);
  const [notice, setNotice] = useState<string | null>(initialBanner.notice);
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState(false);
  const [name, setName] = useState("");
  const [url, setUrl] = useState("");

  const apply = (isStale: () => boolean) => {
    fetchServers()
      .then((data) => {
        if (isStale() || data === null) return;
        setServers(data.servers ?? []);
        setShares(data.shares ?? {});
        setSharedWithMe(data.shared_with_me ?? []);
      })
      .catch((e: unknown) => {
        if (isStale()) return;
        setError(errMessage(e));
      })
      .finally(() => {
        if (isStale()) return;
        setLoading(false);
      });
  };

  const refresh = () => {
    setError(null);
    setLoading(true);
    apply(() => false);
  };

  useEffect(() => {
    let stale = false;
    apply(() => stale);
    // The directory loads independently; a failure just hides the section.
    fetchCatalog()
      .then((c) => {
        if (!stale) setCatalog(c);
      })
      .catch(() => {});
    // Strip the one-shot ?connected / ?error params from the URL (the banner was
    // already derived from them during render). replaceState is not setState, so
    // this stays clear of react-hooks/set-state-in-effect.
    const params = new URLSearchParams(window.location.search);
    if (params.get("connected") || params.get("error")) {
      window.history.replaceState({}, "", "/settings/connections");
    }
    return () => {
      stale = true;
    };
  }, []);

  const addServer = (e: React.FormEvent) => {
    e.preventDefault();
    setError(null);
    setNotice(null);
    setBusy(true);
    fetch("/api/remote-mcp-servers", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ name: name.trim(), url: url.trim() }),
    })
      .then(async (res) => {
        if (!res.ok) {
          throw new Error((await res.text()) || `Add failed: ${res.status}`);
        }
        setName("");
        setUrl("");
        setNotice("Server added. Click Connect to log in.");
        refresh();
      })
      .catch((err: unknown) => setError(errMessage(err)))
      .finally(() => setBusy(false));
  };

  const connect = (id: string) => {
    setError(null);
    setBusy(true);
    fetch(`/api/remote-mcp-servers/${encodeURIComponent(id)}/authorize`, {
      method: "POST",
    })
      .then(async (res) => {
        if (!res.ok) {
          throw new Error(
            (await res.text()) || `Authorize failed: ${res.status}`,
          );
        }
        const data = (await res.json()) as { redirect_url?: string };
        if (!data.redirect_url)
          throw new Error("No authorization URL returned.");
        // Full-page navigation to the authorization server. It redirects back to
        // /api/oauth/mcp/callback, which returns here with ?connected / ?error.
        window.location.href = data.redirect_url;
      })
      .catch((err: unknown) => {
        setError(errMessage(err));
        setBusy(false);
      });
  };

  // One-click add from the directory: same POST as the manual form, prefilled
  // from the curated entry. The server lands in "login_required"; the user
  // then clicks Connect like any manual add. An entry whose endpoint is NOT
  // operated by the service's own vendor first goes through an explicit
  // consent step naming the operator (the operator receives tool-call
  // arguments — which can include conversation content — and, for OAuth
  // flows, often holds the delegated access token).
  const requestAddFromCatalog = (entry: CatalogThirdParty) => {
    if (consentRequired(entry)) {
      setConsentFor(entry);
      return;
    }
    addFromCatalog(entry);
  };

  const addFromCatalog = (entry: CatalogThirdParty) => {
    setConsentFor(null);
    setError(null);
    setNotice(null);
    setBusy(true);
    fetch("/api/remote-mcp-servers", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ name: entry.name, url: entry.url }),
    })
      .then(async (res) => {
        if (!res.ok) {
          throw new Error((await res.text()) || `Add failed: ${res.status}`);
        }
        setNotice(`${entry.display_name} added. Click Connect to sign in.`);
        refresh();
      })
      .catch((err: unknown) => setError(errMessage(err)))
      .finally(() => setBusy(false));
  };

  // Share a connection with another user on the box (grantee email) or with
  // everyone ("*"). Tool calls made through a shared connection authenticate
  // with the owner's login host-side; the grantee never sees the credential.
  const share = (id: string, grantee: string) => {
    const g = grantee.trim();
    if (!g) return;
    setError(null);
    setBusy(true);
    fetch(`/api/remote-mcp-servers/${encodeURIComponent(id)}/shares`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ grantee: g }),
    })
      .then(async (res) => {
        if (!res.ok && res.status !== 204) {
          throw new Error((await res.text()) || `Share failed: ${res.status}`);
        }
        setShareGrantee("");
        refresh();
      })
      .catch((err: unknown) => setError(errMessage(err)))
      .finally(() => setBusy(false));
  };

  const unshare = (id: string, grantee: string) => {
    setError(null);
    setBusy(true);
    fetch(
      `/api/remote-mcp-servers/${encodeURIComponent(id)}/shares/${encodeURIComponent(grantee)}`,
      { method: "DELETE" },
    )
      .then(async (res) => {
        if (!res.ok && res.status !== 204) {
          throw new Error((await res.text()) || `Unshare failed: ${res.status}`);
        }
        refresh();
      })
      .catch((err: unknown) => setError(errMessage(err)))
      .finally(() => setBusy(false));
  };

  const disconnect = (id: string, label: string) => {
    if (
      !window.confirm(
        `Disconnect "${label}"? Its stored tokens are revoked and removed.`,
      )
    ) {
      return;
    }
    setError(null);
    setBusy(true);
    fetch(`/api/remote-mcp-servers/${encodeURIComponent(id)}`, {
      method: "DELETE",
    })
      .then(async (res) => {
        if (!res.ok && res.status !== 204) {
          throw new Error(
            (await res.text()) || `Disconnect failed: ${res.status}`,
          );
        }
        setNotice("Disconnected.");
        refresh();
      })
      .catch((err: unknown) => setError(errMessage(err)))
      .finally(() => setBusy(false));
  };

  // h-dvh + overflow-y-auto: the page owns its vertical scroll. The app
  // shell's html/body rules (h-full, overscroll-behavior: none) break
  // document-level scrolling on iOS, so relying on body scroll left this
  // page unscrollable on mobile.
  return (
    <main className="h-dvh overflow-y-auto bg-[var(--gradient-bg-home-signature)] px-6 py-10 text-[var(--color-text-primary)]">
      <div className="mx-auto w-full max-w-3xl">
        <header className="mb-6 flex items-center justify-between gap-4">
          <Link href="/" className="flex items-center gap-2.5 no-underline">
            <Image
              src="/logos/elcano-mark-primary.svg"
              alt="Elcano"
              width={28}
              height={28}
              priority
            />
            <span className="font-heading text-[0.9375rem] font-semibold">
              Connections
            </span>
          </Link>
          <Link
            href="/"
            className="rounded-full border border-[var(--color-border-strong)] px-3 py-1 text-[0.8125rem] text-[var(--color-text-secondary)] transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)]"
          >
            Back to chat
          </Link>
        </header>

        <p className="mb-5 text-[0.875rem] text-[var(--color-text-secondary)]">
          Connect remote (hosted) MCP servers and sign in to each with your own
          account. Connected servers&apos; tools become available to you in chat
          and your scheduled tasks. Credentials are stored encrypted on the
          server and never shared with other users.
        </p>

        {notice ? (
          <NoticeBanner tone="success" className="mb-4">
            {notice}
          </NoticeBanner>
        ) : null}
        {error ? (
          <NoticeBanner tone="danger" className="mb-4">
            {error}
          </NoticeBanner>
        ) : null}

        <form
          onSubmit={addServer}
          className="mb-6 rounded-[1rem] border border-[var(--color-border)] bg-[var(--gradient-surface-panel)] p-4"
        >
          <h2 className="mb-3 text-[0.9rem] font-semibold">
            Add a remote MCP server
          </h2>
          <div className="grid gap-3 sm:grid-cols-[1fr_2fr_auto] sm:items-end">
            <label className="grid gap-1 text-[0.75rem] text-[var(--color-text-muted)]">
              Name
              <input
                value={name}
                onChange={(e) => setName(e.target.value)}
                placeholder="my-server"
                required
                className="rounded-[0.6rem] border border-[var(--color-border-strong)] bg-[var(--color-overlay-soft)] px-3 py-2 text-[0.875rem] text-[var(--color-text-primary)] outline-none focus:border-[var(--color-accent)]"
              />
            </label>
            <label className="grid gap-1 text-[0.75rem] text-[var(--color-text-muted)]">
              Server URL
              <input
                value={url}
                onChange={(e) => setUrl(e.target.value)}
                placeholder="https://mcp.example.com/mcp"
                type="url"
                required
                className="rounded-[0.6rem] border border-[var(--color-border-strong)] bg-[var(--color-overlay-soft)] px-3 py-2 text-[0.875rem] text-[var(--color-text-primary)] outline-none focus:border-[var(--color-accent)]"
              />
            </label>
            <button
              type="submit"
              disabled={busy || !name.trim() || !url.trim()}
              className="rounded-full border border-[var(--color-border-strong)] px-4 py-2 text-[0.8125rem] font-medium transition hover:bg-[var(--color-overlay-soft)] disabled:opacity-50"
            >
              {busy ? "Working…" : "Add"}
            </button>
          </div>
        </form>

        <div className="overflow-hidden rounded-[1rem] border border-[var(--color-border)] bg-[var(--gradient-surface-panel)]">
          <div className="flex items-center justify-between border-b border-[var(--color-border)] px-4 py-2">
            <span className="text-[0.75rem] uppercase tracking-wide text-[var(--color-text-muted)]">
              Your servers
            </span>
            <button
              type="button"
              onClick={refresh}
              disabled={loading}
              className="text-[0.75rem] text-[var(--color-text-secondary)] underline-offset-2 hover:underline disabled:opacity-50"
            >
              {loading ? "Loading…" : "Refresh"}
            </button>
          </div>
          {loading ? (
            <p className="px-4 py-5 text-center text-[0.875rem] text-[var(--color-text-muted)]">
              Loading…
            </p>
          ) : servers && servers.length > 0 ? (
            <ul>
              {servers.map((s) => (
                <li
                  key={s.id}
                  className="flex flex-wrap items-center justify-between gap-3 border-b border-[var(--color-border-subtle)] px-4 py-3 last:border-none"
                >
                  <div className="min-w-0">
                    <div className="flex items-center gap-2">
                      <span className="font-medium text-[var(--color-text-primary)]">
                        {s.name}
                      </span>
                      <StatusChip tone={statusTone(s.status)}>
                        {STATUS_LABEL[s.status] ?? s.status}
                      </StatusChip>
                    </div>
                    <p className="truncate text-[0.75rem] text-[var(--color-text-muted)]">
                      {s.url}
                    </p>
                    {s.status_detail ? (
                      <p className="text-[0.6875rem] text-[var(--color-warning-soft)]">
                        {s.status_detail}
                      </p>
                    ) : null}
                  </div>
                  <div className="flex items-center gap-2">
                    <button
                      type="button"
                      onClick={() => connect(s.id)}
                      disabled={busy}
                      className="rounded-full border border-[var(--color-border-strong)] px-3 py-1 text-[0.75rem] transition hover:bg-[var(--color-overlay-soft)] disabled:opacity-50"
                    >
                      {s.status === "connected" ? "Reconnect" : "Connect"}
                    </button>
                    <button
                      type="button"
                      onClick={() => {
                        setShareGrantee("");
                        setShareOpenFor((cur) => (cur === s.id ? null : s.id));
                      }}
                      disabled={busy}
                      className="rounded-full border border-[var(--color-border-strong)] px-3 py-1 text-[0.75rem] transition hover:bg-[var(--color-overlay-soft)] disabled:opacity-50"
                    >
                      Share{(shares[s.id]?.length ?? 0) > 0 ? ` (${shares[s.id].length})` : ""}
                    </button>
                    <button
                      type="button"
                      onClick={() => disconnect(s.id, s.name)}
                      disabled={busy}
                      className="rounded-full border border-[var(--color-border-subtle)] px-3 py-1 text-[0.75rem] text-[var(--color-text-secondary)] transition hover:bg-[var(--color-overlay-soft)] disabled:opacity-50"
                    >
                      Remove
                    </button>
                  </div>
                  {shareOpenFor === s.id ? (
                    <div className="mt-1 w-full rounded-[0.75rem] border border-[var(--color-border-subtle)] bg-[var(--color-overlay-soft)] px-3 py-2.5">
                      <p className="mb-2 text-[0.75rem] text-[var(--color-text-muted)]">
                        People you share with can use this connection in their
                        chats and scheduled tasks. Their tool calls run under{" "}
                        <strong className="text-[var(--color-text-secondary)]">
                          your
                        </strong>{" "}
                        login, brokered server-side — they never see the
                        credential, and removing a share revokes access
                        immediately.
                      </p>
                      {(shares[s.id] ?? []).length > 0 ? (
                        <ul className="mb-2 flex flex-wrap gap-1.5">
                          {(shares[s.id] ?? []).map((g) => (
                            <li
                              key={g}
                              className="flex items-center gap-1.5 rounded-full border border-[var(--color-border-strong)] px-2.5 py-0.5 text-[0.75rem]"
                            >
                              {granteeLabel(g)}
                              <button
                                type="button"
                                onClick={() => unshare(s.id, g)}
                                disabled={busy}
                                aria-label={`Stop sharing with ${granteeLabel(g)}`}
                                className="text-[var(--color-text-muted)] transition hover:text-[var(--color-text-primary)] disabled:opacity-50"
                              >
                                ×
                              </button>
                            </li>
                          ))}
                        </ul>
                      ) : null}
                      <div className="flex flex-wrap items-center gap-2">
                        <input
                          value={shareGrantee}
                          onChange={(e) => setShareGrantee(e.target.value)}
                          onKeyDown={(e) => {
                            if (e.key === "Enter") {
                              e.preventDefault();
                              share(s.id, shareGrantee);
                            }
                          }}
                          placeholder="teammate@example.com"
                          type="email"
                          className="min-w-0 flex-1 rounded-[0.6rem] border border-[var(--color-border-strong)] bg-[var(--color-surface-1)] px-3 py-1.5 text-[0.8125rem] text-[var(--color-text-primary)] outline-none focus:border-[var(--color-accent)]"
                        />
                        <button
                          type="button"
                          onClick={() => share(s.id, shareGrantee)}
                          disabled={busy || !shareGrantee.trim()}
                          className="rounded-full border border-[var(--color-border-strong)] px-3 py-1 text-[0.75rem] transition hover:bg-[var(--color-overlay-soft)] disabled:opacity-50"
                        >
                          Share
                        </button>
                        {(shares[s.id] ?? []).includes("*") ? null : (
                          <button
                            type="button"
                            onClick={() => share(s.id, "*")}
                            disabled={busy}
                            className="rounded-full border border-[var(--color-border-subtle)] px-3 py-1 text-[0.75rem] text-[var(--color-text-secondary)] transition hover:bg-[var(--color-overlay-soft)] disabled:opacity-50"
                          >
                            Share with everyone
                          </button>
                        )}
                      </div>
                    </div>
                  ) : null}
                </li>
              ))}
            </ul>
          ) : (
            <p className="px-4 py-5 text-center text-[0.875rem] text-[var(--color-text-muted)]">
              No remote servers yet. Add one above to get started.
            </p>
          )}
        </div>

        {sharedWithMe.length > 0 ? (
          <div className="mt-6 overflow-hidden rounded-[1rem] border border-[var(--color-border)] bg-[var(--gradient-surface-panel)]">
            <div className="border-b border-[var(--color-border)] px-4 py-2">
              <span className="text-[0.75rem] uppercase tracking-wide text-[var(--color-text-muted)]">
                Shared with you
              </span>
            </div>
            <p className="px-4 pt-3 text-[0.75rem] text-[var(--color-text-muted)]">
              Connections other users shared with you. Their tools are
              available in your chats and scheduled tasks; calls run under the
              owner&apos;s login, brokered server-side.
            </p>
            <ul>
              {sharedWithMe.map((s) => (
                <li
                  key={s.id}
                  className="flex flex-wrap items-center justify-between gap-3 border-b border-[var(--color-border-subtle)] px-4 py-3 last:border-none"
                >
                  <div className="min-w-0">
                    <div className="flex items-center gap-2">
                      <span className="font-medium text-[var(--color-text-primary)]">
                        {s.name}
                      </span>
                      <StatusChip tone={statusTone(s.status)}>
                        {STATUS_LABEL[s.status] ?? s.status}
                      </StatusChip>
                    </div>
                    <p className="truncate text-[0.75rem] text-[var(--color-text-muted)]">
                      {s.url}
                    </p>
                  </div>
                  <span className="text-[0.75rem] text-[var(--color-text-secondary)]">
                    shared by {s.owner}
                  </span>
                </li>
              ))}
            </ul>
          </div>
        ) : null}

        {catalog &&
        (catalog.third_party.length > 0 || catalog.bundled.length > 0) ? (
          <div className="mt-6 overflow-hidden rounded-[1rem] border border-[var(--color-border)] bg-[var(--gradient-surface-panel)]">
            <button
              type="button"
              onClick={() => setCatalogOpen((v) => !v)}
              className="flex w-full items-center justify-between px-4 py-3 text-left"
            >
              <span className="text-[0.75rem] uppercase tracking-wide text-[var(--color-text-muted)]">
                Connector directory
              </span>
              <span className="text-[0.75rem] text-[var(--color-text-secondary)]">
                {catalogOpen
                  ? "Hide"
                  : `Browse ${catalog.third_party.length + catalog.bundled.length}`}
              </span>
            </button>
            {catalogOpen ? (
              <div className="border-t border-[var(--color-border)]">
                {catalog.bundled.length > 0 ? (
                  <section className="border-b border-[var(--color-border-subtle)] px-4 py-3">
                    <h3 className="mb-1 text-[0.8125rem] font-semibold">
                      Bundled by your workspace
                    </h3>
                    <p className="mb-2 text-[0.75rem] text-[var(--color-text-muted)]">
                      Reviewed and shipped by your operator. These run inside
                      the sandbox on this deployment with credentials held
                      server-side — nothing leaves the box except the
                      connector&apos;s own API calls. Toggle them per
                      conversation in the Tools picker.
                    </p>
                    <ul className="grid gap-2 sm:grid-cols-2">
                      {catalog.bundled.map((b) => (
                        <li
                          key={b.name}
                          className="rounded-[0.75rem] border border-[var(--color-border-subtle)] px-3 py-2"
                        >
                          <div className="flex items-center gap-2">
                            <span className="text-[0.8125rem] font-medium">
                              {b.display_name || b.name}
                            </span>
                            <StatusChip tone="success">Bundled</StatusChip>
                            {b.beta ? (
                              <span className="rounded-full border border-[var(--color-border-strong)] px-2 py-0.5 text-[0.6875rem] text-[var(--color-text-muted)]">
                                Beta
                              </span>
                            ) : null}
                          </div>
                          <p className="mt-1 line-clamp-2 text-[0.75rem] text-[var(--color-text-muted)]">
                            {b.description}
                          </p>
                        </li>
                      ))}
                    </ul>
                  </section>
                ) : null}
                {catalog.third_party.length > 0 ? (
                  <section className="px-4 py-3">
                    <h3 className="mb-1 text-[0.8125rem] font-semibold">
                      Hosted servers
                    </h3>
                    <p className="mb-2 text-[0.75rem] text-[var(--color-text-muted)]">
                      Run by the named operator, not by your workspace.
                      Connecting one signs you in with that operator and sends
                      tool calls — which can include parts of your conversation
                      — to their service under their terms.{" "}
                      <span className="text-[var(--color-text-secondary)]">
                        Official
                      </span>{" "}
                      = the service&apos;s own vendor runs the endpoint;{" "}
                      <span className="text-[var(--color-text-secondary)]">
                        Aggregator
                      </span>{" "}
                      and{" "}
                      <span className="text-[var(--color-text-secondary)]">
                        Community
                      </span>{" "}
                      endpoints are run by someone else — vet them via the
                      linked docs and source before connecting.
                    </p>
                    <input
                      value={catalogQuery}
                      onChange={(e) => setCatalogQuery(e.target.value)}
                      placeholder={`Search ${catalog.third_party.length} servers — try "postgres", "crm", "scraping"…`}
                      className="mb-2 w-full rounded-[0.6rem] border border-[var(--color-border-strong)] bg-[var(--color-overlay-soft)] px-3 py-1.5 text-[0.8125rem] text-[var(--color-text-primary)] outline-none focus:border-[var(--color-accent)]"
                    />
                    <div className="mb-3 flex flex-wrap gap-1.5">
                      <button
                        type="button"
                        onClick={() => setCatalogCategory("")}
                        className={`rounded-full border px-2.5 py-0.5 text-[0.6875rem] transition ${
                          catalogCategory === ""
                            ? "border-[var(--color-accent)] text-[var(--color-text-primary)]"
                            : "border-[var(--color-border-subtle)] text-[var(--color-text-secondary)] hover:bg-[var(--color-overlay-soft)]"
                        }`}
                      >
                        All ({catalog.third_party.length})
                      </button>
                      {categoriesOf(catalog.third_party).map((c) => (
                        <button
                          key={c.slug}
                          type="button"
                          onClick={() =>
                            setCatalogCategory((cur) =>
                              cur === c.slug ? "" : c.slug,
                            )
                          }
                          className={`rounded-full border px-2.5 py-0.5 text-[0.6875rem] transition ${
                            catalogCategory === c.slug
                              ? "border-[var(--color-accent)] text-[var(--color-text-primary)]"
                              : "border-[var(--color-border-subtle)] text-[var(--color-text-secondary)] hover:bg-[var(--color-overlay-soft)]"
                          }`}
                        >
                          {c.label} ({c.count})
                        </button>
                      ))}
                    </div>
                    {(() => {
                      const filtered = filterCatalog(
                        catalog.third_party,
                        catalogQuery,
                        catalogCategory,
                      );
                      if (filtered.length === 0) {
                        return (
                          <p className="py-4 text-center text-[0.8125rem] text-[var(--color-text-muted)]">
                            No servers match — try a different search, or add
                            any hosted MCP server by URL in the form above.
                          </p>
                        );
                      }
                      return groupByCategory(filtered).map((group) => (
                        <div key={group.slug} className="mb-3 last:mb-0">
                          <h4 className="mb-1.5 text-[0.6875rem] uppercase tracking-wide text-[var(--color-text-muted)]">
                            {group.label}
                          </h4>
                          <ul className="grid gap-2 sm:grid-cols-2">
                            {group.entries.map((tp) => {
                              const already = (servers ?? []).some(
                                (s) => s.url === tp.url,
                              );
                              const badge = provenanceBadge(tp.provenance);
                              const hint = authHint(tp);
                              return (
                                <li
                                  key={tp.name}
                                  className="flex flex-col rounded-[0.75rem] border border-[var(--color-border-subtle)] px-3 py-2"
                                >
                                  <div className="flex items-center gap-2">
                                    <span className="min-w-0 truncate text-[0.8125rem] font-medium">
                                      {tp.display_name}
                                    </span>
                                    <StatusChip tone={badge.tone}>
                                      {badge.label}
                                    </StatusChip>
                                  </div>
                                  <p className="mt-1 line-clamp-2 flex-1 text-[0.75rem] text-[var(--color-text-muted)]">
                                    {tp.description}
                                  </p>
                                  <div className="mt-2 flex items-center justify-between gap-2">
                                    <span className="truncate text-[0.6875rem] text-[var(--color-text-muted)]">
                                      {tp.vendor || tp.url}
                                      {tp.docs_url ? (
                                        <>
                                          {" · "}
                                          <a
                                            href={tp.docs_url}
                                            target="_blank"
                                            rel="noreferrer"
                                            className="underline-offset-2 hover:underline"
                                          >
                                            docs
                                          </a>
                                        </>
                                      ) : null}
                                      {tp.repo_url ? (
                                        <>
                                          {" · "}
                                          <a
                                            href={tp.repo_url}
                                            target="_blank"
                                            rel="noreferrer"
                                            className="underline-offset-2 hover:underline"
                                          >
                                            source
                                          </a>
                                        </>
                                      ) : null}
                                    </span>
                                    {catalog.remote_mcp_enabled ? (
                                      needsTenantURL(tp) ? (
                                        <span
                                          className="whitespace-nowrap text-[0.6875rem] text-[var(--color-text-muted)]"
                                          title="This endpoint is per-organization — copy your own URL from the vendor docs into the form above."
                                        >
                                          {hint}
                                        </span>
                                      ) : (
                                        <span className="flex items-center gap-2 whitespace-nowrap">
                                          {hint ? (
                                            <span className="text-[0.6875rem] text-[var(--color-text-muted)]">
                                              {hint}
                                            </span>
                                          ) : null}
                                          <button
                                            type="button"
                                            onClick={() =>
                                              requestAddFromCatalog(tp)
                                            }
                                            disabled={busy || already}
                                            className="rounded-full border border-[var(--color-border-strong)] px-3 py-1 text-[0.6875rem] transition hover:bg-[var(--color-overlay-soft)] disabled:opacity-50"
                                          >
                                            {already ? "Added" : "Add"}
                                          </button>
                                        </span>
                                      )
                                    ) : null}
                                  </div>
                                </li>
                              );
                            })}
                          </ul>
                        </div>
                      ));
                    })()}
                    {!catalog.remote_mcp_enabled ? (
                      <p className="mt-2 text-[0.6875rem] text-[var(--color-text-muted)]">
                        Connecting hosted servers requires the operator to
                        configure remote MCP OAuth
                        (FLEET_MCP_OAUTH_ENCRYPTION_KEY and
                        FLEET_PUBLIC_BASE_URL).
                      </p>
                    ) : null}
                  </section>
                ) : null}
              </div>
            ) : null}
          </div>
        ) : null}

        {/* Browser Web Push opt-in (#292) — per-browser, low-detail alerts. */}
        <NotificationsCard />
      </div>

      {/* Consent step for endpoints not operated by the service's own vendor.
          A badge alone gets scrolled past; connecting sends conversation-
          derived tool traffic to (and often parks a delegated access token
          with) the named operator, so the add is gated on an explicit,
          operator-named confirmation. */}
      {consentFor ? (
        <div
          role="dialog"
          aria-modal="true"
          aria-label={`Connect ${consentFor.display_name}?`}
          className="fixed inset-0 z-50 flex items-center justify-center bg-[var(--color-overlay-strong)] px-4"
          onClick={() => setConsentFor(null)}
        >
          <div
            className="w-full max-w-md rounded-[1rem] border border-[var(--color-border)] bg-[var(--color-surface-1)] p-5 shadow-[var(--shadow-lg)]"
            onClick={(e) => e.stopPropagation()}
          >
            <div className="mb-2 flex items-center gap-2">
              <h3 className="text-[0.9375rem] font-semibold">
                Connect {consentFor.display_name}?
              </h3>
              <StatusChip tone={provenanceBadge(consentFor.provenance).tone}>
                {provenanceBadge(consentFor.provenance).label}
              </StatusChip>
            </div>
            <p className="mb-3 text-[0.8125rem] text-[var(--color-text-secondary)]">
              This endpoint is operated by{" "}
              <strong className="text-[var(--color-text-primary)]">
                {consentFor.vendor || "an unnamed operator"}
              </strong>
              {provenanceBadge(consentFor.provenance).label === "Aggregator"
                ? " — a platform that hosts access to other vendors' services, not the services themselves."
                : " — not the vendor of the underlying service, and not your workspace."}{" "}
              Once connected, it receives your tool calls (which can include
              parts of your conversations){consentFor.auth === "oauth"
                ? " and holds the access token you grant during sign-in"
                : ""}
              .
            </p>
            <p className="mb-4 text-[0.75rem] text-[var(--color-text-muted)]">
              Vet it first:{" "}
              {consentFor.docs_url ? (
                <a
                  href={consentFor.docs_url}
                  target="_blank"
                  rel="noreferrer"
                  className="underline underline-offset-2"
                >
                  documentation
                </a>
              ) : null}
              {consentFor.docs_url && consentFor.repo_url ? " · " : null}
              {consentFor.repo_url ? (
                <a
                  href={consentFor.repo_url}
                  target="_blank"
                  rel="noreferrer"
                  className="underline underline-offset-2"
                >
                  source code
                </a>
              ) : null}
              {!consentFor.docs_url && !consentFor.repo_url
                ? "no docs or source were provided for this entry."
                : null}
            </p>
            <div className="flex justify-end gap-2">
              <button
                type="button"
                onClick={() => setConsentFor(null)}
                className="rounded-full border border-[var(--color-border-subtle)] px-4 py-1.5 text-[0.8125rem] text-[var(--color-text-secondary)] transition hover:bg-[var(--color-overlay-soft)]"
              >
                Cancel
              </button>
              <button
                type="button"
                onClick={() => addFromCatalog(consentFor)}
                disabled={busy}
                className="rounded-full border border-[var(--color-border-strong)] px-4 py-1.5 text-[0.8125rem] font-medium transition hover:bg-[var(--color-overlay-soft)] disabled:opacity-50"
              >
                I trust this operator — add
              </button>
            </div>
          </div>
        </div>
      ) : null}
    </main>
  );
}
