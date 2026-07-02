"use client";

import Image from "next/image";
import Link from "next/link";
import { useEffect, useState } from "react";

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

type ListResponse = { servers: RemoteServer[] };

// The trust-labeled MCP directory (#538). Bundled entries are the operator's
// sandboxed connectors (informational here — they're toggled per conversation
// in the Tools picker); third-party entries are vendor-hosted servers the user
// can one-click add to the connect flow below.
type CatalogThirdParty = {
  name: string;
  display_name: string;
  description: string;
  url: string;
  vendor?: string;
  docs_url?: string;
};

type CatalogBundled = {
  name: string;
  display_name?: string;
  description: string;
  tool_count: number;
  beta?: boolean;
};

type CatalogResponse = {
  bundled: CatalogBundled[];
  third_party: CatalogThirdParty[];
  remote_mcp_enabled: boolean;
};

const STATUS_LABEL: Record<string, string> = {
  login_required: "Login required",
  connected: "Connected",
  needs_reauth: "Reconnect needed",
  error: "Error",
};

function statusChipClass(status: string): string {
  switch (status) {
    case "connected":
      return "border-[#4fae7e] bg-[color-mix(in_srgb,#4fae7e_18%,transparent)] text-[#7fd6a6]";
    case "needs_reauth":
    case "error":
      return "border-[#e0a060] bg-[color-mix(in_srgb,#e0a060_18%,transparent)] text-[#e0b080]";
    default:
      return "border-[var(--color-border-strong)] bg-[var(--color-overlay-soft)] text-[var(--color-text-secondary)]";
  }
}

async function fetchServers(): Promise<RemoteServer[] | null> {
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
  const data = (await res.json()) as ListResponse;
  return data.servers ?? [];
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
    return { notice: n && n !== "1" ? `Connected ${n}.` : "Connected.", error: null };
  }
  if (params.get("error")) {
    return { notice: null, error: `Authorization failed: ${params.get("error")}` };
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
  const [catalog, setCatalog] = useState<CatalogResponse | null>(null);
  const [catalogOpen, setCatalogOpen] = useState(false);
  const [error, setError] = useState<string | null>(initialBanner.error);
  const [notice, setNotice] = useState<string | null>(initialBanner.notice);
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState(false);
  const [name, setName] = useState("");
  const [url, setUrl] = useState("");

  const apply = (isStale: () => boolean) => {
    fetchServers()
      .then((list) => {
        if (isStale() || list === null) return;
        setServers(list);
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
    fetch(`/api/remote-mcp-servers/${encodeURIComponent(id)}/authorize`, { method: "POST" })
      .then(async (res) => {
        if (!res.ok) {
          throw new Error((await res.text()) || `Authorize failed: ${res.status}`);
        }
        const data = (await res.json()) as { redirect_url?: string };
        if (!data.redirect_url) throw new Error("No authorization URL returned.");
        // Full-page navigation to the authorization server. It redirects back to
        // /api/oauth/mcp/callback, which returns here with ?connected / ?error.
        window.location.href = data.redirect_url;
      })
      .catch((err: unknown) => {
        setError(errMessage(err));
        setBusy(false);
      });
  };

  // One-click add from the third-party directory: same POST as the manual
  // form, prefilled from the curated entry. The server lands in
  // "login_required"; the user then clicks Connect like any manual add.
  const addFromCatalog = (entry: CatalogThirdParty) => {
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

  const disconnect = (id: string, label: string) => {
    if (!window.confirm(`Disconnect "${label}"? Its stored tokens are revoked and removed.`)) {
      return;
    }
    setError(null);
    setBusy(true);
    fetch(`/api/remote-mcp-servers/${encodeURIComponent(id)}`, { method: "DELETE" })
      .then(async (res) => {
        if (!res.ok && res.status !== 204) {
          throw new Error((await res.text()) || `Disconnect failed: ${res.status}`);
        }
        setNotice("Disconnected.");
        refresh();
      })
      .catch((err: unknown) => setError(errMessage(err)))
      .finally(() => setBusy(false));
  };

  return (
    <main className="min-h-screen bg-[var(--gradient-bg-home-signature)] px-6 py-10 text-[var(--color-text-primary)]">
      <div className="mx-auto w-full max-w-3xl">
        <header className="mb-6 flex items-center justify-between gap-4">
          <Link href="/" className="flex items-center gap-2.5 no-underline">
            <Image src="/logos/elcano-mark-primary.svg" alt="Elcano" width={28} height={28} priority />
            <span className="font-heading text-[0.9375rem] font-semibold">Connections</span>
          </Link>
          <Link
            href="/"
            className="rounded-full border border-[var(--color-border-strong)] px-3 py-1 text-[0.8125rem] text-[var(--color-text-secondary)] transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)]"
          >
            Back to chat
          </Link>
        </header>

        <p className="mb-5 text-[0.875rem] text-[var(--color-text-secondary)]">
          Connect remote (hosted) MCP servers and sign in to each with your own account. Connected
          servers&apos; tools become available to you in chat and your scheduled tasks. Credentials are
          stored encrypted on the server and never shared with other users.
        </p>

        {notice ? (
          <div className="mb-4 rounded-[0.95rem] border border-[#4fae7e] bg-[color-mix(in_srgb,#4fae7e_15%,transparent)] px-4 py-3 text-[0.875rem] text-[#7fd6a6]">
            {notice}
          </div>
        ) : null}
        {error ? (
          <div className="mb-4 rounded-[0.95rem] border border-[#e08080] bg-[color-mix(in_srgb,#e08080_15%,transparent)] px-4 py-3 text-[0.875rem] text-[#e08080]">
            {error}
          </div>
        ) : null}

        <form
          onSubmit={addServer}
          className="mb-6 rounded-[1rem] border border-[var(--color-border)] bg-[var(--gradient-surface-panel)] p-4"
        >
          <h2 className="mb-3 text-[0.9rem] font-semibold">Add a remote MCP server</h2>
          <div className="grid gap-3 sm:grid-cols-[1fr_2fr_auto] sm:items-end">
            <label className="grid gap-1 text-[0.75rem] text-[var(--color-text-muted)]">
              Name
              <input
                value={name}
                onChange={(e) => setName(e.target.value)}
                placeholder="my-server"
                required
                className="rounded-[0.6rem] border border-[var(--color-border-strong)] bg-[var(--color-overlay-soft)] px-3 py-2 text-[0.875rem] text-[var(--color-text-primary)] outline-none focus:border-[var(--color-text-secondary)]"
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
                className="rounded-[0.6rem] border border-[var(--color-border-strong)] bg-[var(--color-overlay-soft)] px-3 py-2 text-[0.875rem] text-[var(--color-text-primary)] outline-none focus:border-[var(--color-text-secondary)]"
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
            <p className="px-4 py-5 text-center text-[0.875rem] text-[var(--color-text-muted)]">Loading…</p>
          ) : servers && servers.length > 0 ? (
            <ul>
              {servers.map((s) => (
                <li
                  key={s.id}
                  className="flex flex-wrap items-center justify-between gap-3 border-b border-[var(--color-border-subtle)] px-4 py-3 last:border-none"
                >
                  <div className="min-w-0">
                    <div className="flex items-center gap-2">
                      <span className="font-medium text-[var(--color-text-primary)]">{s.name}</span>
                      <span
                        className={`rounded-full border px-2 py-0.5 text-[0.6875rem] ${statusChipClass(s.status)}`}
                      >
                        {STATUS_LABEL[s.status] ?? s.status}
                      </span>
                    </div>
                    <p className="truncate text-[0.75rem] text-[var(--color-text-muted)]">{s.url}</p>
                    {s.status_detail ? (
                      <p className="text-[0.6875rem] text-[#e0b080]">{s.status_detail}</p>
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
                      onClick={() => disconnect(s.id, s.name)}
                      disabled={busy}
                      className="rounded-full border border-[var(--color-border-subtle)] px-3 py-1 text-[0.75rem] text-[var(--color-text-secondary)] transition hover:bg-[var(--color-overlay-soft)] disabled:opacity-50"
                    >
                      Remove
                    </button>
                  </div>
                </li>
              ))}
            </ul>
          ) : (
            <p className="px-4 py-5 text-center text-[0.875rem] text-[var(--color-text-muted)]">
              No remote servers yet. Add one above to get started.
            </p>
          )}
        </div>

        {catalog && (catalog.third_party.length > 0 || catalog.bundled.length > 0) ? (
          <div className="mt-6 overflow-hidden rounded-[1rem] border border-[var(--color-border)] bg-[var(--gradient-surface-panel)]">
            <button
              type="button"
              onClick={() => setCatalogOpen((v) => !v)}
              className="flex w-full items-center justify-between px-4 py-3 text-left"
            >
              <span className="text-[0.75rem] uppercase tracking-wide text-[var(--color-text-muted)]">
                Connector catalog
              </span>
              <span className="text-[0.75rem] text-[var(--color-text-secondary)]">
                {catalogOpen ? "Hide" : `Browse ${catalog.third_party.length + catalog.bundled.length}`}
              </span>
            </button>
            {catalogOpen ? (
              <div className="border-t border-[var(--color-border)]">
                {catalog.bundled.length > 0 ? (
                  <section className="border-b border-[var(--color-border-subtle)] px-4 py-3">
                    <h3 className="mb-1 text-[0.8125rem] font-semibold">Bundled by your workspace</h3>
                    <p className="mb-2 text-[0.75rem] text-[var(--color-text-muted)]">
                      Reviewed and shipped by your operator. These run inside the sandbox on this
                      deployment with credentials held server-side — nothing leaves the box except
                      the connector&apos;s own API calls. Toggle them per conversation in the Tools
                      picker.
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
                            <span className="rounded-full border border-[#4fae7e] bg-[color-mix(in_srgb,#4fae7e_15%,transparent)] px-2 py-0.5 text-[0.625rem] text-[#7fd6a6]">
                              Bundled
                            </span>
                            {b.beta ? (
                              <span className="rounded-full border border-[var(--color-border-strong)] px-2 py-0.5 text-[0.625rem] text-[var(--color-text-muted)]">
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
                    <h3 className="mb-1 text-[0.8125rem] font-semibold">Third-party hosted</h3>
                    <p className="mb-2 text-[0.75rem] text-[var(--color-text-muted)]">
                      Official servers run by the named vendor, not by your workspace. Connecting
                      one signs you in with that vendor and sends tool calls — which can include
                      parts of your conversation — to their service under their terms.
                    </p>
                    <ul className="grid gap-2 sm:grid-cols-2">
                      {catalog.third_party.map((tp) => {
                        const already = (servers ?? []).some((s) => s.url === tp.url);
                        return (
                          <li
                            key={tp.name}
                            className="flex flex-col rounded-[0.75rem] border border-[var(--color-border-subtle)] px-3 py-2"
                          >
                            <div className="flex items-center gap-2">
                              <span className="text-[0.8125rem] font-medium">{tp.display_name}</span>
                              <span className="rounded-full border border-[#e0a060] bg-[color-mix(in_srgb,#e0a060_15%,transparent)] px-2 py-0.5 text-[0.625rem] text-[#e0b080]">
                                Third-party
                              </span>
                            </div>
                            <p className="mt-1 line-clamp-2 flex-1 text-[0.75rem] text-[var(--color-text-muted)]">
                              {tp.description}
                            </p>
                            <div className="mt-2 flex items-center justify-between gap-2">
                              <span className="truncate text-[0.6875rem] text-[var(--color-text-muted)]">
                                {tp.vendor || new URL(tp.url).hostname}
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
                              </span>
                              {catalog.remote_mcp_enabled ? (
                                <button
                                  type="button"
                                  onClick={() => addFromCatalog(tp)}
                                  disabled={busy || already}
                                  className="rounded-full border border-[var(--color-border-strong)] px-3 py-1 text-[0.6875rem] transition hover:bg-[var(--color-overlay-soft)] disabled:opacity-50"
                                >
                                  {already ? "Added" : "Add"}
                                </button>
                              ) : null}
                            </div>
                          </li>
                        );
                      })}
                    </ul>
                    {!catalog.remote_mcp_enabled ? (
                      <p className="mt-2 text-[0.6875rem] text-[var(--color-text-muted)]">
                        Connecting third-party servers requires the operator to configure remote MCP
                        OAuth (FLEET_MCP_OAUTH_ENCRYPTION_KEY and FLEET_PUBLIC_BASE_URL).
                      </p>
                    ) : null}
                  </section>
                ) : null}
              </div>
            ) : null}
          </div>
        ) : null}
      </div>
    </main>
  );
}
