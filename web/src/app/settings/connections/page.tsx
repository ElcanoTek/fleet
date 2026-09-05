"use client";

import { useCallback, useEffect, useRef, useState } from "react";

import { Icon } from "@/app/shared/ui/Icon";
import {
  authHint,
  categoriesOf,
  categoryIcon,
  connectorParamOf,
  consentRequired,
  FEATURED_SLUG,
  effectiveAutoEnable,
  effectiveEnabled,
  fillPlaceholders,
  filterCatalog,
  groupByCategory,
  placeholderLabel,
  placeholdersOf,
  placeholderValueOK,
  prefFor,
  provenanceBadge,
  setupLink,
  toolCountSuffix,
  type CatalogBundled,
  type CatalogResponse,
  type CatalogThirdParty,
  type ConnectorPref,
} from "./catalog";
import { CredentialAccountAdmin } from "./CredentialAccountAdmin";
import {
  btnClass,
  ClampText,
  CodeChip,
  ConnBadge,
  InlineConfirmButton,
  RevealButton,
  SetSwitch,
  SETTINGS_INPUT,
  type BadgeVariant,
} from "../ui/atoms";
import { useIsAdmin } from "../useIsAdmin";
import {
  ConnEmpty,
  ConnField,
  ConnForm,
  ConnGroup,
  ConnGroupHead,
  ConnPanel,
  ConnPanelHead,
  ConnPanelSub,
  ConnRow,
  ConnRows,
  DirCatHead,
  DirChip,
  DirSearch,
  SetSection,
} from "../ui/panels";
import { useMcpServers } from "@/app/shared/hooks/useMcpServers";
import { ToastProvider, useToast } from "@/app/shared/ui/Toast";
import { DialogShell } from "@/app/shared/ui/DialogShell";

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
  // oauth | open | api_key — drives the row's connect affordance (OAuth
  // servers get Connect/Reconnect; api_key servers get Update key; open
  // servers need neither). Absent on pre-migration rows ⇒ treated as oauth.
  auth_kind?: string;
  // Multi-login (#988): one row per seat (login) under a connection name.
  // `account` is the seat's label — "" is the unlabeled seat every
  // pre-existing connection is, rendered "primary". `is_default` marks the
  // seat chats and tasks mount when nothing picks another.
  account: string;
  is_default: boolean;
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
// sandboxed connectors, surfaced under "Your connections" with the per-user
// availability toggle; third-party entries are hosted servers the user can
// add to the connect flow. Types + the grouping/search/provenance helpers
// live in ./catalog (unit-tested there).

const STATUS_LABEL: Record<string, string> = {
  login_required: "Login required",
  connected: "Connected",
  needs_reauth: "Reconnect needed",
  error: "Error",
};

const STATUS_VARIANTS: Record<string, BadgeVariant> = {
  connected: "success",
  needs_reauth: "warn",
  error: "warn",
};

function statusVariant(status: string): BadgeVariant {
  return STATUS_VARIANTS[status] ?? "neutral";
}

const PROVENANCE_VARIANTS: Record<string, BadgeVariant> = {
  Official: "success",
  Aggregator: "warn",
};

// provenanceVariant maps the catalog helper's trust label onto the design's
// badge palette: Official reads success, Aggregator reads warn (it sees your
// traffic), Community reads neutral — the group intro carries the caution.
function provenanceVariant(provenance: string): BadgeVariant {
  const label = provenanceBadge(provenance).label;
  return PROVENANCE_VARIANTS[label] ?? "neutral";
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

// seatLabel renders a seat's account label; the unlabeled seat is "primary".
function seatLabel(s: { account?: string }): string {
  return s.account || "primary";
}

// groupSeats buckets the caller's own rows by connection name, first
// appearance order, so several logins to one server render as one visual
// group (one row per seat) instead of N look-alike rows.
function groupSeats(
  servers: RemoteServer[],
): { name: string; seats: RemoteServer[] }[] {
  const out: { name: string; seats: RemoteServer[] }[] = [];
  for (const s of servers) {
    const g = out.find((x) => x.name === s.name);
    if (g) g.seats.push(s);
    else out.push({ name: s.name, seats: [s] });
  }
  return out;
}

// readConnectorSpotlight reads the one-shot ?connector=<name> deep link — the
// "take me straight to pasting my key" entry point (docs and the browserbase
// skill link it). Lazy-read during render like readCallbackBanner; SSR-guarded.
function readConnectorSpotlight(): string | null {
  if (typeof window === "undefined") return null;
  return connectorParamOf(window.location.search);
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

// ToggleForMe — the design's .conn-toggle: a small switch plus a clickable
// state label ("Enabled for me" / "Off for me" …). The switch is the
// accessible control; the label is a convenience click target.
function ToggleForMe({
  on,
  onLabel,
  offLabel,
  ariaLabel,
  onToggle,
  disabled,
  title,
}: {
  on: boolean;
  onLabel: string;
  offLabel: string;
  ariaLabel: string;
  onToggle: () => void;
  disabled?: boolean;
  title?: string;
}) {
  return (
    <span className="inline-flex items-center gap-2" title={title}>
      <SetSwitch
        small
        on={on}
        onToggle={onToggle}
        label={ariaLabel}
        disabled={disabled}
      />
      {/* The state text is a convenience hit target for pointer users only:
          it duplicates the switch beside it, which already carries the name,
          the role and the checked state. So it is a real <button> (keyboard
          activation and a click both fire onToggle, no synthetic key handling
          on a <span>) that is kept out of the tab order and out of the
          accessibility tree — an assistive-technology user meets exactly one
          control here instead of a switch followed by a mystery button that
          does the same thing. */}
      <button
        type="button"
        tabIndex={-1}
        aria-hidden="true"
        className="cursor-pointer select-none border-none bg-transparent p-0 text-[0.74rem] font-medium text-[var(--color-text-secondary)]"
        onClick={() => {
          if (!disabled) onToggle();
        }}
      >
        {on ? onLabel : offLabel}
      </button>
    </span>
  );
}

// COMPACT_SELECT — the design's .conn-select .settings-input (the inline
// account seat picker on bundled cards). Written out rather than composed as
// SETTINGS_INPUT + overrides: Tailwind v4 orders same-property utilities by
// value, so appended min-h/w/py/text-size "overrides" would lose to the base.
const COMPACT_SELECT =
  "min-h-[1.9rem] w-auto appearance-none rounded-[var(--radius-md)] border border-[var(--color-border)] bg-[var(--color-overlay-soft)] py-[0.15rem] pl-[0.6rem] pr-[1.9rem] text-[0.75rem] text-[var(--color-text-primary)] outline-none focus-visible:border-[var(--color-border-strong)] focus-visible:shadow-[var(--focus-ring)]";

// BundledCard — one operator-bundled connector (.conn-card): availability
// toggle, optional default credential-account seat, and the explicit-pref
// reset. Top-level so ClampText's expand state survives parent re-renders.
function DataSourceChips({ sources }: { sources?: string[] }) {
  if (!sources?.length) return null;
  return (
    <div
      className="flex flex-wrap items-center gap-[0.35rem]"
      data-testid="data-sources"
    >
      <span className="text-[0.68rem] uppercase tracking-wide text-[var(--color-text-muted)]">
        Reads
      </span>
      {sources.map((s) => (
        <code
          key={s}
          className="rounded-[var(--radius-sm)] border border-[var(--color-border-subtle)] bg-[var(--color-overlay-soft)] px-[0.4rem] py-[0.1rem] text-[0.68rem] text-[var(--color-text-secondary)] [overflow-wrap:anywhere]"
        >
          {s}
        </code>
      ))}
    </div>
  );
}

function BundledCard({
  entry,
  pref,
  on,
  autoOn,
  busy,
  onToggle,
  onToggleAuto,
  onPickAccount,
  onReset,
}: {
  entry: CatalogBundled;
  pref: ConnectorPref | undefined;
  on: boolean;
  autoOn: boolean;
  busy: boolean;
  onToggle: () => void;
  onToggleAuto: () => void;
  onPickAccount: (account: string) => void;
  onReset: () => void;
}) {
  const label = entry.display_name || entry.name;
  return (
    <div
      data-testid={`bundled-card-${entry.name}`}
      className="flex flex-col gap-[0.55rem] rounded-[var(--radius-md)] border border-[var(--color-border)] bg-[var(--color-overlay-soft)] px-[0.9rem] py-[0.8rem]"
    >
      <div className="flex flex-wrap items-center gap-[0.55rem]">
        <span className="text-[0.85rem] font-semibold text-[var(--color-text-primary)] [overflow-wrap:anywhere]">
          {label}
        </span>
        <ConnBadge variant="success">Bundled</ConnBadge>
        {entry.beta ? <ConnBadge>Beta</ConnBadge> : null}
      </div>
      {entry.description ? <ClampText text={entry.description} /> : null}
      <DataSourceChips sources={entry.data_sources} />
      <div className="mt-auto flex flex-wrap items-center gap-[0.8rem]">
        <ToggleForMe
          on={on}
          onLabel="Enabled for me"
          offLabel="Disabled for me"
          ariaLabel={`Enable ${label} for me`}
          onToggle={onToggle}
          disabled={busy}
          title="Off hides this connector from your chat pickers and runs; scheduled tasks keep their own pinned selection."
        />
        {on ? (
          <ToggleForMe
            on={autoOn}
            onLabel="On for new chats"
            offLabel="Off for new chats"
            ariaLabel={`Start ${label} enabled in new chats`}
            onToggle={onToggleAuto}
            disabled={busy}
            title="On seeds this connector enabled in every new conversation; off means you flip it on per conversation from the Tools picker."
          />
        ) : null}
        {on && (entry.accounts?.length ?? 0) > 0 ? (
          <label className="inline-flex items-center gap-[0.45rem] text-[0.73rem] text-[var(--color-text-muted)]">
            Account
            <span className="select-wrap inline-block">
              <select
                className={COMPACT_SELECT}
                value={pref?.default_account ?? ""}
                onChange={(e) => onPickAccount(e.target.value)}
                disabled={busy}
                aria-label={`${entry.name} credential account`}
              >
                <option value="">Default seat</option>
                {(entry.accounts ?? []).map((a) => (
                  <option key={a} value={a}>
                    {a}
                  </option>
                ))}
              </select>
            </span>
          </label>
        ) : null}
        {pref ? (
          <button
            type="button"
            onClick={onReset}
            disabled={busy}
            className="border-none bg-transparent p-0 text-[0.72rem] text-[var(--color-accent)] underline underline-offset-2 disabled:opacity-50"
          >
            Reset
          </button>
        ) : null}
      </div>
    </div>
  );
}

// AddOverrides carries what a DirectoryCard's guided form collected beyond the
// static catalog entry: the tenant URL with its {placeholders} filled in, the
// pasted API key (write-only; sealed server-side, never echoed), and/or a
// bring-your-own OAuth client for vendors without dynamic registration.
type AddOverrides = {
  url?: string;
  apiKey?: string;
  clientId?: string;
  clientSecret?: string;
  // Seat label for "Add another account" (#988) — required by the backend
  // for any second login under a name.
  account?: string;
};

// dirAddButtonClass — the pill Add/Set up button on a directory card.
function dirAddButtonClass(added: boolean): string {
  return [
    "shrink-0 rounded-[var(--radius-pill)] border bg-transparent px-[0.85rem] py-[0.24rem] text-[0.76rem] font-medium transition focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]",
    added
      ? "border-[var(--color-success-border)] text-[var(--color-success)]"
      : "border-[var(--color-border-strong)] text-[var(--color-text-primary)] hover:border-[var(--color-accent)] hover:bg-[var(--color-overlay-soft)] disabled:opacity-50",
  ].join(" ");
}

// DirectoryCard — one third-party entry (.dir-card): provenance badge,
// clamped description, vendor + vetting + setup-guide links, a VISIBLE setup
// hint, and the gated Add. Entries a user can't one-click add get a guided
// inline form instead of a bare "needs your URL" label: one input per
// {placeholder} in a tenant-scoped URL template (with a live preview of the
// resulting endpoint), and a write-only key field for api_key entries.
function DirectoryCard({
  entry,
  added,
  busy,
  remoteEnabled,
  redirectUri,
  onAdd,
  autoOpenForm,
}: {
  entry: CatalogThirdParty;
  added: boolean;
  busy: boolean;
  remoteEnabled: boolean;
  redirectUri?: string;
  // Resolves true when the server was actually added (validated + stored);
  // false keeps the guided form — and whatever the user typed — open so a
  // rejected key or mistyped tenant value can be corrected in place.
  onAdd: (overrides?: AddOverrides) => Promise<boolean>;
  // ?connector= deep link: mount with the guided form already open (and the
  // key field focused), so the linked user lands one paste from connected.
  // Initial state only — an already-added entry has no form to open.
  autoOpenForm?: boolean;
}) {
  const hint = authHint(entry);
  const guide = setupLink(entry);
  const placeholders = placeholdersOf(entry.url);
  const manualClient =
    entry.client_registration === "manual" && entry.auth !== "api_key";
  // The vendor accepts no public clients: a client ID without its secret sails
  // through the consent screen into a token exchange the vendor refuses, so
  // the secret is mandatory here rather than "optional" (GitHub, #1006).
  const secretRequired = manualClient && entry.client_secret === "required";
  const needsForm =
    placeholders.length > 0 || entry.auth === "api_key" || manualClient;
  const [formOpen, setFormOpen] = useState(
    (autoOpenForm ?? false) && needsForm && !added,
  );
  const [values, setValues] = useState<Record<string, string>>({});
  const [apiKey, setApiKey] = useState("");
  // "Add another account" (#988): an already-added entry can take a second
  // login. The same guided form opens, plus a REQUIRED seat label — the
  // backend rejects an unlabeled second seat.
  const [account, setAccount] = useState("");
  const anotherAccount = added;
  // ?connector= deep link: put the cursor in the key field the moment the
  // guided form opens — the whole point of the link is "just paste". Done as
  // an explicit focus() rather than autoFocus so the move is tied to the form
  // actually opening (and re-opening), which is the event a screen-reader user
  // can follow, instead of to whenever React happens to mount the input.
  const apiKeyRef = useRef<HTMLInputElement | null>(null);
  const [clientId, setClientId] = useState("");
  const [clientSecret, setClientSecret] = useState("");

  const focusKeyField = (autoOpenForm ?? false) && formOpen && !added;
  useEffect(() => {
    if (focusKeyField) apiKeyRef.current?.focus();
  }, [focusKeyField]);

  const filledURL = fillPlaceholders(entry.url, values);
  const ready =
    placeholders.every((ph) => placeholderValueOK(values[ph] ?? "")) &&
    (entry.auth !== "api_key" || apiKey.trim() !== "") &&
    (!manualClient || clientId.trim() !== "") &&
    (!secretRequired || clientSecret.trim() !== "") &&
    (!anotherAccount || account.trim() !== "");

  const submit = async () => {
    const ok = await onAdd({
      ...(placeholders.length > 0 ? { url: filledURL } : {}),
      ...(anotherAccount ? { account: account.trim() } : {}),
      ...(entry.auth === "api_key" ? { apiKey: apiKey.trim() } : {}),
      ...(manualClient
        ? {
            clientId: clientId.trim(),
            ...(clientSecret.trim()
              ? { clientSecret: clientSecret.trim() }
              : {}),
          }
        : {}),
    });
    if (!ok) return; // validation failed (or consent pending) — keep the form + values
    setFormOpen(false);
    // Drop the secrets from component state the moment the add succeeds.
    setApiKey("");
    setClientSecret("");
    setAccount("");
  };

  const linkClass =
    "border-b border-dotted border-[var(--color-border-strong)] text-[var(--color-text-secondary)] no-underline hover:text-[var(--color-text-primary)]";
  return (
    <div
      data-testid={`dir-card-${entry.name}`}
      className="flex flex-col gap-2 rounded-[var(--radius-md)] border border-[var(--color-border)] bg-[var(--color-surface-1)] px-[0.95rem] py-[0.85rem]"
    >
      <div className="flex flex-wrap items-center gap-[0.55rem]">
        <Icon
          name={categoryIcon(entry.category ?? "")}
          className="size-4 shrink-0 text-[var(--color-text-muted)]"
        />
        <span className="text-[0.85rem] font-semibold text-[var(--color-text-primary)] [overflow-wrap:anywhere]">
          {entry.display_name}
        </span>
        <ConnBadge variant={provenanceVariant(entry.provenance)}>
          {provenanceBadge(entry.provenance).label}
        </ConnBadge>
      </div>
      {entry.description ? <ClampText text={entry.description} /> : null}
      {entry.setup_hint ? (
        <p className="m-0 text-[0.72rem] leading-[1.5] text-[var(--color-text-secondary)]">
          {entry.setup_hint}
          {guide ? (
            <>
              {" "}
              <a
                href={guide}
                target="_blank"
                rel="noreferrer"
                className={linkClass}
              >
                Setup guide
              </a>
            </>
          ) : null}
        </p>
      ) : null}
      <div className="mt-auto flex items-center gap-[0.6rem] text-[0.72rem] text-[var(--color-text-muted)]">
        <span className="min-w-0 flex-1 truncate">
          {entry.vendor || entry.url}
          {entry.docs_url ? (
            <>
              {" · "}
              <a
                href={entry.docs_url}
                target="_blank"
                rel="noreferrer"
                className={linkClass}
              >
                docs
              </a>
            </>
          ) : null}
          {entry.repo_url ? (
            <>
              {" · "}
              <a
                href={entry.repo_url}
                target="_blank"
                rel="noreferrer"
                className={linkClass}
              >
                source
              </a>
            </>
          ) : null}
          {entry.setup_url && !entry.setup_hint ? (
            <>
              {" · "}
              <a
                href={entry.setup_url}
                target="_blank"
                rel="noreferrer"
                className={linkClass}
              >
                setup guide
              </a>
            </>
          ) : null}
        </span>
        {remoteEnabled ? (
          <>
            {hint ? (
              <span className="shrink-0 whitespace-nowrap">{hint}</span>
            ) : null}
            {added ? (
              <button
                type="button"
                data-testid={`dir-add-account-${entry.name}`}
                aria-expanded={formOpen}
                onClick={() => setFormOpen((o) => !o)}
                disabled={busy}
                className={dirAddButtonClass(false)}
              >
                {formOpen ? "Cancel" : "Add another account"}
              </button>
            ) : null}
            <button
              type="button"
              data-testid={`dir-add-${entry.name}`}
              aria-expanded={needsForm ? formOpen : undefined}
              onClick={() =>
                needsForm ? setFormOpen((o) => !o) : void onAdd()
              }
              disabled={busy || added}
              className={dirAddButtonClass(added)}
            >
              {added
                ? "Added"
                : needsForm
                  ? formOpen
                    ? "Cancel"
                    : "Set up…"
                  : "Add"}
            </button>
          </>
        ) : null}
      </div>
      {remoteEnabled && (needsForm || anotherAccount) && formOpen ? (
        <FormShell
          entry={entry}
          modal={manualClient}
          onClose={() => setFormOpen(false)}
        >
          {anotherAccount ? (
            <label className="grid gap-1 text-[0.72rem] text-[var(--color-text-secondary)]">
              <span className="font-medium">Account label</span>
              <input
                className={SETTINGS_INPUT}
                value={account}
                onChange={(e) => setAccount(e.target.value)}
                placeholder="work, personal…"
                data-testid={`dir-form-account-${entry.name}`}
              />
              <span className="text-[0.68rem] text-[var(--color-text-muted)]">
                Tells the seats apart in the Tools picker. Letters, digits and
                underscores.
              </span>
            </label>
          ) : null}
          {placeholders.map((ph) => (
            <label
              key={ph}
              className="grid gap-1 text-[0.72rem] text-[var(--color-text-secondary)]"
            >
              <span className="font-medium">Your {placeholderLabel(ph)}</span>
              <input
                className={SETTINGS_INPUT}
                value={values[ph] ?? ""}
                onChange={(e) =>
                  setValues((cur) => ({ ...cur, [ph]: e.target.value }))
                }
                placeholder={ph}
              />
            </label>
          ))}
          {placeholders.length > 0 ? (
            <p className="m-0 break-all font-mono text-[0.68rem] text-[var(--color-text-muted)]">
              {filledURL}
            </p>
          ) : null}
          {entry.auth === "api_key" ? (
            <label className="grid gap-1 text-[0.72rem] text-[var(--color-text-secondary)]">
              <span className="font-medium">API key</span>
              <input
                ref={apiKeyRef}
                className={SETTINGS_INPUT}
                type="password"
                autoComplete="off"
                value={apiKey}
                onChange={(e) => setApiKey(e.target.value)}
                placeholder="paste your key (stored encrypted, never shown again)"
              />
            </label>
          ) : null}
          {manualClient ? (
            <>
              {redirectUri ? (
                // min-w-0 (here and on the row) keeps the nowrap URL's intrinsic
                // width from inflating the form grid's column — without it every
                // field in the dialog stretches past the container.
                <div className="grid min-w-0 gap-1 text-[0.72rem] text-[var(--color-text-secondary)]">
                  <span className="font-medium">
                    Authorization callback URL for your app registration
                  </span>
                  <div className="flex min-w-0 items-center gap-2">
                    <code className="min-w-0 flex-1 truncate rounded-[var(--radius-md)] border border-[var(--color-border)] bg-[var(--color-overlay-soft)] px-2 py-1.5 font-[family-name:var(--font-code)] text-[0.72rem] text-[var(--color-text-primary)]">
                      {redirectUri}
                    </code>
                    <button
                      type="button"
                      className={btnClass({ reveal: true })}
                      onClick={() =>
                        navigator.clipboard?.writeText(redirectUri)
                      }
                    >
                      Copy
                    </button>
                  </div>
                </div>
              ) : null}
              <label className="grid gap-1 text-[0.72rem] text-[var(--color-text-secondary)]">
                <span className="font-medium">OAuth client ID</span>
                <input
                  className={SETTINGS_INPUT}
                  value={clientId}
                  onChange={(e) => setClientId(e.target.value)}
                  placeholder="from your app registration — see the setup guide"
                  data-testid={`dir-form-client-id-${entry.name}`}
                />
              </label>
              <label className="grid gap-1 text-[0.72rem] text-[var(--color-text-secondary)]">
                <span className="font-medium">
                  {secretRequired
                    ? "OAuth client secret"
                    : "OAuth client secret (if your client has one)"}
                </span>
                <input
                  className={SETTINGS_INPUT}
                  type="password"
                  autoComplete="off"
                  required={secretRequired}
                  value={clientSecret}
                  onChange={(e) => setClientSecret(e.target.value)}
                  placeholder="stored encrypted, never shown again"
                  data-testid={`dir-form-client-secret-${entry.name}`}
                />
                {secretRequired ? (
                  <span className="text-[0.68rem] text-[var(--color-text-muted)]">
                    Required: this vendor rejects the token exchange without
                    the secret.
                  </span>
                ) : null}
              </label>
            </>
          ) : null}
          <div className="flex justify-end gap-2">
            {manualClient ? (
              <button
                type="button"
                onClick={() => setFormOpen(false)}
                className={btnClass({ reveal: true })}
              >
                Cancel
              </button>
            ) : null}
            <button
              type="button"
              data-testid={`dir-form-add-${entry.name}`}
              onClick={() => void submit()}
              disabled={busy || !ready}
              className={dirAddButtonClass(false)}
            >
              Add
            </button>
          </div>
        </FormShell>
      ) : null}
    </div>
  );
}

// This page's three dialogs (the guided manual-OAuth setup form, the post-add
// sign-in prompt, and the third-party consent step) all sit on the shared
// DialogShell — the same base the chat surface's dialogs use. It owns the
// opaque panel, the scrim-as-<button> (so "click outside to dismiss" is
// reachable from the keyboard and its accessible name says what it does), the
// dialog semantics on the PANEL rather than on the full-screen wrapper,
// Escape, and moving focus into the dialog on open / back to the opener on
// close. It also answers Escape one layer at a time, which is what this page
// needed a hand-rolled "is this the top-most [role=dialog]?" check for: the
// consent step can open on top of the guided setup form.

// FormShell hosts a directory card's guided add form either inline (the card
// grows downward — placeholders, API keys) or, for manual OAuth client
// registration, as a dialog overlay: those forms carry a setup detour (create
// an OAuth app elsewhere, come back with ID + secret) and deserve the focus
// of a modal rather than stretching the card.
function FormShell({
  entry,
  modal,
  onClose,
  children,
}: {
  entry: CatalogThirdParty;
  modal: boolean;
  onClose: () => void;
  children: React.ReactNode;
}) {
  const body = (
    <div
      data-testid={`dir-form-${entry.name}`}
      className="grid gap-2 rounded-[var(--radius-md)] border border-[var(--color-border-subtle)] bg-[var(--color-overlay-soft)] px-3 py-2.5"
    >
      {children}
    </div>
  );
  if (!modal) return body;
  const guide = setupLink(entry);
  return (
    <DialogShell
      label={`Set up ${entry.display_name}`}
      scrimLabel={`Close the ${entry.display_name} setup dialog`}
      onDismiss={onClose}
      className="max-w-md p-5"
    >
      <h3 className="mb-2 text-[0.9375rem] font-semibold">
        Set up {entry.display_name}
      </h3>
      {entry.setup_hint ? (
        <p className="mb-3 text-[0.8125rem] text-[var(--color-text-secondary)]">
          {entry.setup_hint}
          {guide ? (
            <>
              {" "}
              <a
                href={guide}
                target="_blank"
                rel="noreferrer"
                className="underline underline-offset-2"
              >
                Setup guide
              </a>
            </>
          ) : null}
        </p>
      ) : null}
      {body}
    </DialogShell>
  );
}

export default function ConnectionsPage() {
  return (
    <ToastProvider>
      <ConnectionsPageInner />
    </ToastProvider>
  );
}

function ConnectionsPageInner() {
  const [initialBanner] = useState(readCallbackBanner);
  // Admin visibility only (authorization stays server-side): picks which
  // "remote MCP isn't configured" explanation to show.
  const adminState = useIsAdmin();
  const [servers, setServers] = useState<RemoteServer[] | null>(null);
  const [shares, setShares] = useState<Record<string, string[]>>({});
  const [sharedWithMe, setSharedWithMe] = useState<SharedServer[]>([]);
  // Which of your servers has its share panel open, and the pending grantee.
  const [shareOpenFor, setShareOpenFor] = useState<string | null>(null);
  const [shareGrantee, setShareGrantee] = useState("");
  // Which api_key server has its rotate-key form open, and the pending key
  // (write-only; cleared the moment it is submitted).
  const [keyOpenFor, setKeyOpenFor] = useState<string | null>(null);
  const [keyValue, setKeyValue] = useState("");
  // Multi-login (#988): which seat has its rename form open (+ the pending
  // label), and which connection NAME has its "Add another account" form
  // open (+ the pending label and, for api_key servers, the pending key —
  // write-only, cleared on submit).
  const [renameOpenFor, setRenameOpenFor] = useState<string | null>(null);
  const [renameValue, setRenameValue] = useState("");
  const [addSeatFor, setAddSeatFor] = useState<string | null>(null);
  const [addSeatLabel, setAddSeatLabel] = useState("");
  const [addSeatKey, setAddSeatKey] = useState("");
  // Explicit per-user availability choices (unified connector UX); absence of
  // an entry means the operator default.
  const [prefs, setPrefs] = useState<ConnectorPref[]>([]);
  const [catalog, setCatalog] = useState<CatalogResponse | null>(null);
  // The ?connector=<name> deep link: filter the directory to that entry and
  // open its guided form, so "add your Browserbase key" is one click + one
  // paste from anywhere that can mint a URL. One-shot, like the OAuth banner.
  const [spotlight] = useState(readConnectorSpotlight);
  // The directory is the page's main discovery surface — open by default,
  // collapsible for users who only manage existing connections.
  const [catalogOpen, setCatalogOpen] = useState(true);
  // Seeding the search from the spotlight is what filters the directory to the
  // linked entry — search spans every category, so this works wherever the
  // entry is grouped, and clearing the box restores the full directory.
  const [catalogQuery, setCatalogQuery] = useState(spotlight ?? "");
  const [catalogCategory, setCatalogCategory] = useState("");
  // Consent modal state: the non-official (aggregator/community) entry
  // awaiting the user's explicit, operator-named consent, plus whatever the
  // card's guided form collected (tenant URL / API key) so the confirm can
  // complete the same add.
  const [consentFor, setConsentFor] = useState<{
    entry: CatalogThirdParty;
    overrides?: AddOverrides;
  } | null>(null);
  const [error, setError] = useState<string | null>(initialBanner.error);
  const [notice, setNotice] = useState<string | null>(initialBanner.notice);
  const { showToast } = useToast();
  // Post-add "sign in now?" prompt for OAuth servers (id + display name).
  const [connectPromptFor, setConnectPromptFor] = useState<{
    id: string;
    name: string;
  } | null>(null);
  // Set when an OAuth sign-in was opened in a new tab; on return to this tab
  // the server list refreshes so the new connection shows without a reload.
  const awaitingAuthReturn = useRef(false);
  // Every action outcome (including the OAuth callback's one-shot
  // ?connected / ?error result) surfaces as a toast — visible from anywhere
  // on this long page; there is no inline banner copy.
  useEffect(() => {
    if (error) showToast(error, "error", 6000);
  }, [error, showToast]);
  useEffect(() => {
    if (notice) showToast(notice, "success");
  }, [notice, showToast]);
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState(false);
  const [addServerOpen, setAddServerOpen] = useState(false);
  const [name, setName] = useState("");
  const [url, setUrl] = useState("");
  // Optional seat label for the manual add form (#988); "" = the unlabeled
  // "primary" seat, exactly like before.
  const [accountLabel, setAccountLabel] = useState("");
  // The MCP server catalog (names + credential-account names, never secrets)
  // feeds the credential-accounts panel, same as the old General page did.
  const { servers: mcpServers, reload: reloadMcpServers } = useMcpServers(true);
  // Anchor for the directory results: clicking a category pill scrolls back
  // to the top of the list (the sticky bar keeps search + chips in view).
  const directoryResultsRef = useRef<HTMLDivElement | null>(null);
  // The sticky search + chip bar itself — measured on chip clicks so the
  // scroll can land the results just below it instead of underneath it.
  const dirBarRef = useRef<HTMLDivElement | null>(null);

  // apply/refresh are memoized so the focus-refresh effect below can list
  // refresh as its one dependency and subscribe once, instead of tearing down
  // and re-adding the window listener on every render.
  const apply = useCallback((isStale: () => boolean) => {
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
  }, []);

  const refresh = useCallback(() => {
    setError(null);
    setLoading(true);
    apply(() => false);
  }, [apply]);

  // refreshCatalog re-reads bundled accounts + third-party entries — called
  // after credential-account changes so new seats appear in the card selects.
  const refreshCatalog = () => {
    fetchCatalog()
      .then((c) => {
        if (c) setCatalog(c);
      })
      .catch(() => {});
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
    fetch("/api/connector-prefs", { cache: "no-store" })
      .then(async (res) =>
        res.ok ? ((await res.json()) as { prefs: ConnectorPref[] }) : null,
      )
      .then((data) => {
        if (!stale && data) setPrefs(data.prefs ?? []);
      })
      .catch(() => {});
    // Strip the one-shot ?connected / ?error / ?connector params from the URL
    // (banner and spotlight were already derived from them during render).
    // replaceState is not setState, so this stays clear of
    // react-hooks/set-state-in-effect.
    const params = new URLSearchParams(window.location.search);
    if (
      params.get("connected") ||
      params.get("error") ||
      params.get("connector")
    ) {
      window.history.replaceState({}, "", "/settings/connections");
    }
    return () => {
      stale = true;
    };
  }, [apply]);

  // Refresh the list when the user comes back from an OAuth sign-in tab.
  useEffect(() => {
    const onFocus = () => {
      if (!awaitingAuthReturn.current) return;
      awaitingAuthReturn.current = false;
      refresh();
    };
    window.addEventListener("focus", onFocus);
    return () => window.removeEventListener("focus", onFocus);
  }, [refresh]);

  const addServer = (e: React.FormEvent) => {
    e.preventDefault();
    setError(null);
    setNotice(null);
    setBusy(true);
    fetch("/api/remote-mcp-servers", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        name: name.trim(),
        url: url.trim(),
        ...(accountLabel.trim() ? { account: accountLabel.trim() } : {}),
      }),
    })
      .then(async (res) => {
        if (!res.ok) {
          throw new Error((await res.text()) || `Add failed: ${res.status}`);
        }
        const data = (await res.json()) as { id?: string; name?: string };
        setName("");
        setUrl("");
        setAccountLabel("");
        setAddServerOpen(false);
        setNotice("Server added. Click Connect to log in.");
        if (data.id)
          setConnectPromptFor({ id: data.id, name: data.name || "the server" });
        refresh();
      })
      .catch((err: unknown) => setError(errMessage(err)))
      .finally(() => setBusy(false));
  };

  const connect = (id: string) => {
    setError(null);
    setBusy(true);
    // Open the tab NOW, inside the click's user gesture, so popup blockers
    // allow it; it is pointed at the authorization server once the URL
    // arrives. The old behavior (same-tab navigation) remains the fallback
    // when the browser refuses the new tab.
    const authTab = window.open("about:blank", "_blank");
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
        // The URL comes from the remote server's OAuth discovery document —
        // third-party data. Navigating this tab (or a tab whose window.opener
        // is us) to a javascript: URL would run it with scripted access to
        // the app, so only http(s) may pass.
        if (!/^https?:\/\//i.test(data.redirect_url)) {
          throw new Error("Authorization URL has an unsupported scheme.");
        }
        // The authorization server redirects back to /api/oauth/mcp/callback,
        // which lands on this page with ?connected / ?error — in the new tab.
        // This tab refreshes its list when it regains focus.
        if (authTab) {
          authTab.location.href = data.redirect_url;
          awaitingAuthReturn.current = true;
          setBusy(false);
          setNotice("Finish signing in — this page updates when you return.");
        } else {
          window.location.href = data.redirect_url;
        }
      })
      .catch((err: unknown) => {
        authTab?.close();
        setError(errMessage(err));
        setBusy(false);
      });
  };

  // Rotate an api_key connection's key (write-only; the current key is never
  // shown). The server validates the NEW key with a real MCP handshake before
  // storing it — a rejected key leaves the old one untouched and the form
  // open, with the error explaining exactly that.
  const updateKey = (id: string) => {
    setError(null);
    setNotice(null);
    setBusy(true);
    fetch(`/api/remote-mcp-servers/${encodeURIComponent(id)}/key`, {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ api_key: keyValue.trim() }),
    })
      .then(async (res) => {
        if (!res.ok && res.status !== 204) {
          throw new Error((await res.text()) || `Update failed: ${res.status}`);
        }
        const data =
          res.status === 204
            ? null
            : ((await res.json()) as { tool_count?: number });
        setKeyOpenFor(null);
        setKeyValue("");
        setNotice(`API key updated${toolCountSuffix(data?.tool_count)}.`);
        refresh();
      })
      .catch((err: unknown) => setError(errMessage(err)))
      .finally(() => setBusy(false));
  };

  // Multi-login (#988). Make one seat the default among the caller's seats
  // with the same name: chats and tasks mount it unless a conversation or
  // task picks another.
  const setDefaultSeat = (id: string) => {
    setError(null);
    setBusy(true);
    fetch(`/api/remote-mcp-servers/${encodeURIComponent(id)}/default`, {
      method: "POST",
    })
      .then(async (res) => {
        if (!res.ok && res.status !== 204) {
          throw new Error(
            (await res.text()) || `Set default failed: ${res.status}`,
          );
        }
        setNotice("Default account updated.");
        refresh();
      })
      .catch((err: unknown) => setError(errMessage(err)))
      .finally(() => setBusy(false));
  };

  // Rename a seat's account label. The backend canonicalizes (lowercase,
  // spaces/hyphens → _) and rejects duplicates with a message shown verbatim.
  const renameSeat = (id: string) => {
    setError(null);
    setBusy(true);
    fetch(`/api/remote-mcp-servers/${encodeURIComponent(id)}/account`, {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ account: renameValue.trim() }),
    })
      .then(async (res) => {
        if (!res.ok && res.status !== 204) {
          throw new Error((await res.text()) || `Rename failed: ${res.status}`);
        }
        setRenameOpenFor(null);
        setRenameValue("");
        refresh();
      })
      .catch((err: unknown) => setError(errMessage(err)))
      .finally(() => setBusy(false));
  };

  // Add another login under an existing connection name: same POST as any
  // add, with the URL/auth copied from the group's first seat, the REQUIRED
  // label, and — for api_key servers — the new key plus the directory entry's
  // header/query name (a key-in-query server like Browserbase fails its
  // validation probe without it). OAuth seats land in login_required and get
  // the same "sign in now?" prompt a fresh directory add does.
  const addSeat = (group: { name: string; seats: RemoteServer[] }) => {
    const template = group.seats[0];
    if (!template) return;
    const label = addSeatLabel.trim();
    if (!label) return;
    const authKind = template.auth_kind || "oauth";
    const dir = (catalog?.third_party ?? []).find(
      (e) => e.name === group.name,
    );
    setError(null);
    setNotice(null);
    setBusy(true);
    fetch("/api/remote-mcp-servers", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        name: group.name,
        url: template.url,
        ...(authKind === "oauth" ? {} : { auth: authKind }),
        account: label,
        ...(authKind === "api_key"
          ? {
              api_key: addSeatKey.trim(),
              api_key_header: dir?.api_key_header,
              api_key_query: dir?.api_key_query,
            }
          : {}),
      }),
    })
      .then(async (res) => {
        if (!res.ok) {
          throw new Error((await res.text()) || `Add failed: ${res.status}`);
        }
        const data = (await res.json()) as { id?: string; tool_count?: number };
        const shown = `${group.name} (${label})`;
        setAddSeatFor(null);
        setAddSeatLabel("");
        setAddSeatKey("");
        if (authKind === "oauth") {
          setNotice(`${shown} added. Click Connect to sign in.`);
          if (data.id) setConnectPromptFor({ id: data.id, name: shown });
        } else {
          setNotice(`${shown} connected${toolCountSuffix(data.tool_count)}.`);
        }
        refresh();
      })
      .catch((err: unknown) => setError(errMessage(err)))
      .finally(() => setBusy(false));
  };

  // Add from the directory: same POST as the manual form, prefilled from the
  // curated entry plus whatever the card's guided form collected (a tenant URL
  // with its placeholders filled, a pasted API key). OAuth entries land in
  // "login_required" and the user clicks Connect; open and api_key entries are
  // validated server-side with a real MCP handshake and arrive already
  // connected — the success notice carries the observed tool count, and a
  // rejected key/URL resolves false so the card keeps its form (and the
  // typed values) open for correction. An entry whose endpoint is NOT
  // operated by the service's own vendor first goes through an explicit
  // consent step naming the operator (the operator receives tool-call
  // arguments — which can include conversation content — and, for OAuth
  // flows, often holds the delegated access token).
  const requestAddFromCatalog = (
    entry: CatalogThirdParty,
    overrides?: AddOverrides,
  ): Promise<boolean> => {
    if (consentRequired(entry)) {
      setConsentFor({ entry, overrides });
      // Not added yet — the card keeps its form; a confirmed consent add
      // flips `added`, which hides the form anyway.
      return Promise.resolve(false);
    }
    return addFromCatalog(entry, overrides);
  };

  const addFromCatalog = (
    entry: CatalogThirdParty,
    overrides?: AddOverrides,
  ): Promise<boolean> => {
    setConsentFor(null);
    setError(null);
    setNotice(null);
    setBusy(true);
    return fetch("/api/remote-mcp-servers", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        name: entry.name,
        url: overrides?.url ?? entry.url,
        // "tenant" describes the URL shape, not the auth protocol — once the
        // user has supplied their URL the add goes through the default OAuth
        // discovery path.
        auth: entry.auth === "tenant" ? undefined : entry.auth,
        ...(overrides?.account ? { account: overrides.account } : {}),
        ...(overrides?.apiKey
          ? {
              api_key: overrides.apiKey,
              api_key_header: entry.api_key_header,
              api_key_query: entry.api_key_query,
            }
          : {}),
        ...(overrides?.clientId
          ? {
              client_id: overrides.clientId,
              client_secret: overrides.clientSecret,
            }
          : {}),
      }),
    })
      .then(async (res) => {
        if (!res.ok) {
          throw new Error((await res.text()) || `Add failed: ${res.status}`);
        }
        const data = (await res.json()) as { id?: string; tool_count?: number };
        const shown = overrides?.account
          ? `${entry.display_name} (${overrides.account})`
          : entry.display_name;
        if (entry.auth === "open" || entry.auth === "api_key") {
          setNotice(`${shown} connected${toolCountSuffix(data.tool_count)}.`);
        } else {
          setNotice(`${shown} added. Click Connect to sign in.`);
          // OAuth adds land in login_required — offer the sign-in right here
          // instead of making the user find the row in "Your connections".
          if (data.id) setConnectPromptFor({ id: data.id, name: shown });
        }
        refresh();
        return true;
      })
      .catch((err: unknown) => {
        setError(errMessage(err));
        return false;
      })
      .finally(() => setBusy(false));
  };

  // Availability layer: write (or revert) an explicit per-user choice. Off =
  // the connector disappears from your chat pickers and runs; for bundled
  // connectors the seat is the credential account your chats use by default.
  const setConnectorPref = (pref: ConnectorPref) => {
    setError(null);
    setBusy(true);
    fetch("/api/connector-prefs", {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(pref),
    })
      .then(async (res) => {
        if (!res.ok && res.status !== 204) {
          throw new Error((await res.text()) || `Save failed: ${res.status}`);
        }
        setPrefs((cur) => [
          ...cur.filter(
            (p) =>
              !(p.kind === pref.kind && p.connector_id === pref.connector_id),
          ),
          pref,
        ]);
      })
      .catch((err: unknown) => setError(errMessage(err)))
      .finally(() => setBusy(false));
  };

  const resetConnectorPref = (kind: "bundled" | "remote", id: string) => {
    setError(null);
    setBusy(true);
    fetch(
      `/api/connector-prefs?kind=${encodeURIComponent(kind)}&id=${encodeURIComponent(id)}`,
      { method: "DELETE" },
    )
      .then(async (res) => {
        if (!res.ok && res.status !== 204) {
          throw new Error((await res.text()) || `Reset failed: ${res.status}`);
        }
        setPrefs((cur) =>
          cur.filter((p) => !(p.kind === kind && p.connector_id === id)),
        );
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
          throw new Error(
            (await res.text()) || `Unshare failed: ${res.status}`,
          );
        }
        refresh();
      })
      .catch((err: unknown) => setError(errMessage(err)))
      .finally(() => setBusy(false));
  };

  const signOut = (id: string) => {
    setError(null);
    setBusy(true);
    fetch(`/api/remote-mcp-servers/${encodeURIComponent(id)}/signout`, {
      method: "POST",
    })
      .then(async (res) => {
        if (!res.ok && res.status !== 204) {
          throw new Error(
            (await res.text()) || `Sign out failed: ${res.status}`,
          );
        }
        setNotice(
          "Signed out. The connection is kept — Connect signs back in.",
        );
        refresh();
      })
      .catch((err: unknown) => setError(errMessage(err)))
      .finally(() => setBusy(false));
  };

  // Remove is confirmed inline on the button itself (InlineConfirmButton) —
  // no window.confirm.
  const disconnect = (id: string) => {
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

  // Directory filtering. A live search spans EVERY category — it overrides
  // the active pill (without clearing it, so emptying the search restores the
  // filter). Pills themselves are true filters.
  const thirdParty = catalog?.third_party ?? [];
  const trimmedQuery = catalogQuery.trim();
  const effectiveCategory = trimmedQuery ? "" : catalogCategory;
  const dirHits = filterCatalog(thirdParty, catalogQuery, effectiveCategory);
  const dirFiltering = trimmedQuery !== "" || catalogCategory !== "";
  const dirCategories = categoriesOf(thirdParty);
  // The Featured shelf: curated household-name picks, rendered before the
  // category listing on the unfiltered view only (searching or picking a
  // category means the user already knows what they want).
  const dirFeatured = thirdParty.filter((e) => e.featured);
  const activeCategoryLabel =
    catalogCategory === FEATURED_SLUG
      ? "Featured"
      : (dirCategories.find((c) => c.slug === catalogCategory)?.label ??
        catalogCategory);

  // One card, one prop wiring — shared by the Featured shelf and the category
  // groups. Added-state matches by URL for one-click entries and by
  // registration name for tenant entries (their added URL has the user's
  // placeholder values filled in).
  const renderDirCard = (tp: CatalogThirdParty) => (
    <DirectoryCard
      key={tp.name}
      entry={tp}
      added={(servers ?? []).some(
        (s) => s.url === tp.url || s.name === tp.name,
      )}
      busy={busy}
      remoteEnabled={catalog?.remote_mcp_enabled ?? false}
      redirectUri={catalog?.oauth_redirect_uri}
      onAdd={(overrides) => requestAddFromCatalog(tp, overrides)}
      autoOpenForm={spotlight === tp.name}
    />
  );

  // Once the directory has loaded, bring the deep-linked card into view. The
  // spotlight card is usually the only hit (the search box was seeded with its
  // name), but scroll anyway: the page has three groups above the directory.
  useEffect(() => {
    if (!spotlight || !catalog) return;
    document
      .querySelector(`[data-testid="dir-card-${CSS.escape(spotlight)}"]`)
      ?.scrollIntoView({ behavior: "auto", block: "center" });
  }, [spotlight, catalog]);

  const scrollDirectoryResults = () => {
    const el = directoryResultsRef.current;
    if (!el) return;
    // The sticky bar overlays the top of the scrollport, so a bare
    // scrollIntoView would park the results count, category heading, and
    // first card row underneath it. Its height varies with how the chips wrap
    // at each viewport width, so measure it at click time (plus breathing
    // room) rather than hardcoding a scroll margin.
    const barHeight = dirBarRef.current?.getBoundingClientRect().height ?? 0;
    el.style.scrollMarginTop = `${Math.ceil(barHeight) + 16}px`;
    const reduce =
      typeof window.matchMedia === "function" &&
      window.matchMedia("(prefers-reduced-motion: reduce)").matches;
    el.scrollIntoView({ behavior: reduce ? "auto" : "smooth", block: "start" });
  };

  const pickCategory = (slug: string) => {
    setCatalogCategory((cur) => (cur === slug ? "" : slug));
    scrollDirectoryResults();
  };

  return (
    <SetSection
      title="Connections"
      intro="Connect remote (hosted) MCP servers and sign in to each with your own account. Connected servers’ tools become available to you in chat and your scheduled tasks. Credentials are stored encrypted on the server and never shared with other users."
    >
      {/* ── Group 1 — Your connections ── */}
      <ConnGroup>
        <ConnGroupHead title="Your connections">
          Everything already available to you — the connectors bundled with your
          workspace, plus any remote servers you’ve added yourself.
        </ConnGroupHead>

        {catalog && catalog.bundled.length > 0 ? (
          <ConnPanel>
            <ConnPanelHead title="Bundled by your workspace" />
            <ConnPanelSub>
              Reviewed and shipped by your operator. These run inside the
              sandbox on this deployment with credentials held server-side —
              nothing leaves the box except the connector’s own API calls.
              Turning one off here hides it from your chats; pick a default
              credential account and each conversation can still narrow the set
              in the Tools picker. Scheduled tasks pin their own selection and
              are unaffected.
            </ConnPanelSub>
            <div className="grid grid-cols-2 gap-[0.7rem] max-[860px]:grid-cols-1">
              {catalog.bundled.map((b) => {
                if (b.optional === false) {
                  // Always-on: operator-wired into every turn; shown locked so
                  // nothing is invisibly enabled.
                  return (
                    <div
                      key={b.name}
                      className="flex flex-col gap-[0.55rem] rounded-[var(--radius-md)] border border-[var(--color-border)] bg-[var(--color-overlay-soft)] px-[0.9rem] py-[0.8rem]"
                    >
                      <div className="flex flex-wrap items-center gap-[0.55rem]">
                        <span className="text-[0.85rem] font-semibold text-[var(--color-text-primary)] [overflow-wrap:anywhere]">
                          {b.display_name || b.name}
                        </span>
                        <ConnBadge>Always on</ConnBadge>
                      </div>
                      {/* .conn-desc.muted — the important flag is needed
                          because ClampText's own text color would otherwise
                          win the same-property utility ordering. */}
                      <ClampText
                        text={
                          b.description ||
                          "Enabled by your operator in every conversation."
                        }
                        className="text-[var(--color-text-muted)]!"
                      />
                      <DataSourceChips sources={b.data_sources} />
                    </div>
                  );
                }
                const pref = prefFor(prefs, "bundled", b.name);
                const on = effectiveEnabled(prefs, "bundled", b.name);
                const autoOn = effectiveAutoEnable(
                  prefs,
                  b.name,
                  b.enabled_by_default,
                );
                return (
                  <BundledCard
                    key={b.name}
                    entry={b}
                    pref={pref}
                    on={on}
                    autoOn={autoOn}
                    busy={busy}
                    onToggle={() =>
                      setConnectorPref({
                        kind: "bundled",
                        connector_id: b.name,
                        enabled: !on,
                        // First explicit row inherits the current effective
                        // seeding state so flipping availability alone never
                        // changes new-chat behavior; disabling clears both.
                        auto_enable: on ? false : autoOn,
                        default_account: on ? "" : pref?.default_account,
                      })
                    }
                    onToggleAuto={() =>
                      setConnectorPref({
                        kind: "bundled",
                        connector_id: b.name,
                        enabled: true,
                        auto_enable: !autoOn,
                        default_account: pref?.default_account,
                      })
                    }
                    onPickAccount={(account) =>
                      setConnectorPref({
                        kind: "bundled",
                        connector_id: b.name,
                        enabled: true,
                        auto_enable: autoOn,
                        default_account: account,
                      })
                    }
                    onReset={() => resetConnectorPref("bundled", b.name)}
                  />
                );
              })}
            </div>
          </ConnPanel>
        ) : null}

        <ConnPanel>
          <ConnPanelHead title="Remote servers">
            <RevealButton
              open={addServerOpen}
              closedLabel="Add remote server"
              onClick={() => setAddServerOpen((o) => !o)}
            />
          </ConnPanelHead>
          <ConnPanelSub>
            Hosted MCP endpoints you’ve connected yourself — from the directory
            below, or by URL.
          </ConnPanelSub>
          {addServerOpen ? (
            <form onSubmit={addServer}>
              <ConnForm>
                <ConnField label="Name">
                  <input
                    className={SETTINGS_INPUT}
                    value={name}
                    onChange={(e) => setName(e.target.value)}
                    placeholder="my-server"
                    required
                  />
                </ConnField>
                <ConnField label="Server URL" grow>
                  <input
                    className={SETTINGS_INPUT}
                    value={url}
                    onChange={(e) => setUrl(e.target.value)}
                    placeholder="https://mcp.example.com/mcp"
                    type="url"
                    required
                  />
                </ConnField>
                <ConnField label="Account label (optional)">
                  <input
                    className={SETTINGS_INPUT}
                    value={accountLabel}
                    onChange={(e) => setAccountLabel(e.target.value)}
                    placeholder="work"
                    aria-label="Account label (optional)"
                  />
                </ConnField>
                <button
                  type="submit"
                  disabled={busy || !name.trim() || !url.trim()}
                  className={btnClass({ variant: "primary" })}
                >
                  Add
                </button>
              </ConnForm>
            </form>
          ) : null}
          {loading ? (
            <p className="mt-[0.2rem] py-[1.05rem] text-center text-[0.79rem] text-[var(--color-text-muted)]">
              Loading…
            </p>
          ) : servers && servers.length > 0 ? (
            <div className="grid">
              {/* One visual group per connection name; one row per seat
                  (login) inside it (#988). A single-seat group looks like the
                  old flat list plus its account badge. */}
              {groupSeats(servers).map((group) => {
                const multi = group.seats.length > 1;
                const template = group.seats[0];
                const groupAuth = template?.auth_kind || "oauth";
                return (
                  <div
                    key={group.name}
                    data-testid={`remote-group-${group.name}`}
                    className="border-b border-[var(--color-border-subtle)] last:border-b-0"
                  >
                    <ConnRows>
                    {group.seats.map((s) => {
                      const enabledForMe = effectiveEnabled(prefs, "remote", s.id);
                      const shareCount = shares[s.id]?.length ?? 0;
                      return (
                        <ConnRow
                          key={s.id}
                          name={
                            <span className="inline-flex flex-wrap items-center gap-[0.55rem]">
                              {s.name}
                              <ConnBadge title="Account label for this login">
                                {seatLabel(s)}
                              </ConnBadge>
                              {s.is_default ? (
                                <ConnBadge
                                  variant="overridden"
                                  title="Chats and tasks use this login unless a conversation or task picks another."
                                >
                                  Default
                                </ConnBadge>
                              ) : null}
                              <ConnBadge variant={statusVariant(s.status)}>
                                {STATUS_LABEL[s.status] ?? s.status}
                              </ConnBadge>
                            </span>
                          }
                          sub={s.url}
                          actions={
                            <>
                              <span className="mr-[0.25rem]">
                                <ToggleForMe
                                  on={enabledForMe}
                                  onLabel="On for me"
                                  offLabel="Off for me"
                                  ariaLabel={`Enable ${s.name} (${seatLabel(s)}) for me`}
                                  onToggle={() =>
                                    setConnectorPref({
                                      kind: "remote",
                                      connector_id: s.id,
                                      enabled: !enabledForMe,
                                    })
                                  }
                                  disabled={busy}
                                  title="Off hides this connection from your own chats and tasks; people you share with are unaffected."
                                />
                              </span>
                              {s.auth_kind === "api_key" ? (
                                <button
                                  type="button"
                                  aria-expanded={keyOpenFor === s.id}
                                  onClick={() => {
                                    setKeyValue("");
                                    setShareOpenFor(null);
                                    setRenameOpenFor(null);
                                    setKeyOpenFor((cur) =>
                                      cur === s.id ? null : s.id,
                                    );
                                  }}
                                  disabled={busy}
                                  className={btnClass({ sm: true, reveal: true })}
                                >
                                  Update key
                                </button>
                              ) : s.auth_kind === "open" ? null : (
                                <button
                                  type="button"
                                  onClick={() => connect(s.id)}
                                  disabled={busy}
                                  className={btnClass({ sm: true, reveal: true })}
                                >
                                  {s.status === "connected" ? "Reconnect" : "Connect"}
                                </button>
                              )}
                              {s.auth_kind !== "api_key" &&
                              s.auth_kind !== "open" &&
                              s.status === "connected" ? (
                                <button
                                  type="button"
                                  onClick={() => signOut(s.id)}
                                  disabled={busy}
                                  title="Ends the authorization but keeps the connection and its OAuth client — Connect signs back in without re-entering credentials."
                                  className={btnClass({ sm: true, reveal: true })}
                                >
                                  Sign out
                                </button>
                              ) : null}
                              {multi && !s.is_default ? (
                                <button
                                  type="button"
                                  onClick={() => setDefaultSeat(s.id)}
                                  disabled={busy}
                                  title="Chats and tasks mount this login unless a conversation or task picks another."
                                  className={btnClass({ sm: true, reveal: true })}
                                >
                                  Set default
                                </button>
                              ) : null}
                              <button
                                type="button"
                                aria-expanded={renameOpenFor === s.id}
                                onClick={() => {
                                  setRenameValue(s.account ?? "");
                                  setKeyOpenFor(null);
                                  setShareOpenFor(null);
                                  setRenameOpenFor((cur) =>
                                    cur === s.id ? null : s.id,
                                  );
                                }}
                                disabled={busy}
                                className={btnClass({ sm: true, reveal: true })}
                              >
                                Rename
                              </button>
                              <button
                                type="button"
                                aria-expanded={shareOpenFor === s.id}
                                onClick={() => {
                                  setShareGrantee("");
                                  setKeyOpenFor(null);
                                  setRenameOpenFor(null);
                                  setShareOpenFor((cur) =>
                                    cur === s.id ? null : s.id,
                                  );
                                }}
                                disabled={busy}
                                className={btnClass({ sm: true, reveal: true })}
                              >
                                Share{shareCount > 0 ? ` (${shareCount})` : ""}
                              </button>
                              <InlineConfirmButton
                                label="Remove"
                                confirmLabel="Confirm remove"
                                onConfirm={() => disconnect(s.id)}
                                disabled={busy}
                              />
                            </>
                          }
                          detail={
                            renameOpenFor === s.id ? (
                              <div className="mt-2 flex flex-wrap items-center gap-2 rounded-[var(--radius-md)] border border-[var(--color-border-subtle)] bg-[var(--color-overlay-soft)] px-3 py-2.5">
                                <input
                                  value={renameValue}
                                  onChange={(e) => setRenameValue(e.target.value)}
                                  onKeyDown={(e) => {
                                    if (e.key === "Enter") {
                                      e.preventDefault();
                                      renameSeat(s.id);
                                    }
                                  }}
                                  placeholder="account label — e.g. work"
                                  aria-label={`New account label for ${s.name} (${seatLabel(s)})`}
                                  className="min-w-0 flex-1 rounded-[var(--radius-md)] border border-[var(--color-border)] bg-[var(--color-surface-1)] px-3 py-1.5 text-[0.8125rem] text-[var(--color-text-primary)] outline-none placeholder:text-[var(--color-text-muted)] focus-visible:border-[var(--color-border-strong)] focus-visible:shadow-[var(--focus-ring)]"
                                />
                                <button
                                  type="button"
                                  onClick={() => renameSeat(s.id)}
                                  disabled={
                              busy || renameValue.trim() === (s.account ?? "")
                            }
                                  className={btnClass({ sm: true, reveal: true })}
                                >
                                  Save label
                                </button>
                              </div>
                            ) : keyOpenFor === s.id ? (
                              <div className="mt-2 flex flex-wrap items-center gap-2 rounded-[var(--radius-md)] border border-[var(--color-border-subtle)] bg-[var(--color-overlay-soft)] px-3 py-2.5">
                                <input
                                  value={keyValue}
                                  onChange={(e) => setKeyValue(e.target.value)}
                                  onKeyDown={(e) => {
                                    if (e.key === "Enter" && keyValue.trim()) {
                                      e.preventDefault();
                                      updateKey(s.id);
                                    }
                                  }}
                                  placeholder="paste the new API key (never shown again)"
                                  type="password"
                                  autoComplete="off"
                                  className="min-w-0 flex-1 rounded-[var(--radius-md)] border border-[var(--color-border)] bg-[var(--color-surface-1)] px-3 py-1.5 text-[0.8125rem] text-[var(--color-text-primary)] outline-none placeholder:text-[var(--color-text-muted)] focus-visible:border-[var(--color-border-strong)] focus-visible:shadow-[var(--focus-ring)]"
                                />
                                <button
                                  type="button"
                                  onClick={() => updateKey(s.id)}
                                  disabled={busy || !keyValue.trim()}
                                  className={btnClass({ sm: true, reveal: true })}
                                >
                                  Save key
                                </button>
                              </div>
                            ) : shareOpenFor === s.id ? (
                              <div className="mt-2 rounded-[var(--radius-md)] border border-[var(--color-border-subtle)] bg-[var(--color-overlay-soft)] px-3 py-2.5">
                                <p className="mb-2 mt-0 text-[0.75rem] leading-[1.5] text-[var(--color-text-muted)]">
                                  People you share with can use this connection in
                                  their chats and scheduled tasks. Their tool calls
                                  run under{" "}
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
                                        className="flex items-center gap-1.5 rounded-[var(--radius-pill)] border border-[var(--color-border-strong)] px-2.5 py-0.5 text-[0.75rem] text-[var(--color-text-secondary)]"
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
                                    className="min-w-0 flex-1 rounded-[var(--radius-md)] border border-[var(--color-border)] bg-[var(--color-surface-1)] px-3 py-1.5 text-[0.8125rem] text-[var(--color-text-primary)] outline-none placeholder:text-[var(--color-text-muted)] focus-visible:border-[var(--color-border-strong)] focus-visible:shadow-[var(--focus-ring)]"
                                  />
                                  <button
                                    type="button"
                                    onClick={() => share(s.id, shareGrantee)}
                                    disabled={busy || !shareGrantee.trim()}
                                    className={btnClass({ sm: true, reveal: true })}
                                  >
                                    Share
                                  </button>
                                  {(shares[s.id] ?? []).includes("*") ? null : (
                                    // Opens the connection to every account in
                                    // the workspace at once — armed first, like
                                    // Remove, rather than granted on one click.
                                    <InlineConfirmButton
                                      label="Share with everyone"
                                      confirmLabel="Confirm share with everyone"
                                      disabled={busy}
                                      onConfirm={() => share(s.id, "*")}
                                      testId={`share-everyone-${s.id}`}
                                    />
                                  )}
                                </div>
                              </div>
                            ) : null
                          }
                        >
                          {s.status_detail ? (
                            <span className="text-[0.72rem] text-[var(--color-warning)] [overflow-wrap:anywhere]">
                              {s.status_detail}
                            </span>
                          ) : null}
                        </ConnRow>
                      );
                    })}
                    </ConnRows>
                    <div className="flex flex-wrap items-center gap-x-3 gap-y-1 py-[0.45rem]">
                      <button
                        type="button"
                        data-testid={`add-seat-${group.name}`}
                        aria-expanded={addSeatFor === group.name}
                        onClick={() => {
                          setAddSeatLabel("");
                          setAddSeatKey("");
                          setAddSeatFor((cur) =>
                            cur === group.name ? null : group.name,
                          );
                        }}
                        disabled={busy}
                        className={btnClass({ sm: true })}
                      >
                        {addSeatFor === group.name
                          ? "Cancel"
                          : "Add another account"}
                      </button>
                      {multi ? (
                        <span className="text-[0.72rem] text-[var(--color-text-muted)]">
                          Chats use the default login unless a conversation or
                          task picks another.
                        </span>
                      ) : null}
                    </div>
                    {addSeatFor === group.name ? (
                      <div
                        data-testid={`add-seat-form-${group.name}`}
                        className="mb-[0.55rem] flex flex-wrap items-center gap-2 rounded-[var(--radius-md)] border border-[var(--color-border-subtle)] bg-[var(--color-overlay-soft)] px-3 py-2.5"
                      >
                        <input
                          value={addSeatLabel}
                          onChange={(e) => setAddSeatLabel(e.target.value)}
                          onKeyDown={(e) => {
                            if (
                              e.key === "Enter" &&
                              addSeatLabel.trim() &&
                              (groupAuth !== "api_key" || addSeatKey.trim())
                            ) {
                              e.preventDefault();
                              addSeat(group);
                            }
                          }}
                          placeholder="account label — e.g. work"
                          aria-label={`Account label for the new ${group.name} login`}
                          className="min-w-0 flex-1 basis-[10rem] rounded-[var(--radius-md)] border border-[var(--color-border)] bg-[var(--color-surface-1)] px-3 py-1.5 text-[0.8125rem] text-[var(--color-text-primary)] outline-none placeholder:text-[var(--color-text-muted)] focus-visible:border-[var(--color-border-strong)] focus-visible:shadow-[var(--focus-ring)]"
                        />
                        {groupAuth === "api_key" ? (
                          <input
                            value={addSeatKey}
                            onChange={(e) => setAddSeatKey(e.target.value)}
                            placeholder="API key for this account (never shown again)"
                            aria-label={`API key for the new ${group.name} login`}
                            type="password"
                            autoComplete="off"
                            className="min-w-0 flex-1 basis-[14rem] rounded-[var(--radius-md)] border border-[var(--color-border)] bg-[var(--color-surface-1)] px-3 py-1.5 text-[0.8125rem] text-[var(--color-text-primary)] outline-none placeholder:text-[var(--color-text-muted)] focus-visible:border-[var(--color-border-strong)] focus-visible:shadow-[var(--focus-ring)]"
                          />
                        ) : null}
                        <button
                          type="button"
                          data-testid={`add-seat-submit-${group.name}`}
                          onClick={() => addSeat(group)}
                          disabled={
                            busy ||
                            !addSeatLabel.trim() ||
                            (groupAuth === "api_key" && !addSeatKey.trim())
                          }
                          className={btnClass({ sm: true, reveal: true })}
                        >
                          {groupAuth === "oauth" ? "Add and sign in" : "Add"}
                        </button>
                        <span className="basis-full text-[0.7rem] text-[var(--color-text-muted)]">
                          A label is required for a second login — it tells the
                          seats apart in the Tools picker.
                        </span>
                      </div>
                    ) : null}
                  </div>
                );
              })}
            </div>
          ) : (
            <ConnEmpty>
              No remote servers yet — pick one from the directory below, or add
              one by URL.
            </ConnEmpty>
          )}
        </ConnPanel>

        {sharedWithMe.length > 0 ? (
          <ConnPanel>
            <ConnPanelHead title="Shared with you" />
            <ConnPanelSub>
              Connections other users shared with you. Their tools are available
              in your chats and scheduled tasks; calls run under the
              owner&apos;s login, brokered server-side.
            </ConnPanelSub>
            <ConnRows>
              {sharedWithMe.map((s) => {
                const on = effectiveEnabled(prefs, "remote", s.id);
                return (
                  <ConnRow
                    key={s.id}
                    name={
                      <span className="inline-flex flex-wrap items-center gap-[0.55rem]">
                        {s.name}
                        <ConnBadge title="The owner's account label for this login">
                          {seatLabel(s)}
                        </ConnBadge>
                        <ConnBadge variant={statusVariant(s.status)}>
                          {STATUS_LABEL[s.status] ?? s.status}
                        </ConnBadge>
                      </span>
                    }
                    sub={s.url}
                    actions={
                      <>
                        <span className="text-[0.72rem] text-[var(--color-text-muted)]">
                          shared by {s.owner}
                        </span>
                        <span className="ml-[0.45rem]">
                          <ToggleForMe
                            on={on}
                            onLabel="On for me"
                            offLabel="Off for me"
                            ariaLabel={`Enable ${s.name} (${seatLabel(s)}) for me`}
                            onToggle={() =>
                              setConnectorPref({
                                kind: "remote",
                                connector_id: s.id,
                                enabled: !on,
                              })
                            }
                            disabled={busy}
                            title="Off hides this shared connection from your chats and tasks — only for you; the owner and other users are unaffected."
                          />
                        </span>
                      </>
                    }
                  />
                );
              })}
            </ConnRows>
          </ConnPanel>
        ) : null}
      </ConnGroup>

      {/* ── Group 2 — Credential accounts ── */}
      <ConnGroup>
        <ConnGroupHead title="Credential accounts">
          Named sign-ins your connectors use — e.g. one per client seat. Secret
          values are write-only; they are never displayed after saving.
        </ConnGroupHead>
        <CredentialAccountAdmin
          servers={mcpServers}
          onChanged={() => {
            void reloadMcpServers();
            // Bundled cards offer accounts as default seats — refresh them too.
            refreshCatalog();
          }}
        />
      </ConnGroup>

      {/* ── Group 3 — Connector directory (collapsible) ── */}
      {catalog && catalog.third_party.length > 0 ? (
        <ConnGroup>
          {/* ConnGroupHead's metrics, recomposed so the collapse toggle sits on
              the title row. */}
          <div className="mb-[0.8rem]">
            <div className="flex items-center justify-between gap-4">
              <h3 className="m-0 text-[0.98rem] font-semibold text-[var(--color-text-primary)]">
                Connector directory
              </h3>
              <button
                type="button"
                aria-expanded={catalogOpen}
                onClick={() => setCatalogOpen((v) => !v)}
                className={btnClass({ sm: true })}
              >
                {catalogOpen ? "Hide" : `Browse ${catalog.third_party.length}`}
              </button>
            </div>
            <p className="mb-0 mt-[0.35rem] text-[0.78rem] leading-[1.55] text-[var(--color-text-muted)] [&_b]:font-semibold [&_b]:text-[var(--color-text-secondary)]">
              Hosted servers run by the named operator, not by your workspace.
              Connecting one signs you in with that operator and sends tool
              calls — which can include parts of your conversation — to their
              service under their terms. <b>Official</b> = the service’s own
              vendor runs the endpoint; <b>Aggregator</b> and <b>Community</b>{" "}
              endpoints are run by someone else — vet them via the linked docs
              and source before connecting.
            </p>
          </div>

          {catalogOpen ? (
            <>
              <div
                ref={dirBarRef}
                data-testid="dir-filter-bar"
                className="sticky top-0 z-20 mb-[1.1rem] grid gap-[0.55rem] border-b border-[var(--color-border)] bg-[var(--color-bg)] pb-[0.75rem] pt-[0.7rem] [background-attachment:fixed] [background-image:var(--gradient-bg)] [background-size:cover]"
              >
                <DirSearch
                  value={catalogQuery}
                  onChange={setCatalogQuery}
                  placeholder="Search servers — try “postgres”, “crm”, “scraping”…"
                  label="Search connector directory"
                />
                <div
                  className="flex flex-wrap gap-[0.35rem]"
                  role="tablist"
                  aria-label="Filter by category"
                >
                  <DirChip
                    active={catalogCategory === ""}
                    onClick={() => {
                      setCatalogCategory("");
                      scrollDirectoryResults();
                    }}
                    count={catalog.third_party.length}
                    role="tab"
                    ariaSelected={catalogCategory === ""}
                  >
                    All
                  </DirChip>
                  {dirFeatured.length > 0 ? (
                    <DirChip
                      active={catalogCategory === FEATURED_SLUG}
                      onClick={() => pickCategory(FEATURED_SLUG)}
                      count={dirFeatured.length}
                      role="tab"
                      ariaSelected={catalogCategory === FEATURED_SLUG}
                    >
                      ✦ Featured
                    </DirChip>
                  ) : null}
                  {dirCategories.map((c) => (
                    <DirChip
                      key={c.slug}
                      active={catalogCategory === c.slug}
                      onClick={() => pickCategory(c.slug)}
                      count={c.count}
                      role="tab"
                      ariaSelected={catalogCategory === c.slug}
                      leading={
                        <Icon
                          name={categoryIcon(c.slug)}
                          className="mr-[0.32rem] size-3 shrink-0 self-center"
                        />
                      }
                    >
                      {c.label}
                    </DirChip>
                  ))}
                </div>
              </div>

              <div ref={directoryResultsRef}>
                {dirFiltering ? (
                  <p className="m-0 text-[0.72rem] text-[var(--color-text-muted)]">
                    {dirHits.length === 1
                      ? "1 server matches"
                      : `${dirHits.length} servers match`}
                  </p>
                ) : null}
                {!dirFiltering && dirFeatured.length > 0 ? (
                  <div data-testid="dir-featured">
                    <DirCatHead>✦ Featured</DirCatHead>
                    <div className="grid grid-cols-2 gap-[0.7rem] max-[860px]:grid-cols-1">
                      {dirFeatured.map(renderDirCard)}
                    </div>
                  </div>
                ) : null}
                {dirHits.length === 0 ? (
                  <ConnEmpty>
                    No servers match “{trimmedQuery}”
                    {!trimmedQuery && catalogCategory
                      ? ` in ${activeCategoryLabel}`
                      : ""}
                    .
                  </ConnEmpty>
                ) : (
                  groupByCategory(dirHits).map((group) => (
                    <div key={group.slug}>
                      <DirCatHead>
                        <span className="inline-flex items-center gap-[0.35rem]">
                          <Icon
                            name={categoryIcon(group.slug)}
                            className="size-3 shrink-0"
                          />
                          {group.label}
                        </span>
                      </DirCatHead>
                      <div className="grid grid-cols-2 gap-[0.7rem] max-[860px]:grid-cols-1">
                        {group.entries.map(renderDirCard)}
                      </div>
                    </div>
                  ))
                )}
                {!catalog.remote_mcp_enabled ? (
                  <p className="mt-2 text-[0.72rem] text-[var(--color-text-muted)]">
                    {adminState === "admin" ? (
                      <>
                        Connecting hosted servers is off because remote MCP
                        isn’t configured. To enable it, set{" "}
                        <CodeChip>FLEET_MCP_OAUTH_ENCRYPTION_KEY</CodeChip> (a
                        32-byte base64 key) and{" "}
                        <CodeChip>FLEET_PUBLIC_BASE_URL</CodeChip> on the
                        server, then restart — see docs/MCP-CATALOG.md in the
                        fleet repository for the walkthrough.
                      </>
                    ) : (
                      <>
                        Connecting hosted servers isn’t enabled on this
                        workspace yet — ask your administrator to turn on remote
                        MCP connections.
                      </>
                    )}
                  </p>
                ) : null}
              </div>
            </>
          ) : null}
        </ConnGroup>
      ) : null}

      {/* Consent step for endpoints not operated by the service's own vendor.
          A badge alone gets scrolled past; connecting sends conversation-
          derived tool traffic to (and often parks a delegated access token
          with) the named operator, so the add is gated on an explicit,
          operator-named confirmation. */}
      {/* Post-add sign-in prompt: an OAuth server was just added and needs a
          login to finish connecting. Offer it here so the user doesn't have
          to scroll up to the "Your connections" row to click Connect. */}
      {connectPromptFor ? (
        <DialogShell
          label={`Sign in to ${connectPromptFor.name}?`}
          scrimLabel={`Dismiss the ${connectPromptFor.name} sign-in prompt`}
          onDismiss={() => setConnectPromptFor(null)}
          className="max-w-md p-5"
        >
          <h3 className="mb-2 text-[0.9375rem] font-semibold">
            {connectPromptFor.name} added — sign in now?
          </h3>
          <p className="mb-4 text-[0.8125rem] text-[var(--color-text-secondary)]">
            One step left: sign in so its tools become available to you. You can
            also do it later from the Connect button under Your connections.
          </p>
          <div className="flex justify-end gap-2">
            <button
              type="button"
              onClick={() => setConnectPromptFor(null)}
              className={btnClass({ reveal: true })}
            >
              Later
            </button>
            <button
              type="button"
              onClick={() => {
                const id = connectPromptFor.id;
                setConnectPromptFor(null);
                connect(id);
              }}
              disabled={busy}
              className={btnClass({ variant: "primary" })}
            >
              Sign in
            </button>
          </div>
        </DialogShell>
      ) : null}
      {consentFor ? (
        <DialogShell
          label={`Connect ${consentFor.entry.display_name}?`}
          scrimLabel={`Dismiss the ${consentFor.entry.display_name} connect dialog`}
          onDismiss={() => setConsentFor(null)}
          className="max-w-md p-5"
        >
          <div className="mb-2 flex items-center gap-2">
            <h3 className="text-[0.9375rem] font-semibold">
              Connect {consentFor.entry.display_name}?
            </h3>
            <ConnBadge
              variant={provenanceVariant(consentFor.entry.provenance)}
            >
              {provenanceBadge(consentFor.entry.provenance).label}
            </ConnBadge>
          </div>
          <p className="mb-3 text-[0.8125rem] text-[var(--color-text-secondary)]">
            This endpoint is operated by{" "}
            <strong className="text-[var(--color-text-primary)]">
              {consentFor.entry.vendor || "an unnamed operator"}
            </strong>
            {provenanceBadge(consentFor.entry.provenance).label ===
            "Aggregator"
              ? " — a platform that hosts access to other vendors' services, not the services themselves."
              : " — not the vendor of the underlying service, and not your workspace."}{" "}
            Once connected, it receives your tool calls (which can include
            parts of your conversations)
            {consentFor.entry.auth === "oauth"
              ? " and holds the access token you grant during sign-in"
              : ""}
            {consentFor.overrides?.apiKey
              ? " and holds the API key you provide"
              : ""}
            .
          </p>
          <p className="mb-4 text-[0.75rem] text-[var(--color-text-muted)]">
            Vet it first:{" "}
            {consentFor.entry.docs_url ? (
              <a
                href={consentFor.entry.docs_url}
                target="_blank"
                rel="noreferrer"
                className="underline underline-offset-2"
              >
                documentation
              </a>
            ) : null}
            {consentFor.entry.docs_url && consentFor.entry.repo_url
              ? " · "
              : null}
            {consentFor.entry.repo_url ? (
              <a
                href={consentFor.entry.repo_url}
                target="_blank"
                rel="noreferrer"
                className="underline underline-offset-2"
              >
                source code
              </a>
            ) : null}
            {!consentFor.entry.docs_url && !consentFor.entry.repo_url
              ? "no docs or source were provided for this entry."
              : null}
          </p>
          <div className="flex justify-end gap-2">
            <button
              type="button"
              onClick={() => setConsentFor(null)}
              className={btnClass({ reveal: true })}
            >
              Cancel
            </button>
            <button
              type="button"
              onClick={() =>
                void addFromCatalog(consentFor.entry, consentFor.overrides)
              }
              disabled={busy}
              className={btnClass({ variant: "primary" })}
            >
              I trust this operator — add
            </button>
          </div>
        </DialogShell>
      ) : null}
    </SetSection>
  );
}
