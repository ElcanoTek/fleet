import { useCallback, useEffect, useRef, useState } from "react";
import { createPortal } from "react-dom";

import { Icon } from "@/app/shared/ui/Icon";
import { navigate } from "@/app/lib/navigation";
import {
  authHint,
  categoryIcon,
  connectorBuilderMessage,
  connectorParamOf,
  consentRequired,
  customTransportBadge,
  effectiveAutoEnable,
  effectiveEnabled,
  fillPlaceholders,
  filterCatalog,
  placeholderLabel,
  placeholdersOf,
  placeholderValueOK,
  prefFor,
  provenanceBadge,
  setupLink,
  toolCountSuffix,
  keyVerifiedSuffix,
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
import { useClientConfig } from "@/app/lib/useClientConfig";
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
  DirSearch,
  SetSection,
} from "../ui/panels";
import { useMcpServers } from "@/app/shared/hooks/useMcpServers";
import { ToastProvider, useToast } from "@/app/shared/ui/Toast";
import { DialogShell } from "@/app/shared/ui/DialogShell";
import { CloseButton } from "@/app/shared/ui/CloseButton";
import { responseErrorText } from "@/app/lib/responseError";
import { copyText } from "@/app/shared/lib/clipboard";
import { errorMessage } from "@/app/shared/lib/errorMessage";
import { getGatedJson } from "@/app/settings/lib/getGated";

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
  // The scheme fleet sends in front of an api_key under its header ("Token
  // token="), from the directory entry at add time; the key forms show it.
  api_key_prefix?: string;
  // Multi-login (#988): one row per seat (login) under a connection name.
  // `account` is the seat's label — "" is the unlabeled seat every
  // pre-existing connection is, rendered "primary". `is_default` marks the
  // seat chats and tasks mount when nothing picks another.
  account: string;
  is_default: boolean;
  created_at: number;
  updated_at: number;
  // What the shipped directory entry of this name now says differently from
  // the row (F13): the row keeps the URL and auth it was added with, so a
  // corrected entry is surfaced here and taken by removing and re-adding the
  // connection. Absent when the row matches its entry, or no directory entry
  // has its name — the join is the name, not where the row came from.
  catalog_drift?: { url?: string; auth?: string; key_sent_as?: string };
  // Custom HTTP API or Playwright browser tools synthesized by connector_studio.
  custom_tools?: Array<{ name: string; description?: string }>;
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

// The searchable MCP directory. Bundled entries are the operator's
// sandboxed connectors, surfaced under "Your connections" with the per-user
// availability toggle; third-party entries are hosted servers the user can
// add to the connect flow. Types + the search/provenance helpers
// live in ./catalog (unit-tested there).

const BUNDLED_PREVIEW_COUNT = 4;
const DIRECTORY_PREVIEW_COUNT = 8;
// Editorial picks, in display order; these are not usage rankings. Keep the
// catalog's endpoint, operator and setup instructions behind the short names.
const POPULAR_CONNECTORS = new Map([
  ["microsoft-workiq-mail", "Microsoft 365 (Outlook)"],
  ["github", "GitHub"],
  ["slack", "Slack"],
  ["google-gmail", "Gmail"],
  ["google-drive", "Google Drive"],
  ["notion", "Notion"],
  ["linear", "Linear"],
  ["salesforce", "Salesforce"],
]);

const STATUS_LABEL: Record<string, string> = {
  login_required: "Sign-in needed",
  connected: "Connected",
  needs_reauth: "Sign-in needed",
  error: "Error",
};

const STATUS_VARIANTS: Record<string, BadgeVariant> = {
  connected: "success",
  login_required: "warn",
  needs_reauth: "warn",
  error: "warn",
};

// The badge text for a seat. A rejected API key and an ended OAuth sign-in
// share needs_reauth; the key case asks for a new key, matching its Update key
// button.
function statusLabel(status: string, authKind?: string): string {
  if (status === "needs_reauth" && authKind === "api_key") return "New key needed";
  return STATUS_LABEL[status] ?? status;
}

function statusVariant(status: string): BadgeVariant {
  return STATUS_VARIANTS[status] ?? "neutral";
}

// A seat whose tools can't run until the user signs in (or pastes a new key).
// Its sign-in action becomes the row's one filled button so it is the first
// thing a reader sees.
function needsSignIn(status: string): boolean {
  return status === "login_required" || status === "needs_reauth" || status === "error";
}

// hostOf shows an endpoint by its host in the card's endpoint select, where a
// full URL would be noise and the host is what distinguishes regions.
function hostOf(url: string): string {
  try {
    return new URL(url).host;
  } catch {
    return url;
  }
}

// keySentAsNote words how an api_key connection sends its key when the entry
// declares a scheme prefix, so the user pastes only the key and can see that
// the "Token token=" part is fleet's job (#986, PagerDuty). Null when the
// key is sent raw or as a bearer, which needs no explanation.
// The directory card knows the entry's header; a saved row does not carry
// its header name, so its Update key form names only the scheme.
function keySentAsNote(header: string | undefined, prefix?: string): string | null {
  if (!prefix) return null;
  return header
    ? `Sent as ${header}: ${prefix}<key> — paste only the key.`
    : `Sent as ${prefix}<key> — paste only the key.`;
}

const AUTH_LABEL: Record<string, string> = {
  oauth: "OAuth",
  api_key: "API key",
  open: "no",
};

// driftSummary words a row's catalog_drift for the row's sub line: what the
// directory entry of this name lists now, and that re-adding is how to take
// it. The join is the name, not provenance — a server added by hand under a
// directory name is compared too — so the sentence says what the directory
// lists, never where the row came from.
function driftSummary(d: NonNullable<RemoteServer["catalog_drift"]>): string {
  const parts: string[] = [];
  if (d.url) parts.push(`the address ${d.url}`);
  if (d.auth) parts.push(`${AUTH_LABEL[d.auth] ?? d.auth} sign-in`);
  if (d.key_sent_as) parts.push(`the key sent in ${d.key_sent_as}`);
  return `The directory entry of this name now lists ${parts.join(", ")}. Remove this connection and add it again from the directory to take the change, or keep it as it is if you set it up by hand.`;
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
  return getGatedJson<ListResponse>("/api/remote-mcp-servers", {
    label: "Connections",
    statusText: {
      503: "Remote MCP OAuth is not configured on this server (set FLEET_MCP_OAUTH_ENCRYPTION_KEY and FLEET_PUBLIC_BASE_URL).",
    },
  });
}

// granteeLabel renders the everyone wildcard readably.
function granteeLabel(g: string): string {
  return g === "*" ? "Everyone on this box" : g;
}

function errMessage(err: unknown): string {
  return errorMessage(err, "Something went wrong.");
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

function readConnectorAnchor(): string | null {
  if (typeof window === "undefined") return null;
  try {
    const hash = decodeURIComponent(window.location.hash.slice(1));
    return hash.startsWith("connector-") ? hash : null;
  } catch {
    return null;
  }
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
// state label ("On for me" / "Off for me" …). The switch is the
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

// The anchor the chat Tools library links to ("Manage in Settings",
// "Set up in Settings"): /settings/connections#connector-<name>.
export const connectorAnchor = (name: string) => `connector-${name}`;

// A bundled card's frame; scroll-margin keeps a deep-linked card clear of the
// page header, and :target rings the card the link landed on.
const BUNDLED_CARD =
  "flex scroll-mt-24 flex-col gap-[0.55rem] rounded-[var(--radius-md)] border border-[var(--color-border)] bg-[var(--color-overlay-soft)] px-[0.9rem] py-[0.8rem] target:border-[var(--color-accent)] target:shadow-[0_0_0_1px_var(--color-accent)]";

// A connector the workspace declares but this deployment has no credentials
// for. Nothing to toggle; the copy says who can fix it. Connector credentials
// are set on the server, not in this app, so the admin copy says so rather
// than offering a form that does not exist.
function NeedsSetupCard({ entry, isAdmin, idPrefix = "" }: { entry: CatalogBundled; isAdmin: boolean; idPrefix?: string }) {
  const label = entry.display_name || entry.name;
  return (
    <div id={`${idPrefix}${connectorAnchor(entry.name)}`} data-testid={`bundled-card-${entry.name}`} className={BUNDLED_CARD}>
      <div className="flex flex-wrap items-center gap-[0.55rem]">
        <span className="text-[0.85rem] font-semibold text-[var(--color-text-primary)] [overflow-wrap:anywhere]">
          {label}
        </span>
        <ConnBadge variant="warn">Needs setup</ConnBadge>
      </div>
      {entry.description ? (
        <ClampText text={entry.description} className="text-[var(--color-text-muted)]!" />
      ) : null}
      <div className="rounded-[var(--radius-md)] border border-[var(--color-panel-warn-border)] bg-[var(--color-panel-warn-bg)] px-[0.75rem] py-[0.5rem] text-[0.75rem] leading-[1.5] text-[var(--color-text-secondary)]">
        {isAdmin
          ? "Needs setup before it can be used. Its credentials are added on the server by whoever deploys this workspace."
          : "An admin needs to set this up."}
      </div>
    </div>
  );
}

function BundledCard({
  entry,
  idPrefix = "",
  pref,
  on,
  autoOn,
  busy,
  appName,
  onToggle,
  onToggleAuto,
  onPickAccount,
  onReset,
}: {
  entry: CatalogBundled;
  idPrefix?: string;
  pref: ConnectorPref | undefined;
  on: boolean;
  autoOn: boolean;
  busy: boolean;
  // The deployment's name: the default seat's label ("Elcano").
  appName: string;
  onToggle: () => void;
  onToggleAuto: () => void;
  onPickAccount: (account: string) => void;
  onReset: () => void;
}) {
  const label = entry.display_name || entry.name;
  return (
    <div id={`${idPrefix}${connectorAnchor(entry.name)}`} data-testid={`bundled-card-${entry.name}`} className={BUNDLED_CARD}>
      <div className="flex flex-wrap items-center gap-[0.55rem]">
        <span className="text-[0.85rem] font-semibold text-[var(--color-text-primary)] [overflow-wrap:anywhere]">
          {label}
        </span>
        {entry.beta ? <ConnBadge>Beta</ConnBadge> : null}
      </div>
      {entry.description ? <ClampText text={entry.description} /> : null}
      <DataSourceChips sources={entry.data_sources} />
      <div className="mt-auto flex flex-wrap items-center gap-[0.8rem]">
        <ToggleForMe
          on={on}
          onLabel="On for me"
          offLabel="Off for me"
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
          <label
            htmlFor={`${idPrefix}bundledAccount-${entry.name}`}
            className="inline-flex items-center gap-[0.45rem] text-[0.73rem] text-[var(--color-text-muted)]"
          >
            Account
            <span className="select-wrap inline-block">
              <select
                id={`${idPrefix}bundledAccount-${entry.name}`}
                className={COMPACT_SELECT}
                value={pref?.default_account ?? ""}
                onChange={(e) => onPickAccount(e.target.value)}
                disabled={busy}
                aria-label={`${entry.name} credential account`}
              >
                <option value="">{appName}</option>
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

// dirAddButtonClass — the Add/Set up button on a directory card. The action
// takes the button radius; once added it becomes an "Added" status and takes
// the pill, matching the badge rule in DESIGN.md.
function dirAddButtonClass(added: boolean): string {
  return [
    "shrink-0 border bg-transparent px-[0.85rem] py-[0.24rem] text-[0.76rem] font-medium transition focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]",
    added
      ? "rounded-[var(--radius-pill)] border-[var(--color-success-border)] text-[var(--color-success)]"
      : "rounded-[var(--radius-md)] border-[var(--color-border-strong)] text-[var(--color-text-primary)] hover:border-[var(--color-accent)] hover:bg-[var(--color-overlay-soft)] disabled:opacity-50",
  ].join(" ");
}

// DirectoryCard — one third-party entry (.dir-card): optional Popular badge,
// clamped description, vendor + vetting + setup-guide links, a VISIBLE setup
// hint, and the gated Add. Entries a user can't one-click add get a guided
// inline form instead of a bare "needs your URL" label: one input per
// {placeholder} in a tenant-scoped URL template (with a live preview of the
// resulting endpoint), and a write-only key field for api_key entries.
function DirectoryCard({
  entry,
  popular,
  added,
  addedURL,
  busy,
  remoteEnabled,
  redirectUri,
  onAdd,
  autoOpenForm,
}: {
  entry: CatalogThirdParty;
  popular: boolean;
  added: boolean;
  // The URL the entry's first seat was added on, when added: a second seat
  // starts on the same endpoint (the row-level "add another" copies it too).
  addedURL?: string;
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
  const variants = entry.url_variants ?? [];
  const needsForm =
    placeholders.length > 0 || entry.auth === "api_key" || manualClient || variants.length > 0;
  const [formOpen, setFormOpen] = useState(
    (autoOpenForm ?? false) && needsForm && !added,
  );
  const [values, setValues] = useState<Record<string, string>>({});
  // Which of the entry's endpoints to add — the default, or one of its
  // url_variants (a regional host). The whole URL, so the add posts it as is.
  // A second seat starts where the first one was added — re-derived each
  // time the "Add another account" form opens, because the saved seats can
  // load after this card mounts and a mount-time initializer would miss them.
  const seatEndpointURL =
    added && addedURL && variants.some((v) => v.url === addedURL)
      ? addedURL
      : entry.url;
  const [endpointURL, setEndpointURL] = useState(seatEndpointURL);
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
      ...(variants.length > 0 && endpointURL !== entry.url ? { url: endpointURL } : {}),
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
        {popular ? <ConnBadge>Popular</ConnBadge> : null}
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
                onClick={() => {
                  if (!formOpen) setEndpointURL(seatEndpointURL);
                  setFormOpen((o) => !o);
                }}
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
            <label
              htmlFor={`dirFormAccount-${entry.name}`}
              className="grid gap-1 text-[0.72rem] text-[var(--color-text-secondary)]"
            >
              <span className="font-medium">Account label</span>
              <input
                id={`dirFormAccount-${entry.name}`}
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
          {variants.length > 0 ? (
            <label
              htmlFor={`dirFormEndpoint-${entry.name}`}
              className="grid gap-1 text-[0.72rem] text-[var(--color-text-secondary)]"
            >
              <span className="font-medium">Endpoint</span>
              <select
                id={`dirFormEndpoint-${entry.name}`}
                className={SETTINGS_INPUT}
                value={endpointURL}
                onChange={(e) => setEndpointURL(e.target.value)}
                data-testid={`dir-form-endpoint-${entry.name}`}
              >
                <option value={entry.url}>Default ({hostOf(entry.url)})</option>
                {variants.map((v) => (
                  <option key={v.id} value={v.url}>
                    {v.label} — {hostOf(v.url)}
                  </option>
                ))}
              </select>
            </label>
          ) : null}
          {placeholders.map((ph) => (
            <label
              key={ph}
              htmlFor={`dirFormField-${entry.name}-${ph}`}
              className="grid gap-1 text-[0.72rem] text-[var(--color-text-secondary)]"
            >
              <span className="font-medium">Your {placeholderLabel(ph)}</span>
              <input
                id={`dirFormField-${entry.name}-${ph}`}
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
            <label
              htmlFor={`dirFormApiKey-${entry.name}`}
              className="grid gap-1 text-[0.72rem] text-[var(--color-text-secondary)]"
            >
              <span className="font-medium">API key</span>
              <input
                id={`dirFormApiKey-${entry.name}`}
                ref={apiKeyRef}
                className={SETTINGS_INPUT}
                type="password"
                autoComplete="off"
                value={apiKey}
                onChange={(e) => setApiKey(e.target.value)}
                placeholder="paste your key (stored encrypted, never shown again)"
              />
              {keySentAsNote(entry.api_key_header, entry.api_key_prefix) ? (
                <span
                  className="font-mono text-[0.66rem] text-[var(--color-text-muted)]"
                  data-testid={`dir-form-key-note-${entry.name}`}
                >
                  {keySentAsNote(entry.api_key_header, entry.api_key_prefix)}
                </span>
              ) : null}
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
                      onClick={() => void copyText(redirectUri)}
                    >
                      Copy
                    </button>
                  </div>
                </div>
              ) : null}
              <label
                htmlFor={`dirFormClientId-${entry.name}`}
                className="grid gap-1 text-[0.72rem] text-[var(--color-text-secondary)]"
              >
                <span className="font-medium">OAuth client ID</span>
                <input
                  id={`dirFormClientId-${entry.name}`}
                  className={SETTINGS_INPUT}
                  value={clientId}
                  onChange={(e) => setClientId(e.target.value)}
                  placeholder="from your app registration — see the setup guide"
                  data-testid={`dir-form-client-id-${entry.name}`}
                />
              </label>
              <label
                htmlFor={`dirFormClientSecret-${entry.name}`}
                className="grid gap-1 text-[0.72rem] text-[var(--color-text-secondary)]"
              >
                <span className="font-medium">
                  {secretRequired
                    ? "OAuth client secret"
                    : "OAuth client secret (if your client has one)"}
                </span>
                <input
                  id={`dirFormClientSecret-${entry.name}`}
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
  // The deployment's name labels the default credential seat ("Elcano").
  const { branding } = useClientConfig();
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
  // The endpoint the new seat is added on, for an entry with url_variants
  // (regional hosts): it starts on the first seat's URL, and the form offers
  // the entry's endpoints, since a second account can live in another region.
  const [addSeatURL, setAddSeatURL] = useState("");
  // Explicit per-user availability choices (unified connector UX); absence of
  // an entry means the operator default.
  const [prefs, setPrefs] = useState<ConnectorPref[]>([]);
  const [catalog, setCatalog] = useState<CatalogResponse | null>(null);
  const [bundledAnchor, setBundledAnchor] = useState(readConnectorAnchor);
  // A link to a connector outside the preview opens its full list as soon as
  // the catalog arrives. An explicit open/close choice wins after that.
  const [bundledDialogOpen, setBundledDialogOpen] = useState<boolean | null>(null);
  const showBundledDialog = bundledDialogOpen ?? (catalog?.bundled
    .slice(BUNDLED_PREVIEW_COUNT)
    .some((entry) => connectorAnchor(entry.name) === bundledAnchor) ?? false);
  useEffect(() => {
    const onHashChange = () => {
      setBundledAnchor(readConnectorAnchor());
      setBundledDialogOpen(null);
    };
    window.addEventListener("hashchange", onHashChange);
    return () => window.removeEventListener("hashchange", onHashChange);
  }, []);
  // The ?connector=<name> deep link: filter the directory to that entry and
  // open its guided form, so "add your Browserbase key" is one click + one
  // paste from anywhere that can mint a URL. One-shot, like the OAuth banner.
  const [spotlight] = useState(readConnectorSpotlight);
  // The directory is the page's main discovery surface — open by default,
  // collapsible for users who only manage existing connections.
  const [catalogOpen, setCatalogOpen] = useState(true);
  // Seeding the search from the spotlight is what filters the directory to the
  // linked entry — search spans every category, so this works wherever the
  // entry is grouped, and clearing the box restores the popular picks.
  const [catalogQuery, setCatalogQuery] = useState(spotlight ?? "");
  // Consent modal state: the non-official (aggregator/community) entry
  // awaiting the user's explicit, operator-named consent, plus whatever the
  // card's guided form collected (tenant URL / API key) so the confirm can
  // complete the same add.
  const [consentFor, setConsentFor] = useState<{
    entry: CatalogThirdParty;
    overrides?: AddOverrides;
  } | null>(null);
  // A banner is delivered as a one-shot TOAST, so it must fire again when the
  // same message repeats. Keyed on the message VALUE, a retry that failed
  // identically ("Connection refused" twice) changed no state, so no second
  // toast appeared and the retry read as success. Carry a monotonic sequence
  // beside the text and key the effects on the whole record, so every call to
  // setError/setNotice is one user-visible event regardless of its wording.
  const [errorBanner, setErrorBanner] = useState<{
    text: string | null;
    seq: number;
  }>(() => ({ text: initialBanner.error, seq: 0 }));
  const [noticeBanner, setNoticeBanner] = useState<{
    text: string | null;
    seq: number;
  }>(() => ({ text: initialBanner.notice, seq: 0 }));
  const setError = useCallback((text: string | null) => {
    setErrorBanner((prev) => ({ text, seq: prev.seq + 1 }));
  }, []);
  const setNotice = useCallback((text: string | null) => {
    setNoticeBanner((prev) => ({ text, seq: prev.seq + 1 }));
  }, []);
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
    if (errorBanner.text) showToast(errorBanner.text, "error", 6000);
  }, [errorBanner, showToast]);
  useEffect(() => {
    if (noticeBanner.text) showToast(noticeBanner.text, "success");
  }, [noticeBanner, showToast]);
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
  const { servers: mcpServers } = useMcpServers(true);

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
  }, [setError]);

  const refresh = useCallback(() => {
    setError(null);
    setLoading(true);
    apply(() => false);
  }, [apply, setError]);

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
          throw new Error(await responseErrorText(res, "Add failed"));
        }
        const data = (await res.json()) as { id?: string; name?: string };
        setName("");
        setUrl("");
        setAccountLabel("");
        setAddServerOpen(false);
        setNotice("Connection added. Click Sign in to finish.");
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
            await responseErrorText(res, "Authorize failed"),
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
          throw new Error(await responseErrorText(res, "Update failed"));
        }
        const data =
          res.status === 204
            ? null
            : ((await res.json()) as { tool_count?: number; key_verified?: boolean });
        setKeyOpenFor(null);
        setKeyValue("");
        setNotice(
          `API key updated${toolCountSuffix(data?.tool_count)}${keyVerifiedSuffix(data?.key_verified)}.`,
        );
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
            await responseErrorText(res, "Set default failed"),
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
          throw new Error(await responseErrorText(res, "Rename failed"));
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
        url: addSeatURL || template.url,
        ...(authKind === "oauth" ? {} : { auth: authKind }),
        account: label,
        ...(authKind === "api_key"
          ? {
              api_key: addSeatKey.trim(),
              api_key_header: dir?.api_key_header,
              api_key_query: dir?.api_key_query,
              api_key_prefix: dir?.api_key_prefix,
            }
          : {}),
      }),
    })
      .then(async (res) => {
        if (!res.ok) {
          throw new Error(await responseErrorText(res, "Add failed"));
        }
        const data = (await res.json()) as {
          id?: string;
          tool_count?: number;
          key_verified?: boolean;
        };
        const shown = `${group.name} (${label})`;
        setAddSeatFor(null);
        setAddSeatLabel("");
        setAddSeatKey("");
        setAddSeatURL("");
        if (authKind === "oauth") {
          setNotice(`${shown} added. Click Sign in to finish.`);
          if (data.id) setConnectPromptFor({ id: data.id, name: shown });
        } else {
          setNotice(
            `${shown} connected${toolCountSuffix(data.tool_count)}${keyVerifiedSuffix(data.key_verified)}.`,
          );
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
              api_key_prefix: entry.api_key_prefix,
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
          throw new Error(await responseErrorText(res, "Add failed"));
        }
        const data = (await res.json()) as {
          id?: string;
          tool_count?: number;
          key_verified?: boolean;
        };
        const shown = overrides?.account
          ? `${entry.display_name} (${overrides.account})`
          : entry.display_name;
        if (entry.auth === "open" || entry.auth === "api_key") {
          setNotice(
            `${shown} connected${toolCountSuffix(data.tool_count)}${keyVerifiedSuffix(data.key_verified)}.`,
          );
        } else {
          setNotice(`${shown} added. Click Sign in to finish.`);
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
          throw new Error(await responseErrorText(res, "Save failed"));
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
          throw new Error(await responseErrorText(res, "Reset failed"));
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
          throw new Error(await responseErrorText(res, "Share failed"));
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
            await responseErrorText(res, "Unshare failed"),
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
            await responseErrorText(res, "Sign out failed"),
          );
        }
        setNotice(
          "Signed out. The connection is kept; click Sign in to use it again.",
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
            await responseErrorText(res, "Disconnect failed"),
          );
        }
        setNotice("Disconnected.");
        refresh();
      })
      .catch((err: unknown) => setError(errMessage(err)))
      .finally(() => setBusy(false));
  };

  const renderBundledCard = (b: CatalogBundled, idPrefix = "") => {
    if (b.needs_setup) {
      return <NeedsSetupCard key={b.name} entry={b} isAdmin={adminState === "admin"} idPrefix={idPrefix} />;
    }
    if (b.optional === false) {
      // Always-on: operator-wired into every turn; shown locked so
      // nothing is invisibly enabled.
      return (
        <div key={b.name} id={`${idPrefix}${connectorAnchor(b.name)}`} className={BUNDLED_CARD}>
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
        idPrefix={idPrefix}
        appName={branding.app_name}
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
  };

  // Keep the landing view small; a search still covers the whole catalog.
  // Workspace overrides can omit our picks, so fill any remaining slots from
  // featured entries, then the workspace's own catalog order.
  const popularNames = [...POPULAR_CONNECTORS.keys()];
  const thirdParty = (catalog?.third_party ?? []).map((entry) => {
    const displayName = POPULAR_CONNECTORS.get(entry.name);
    return displayName
      ? {
          ...entry,
          display_name: displayName,
          tags: [...(entry.tags ?? []), entry.display_name.toLowerCase()],
        }
      : entry;
  });
  const popularRank = (entry: CatalogThirdParty) => {
    const index = popularNames.indexOf(entry.name);
    return index >= 0 ? index : popularNames.length + (entry.featured ? 0 : 1);
  };
  const dirPopular = [...thirdParty]
    .sort((a, b) => popularRank(a) - popularRank(b))
    .slice(0, DIRECTORY_PREVIEW_COUNT);
  const trimmedQuery = catalogQuery.trim();
  const dirHits = trimmedQuery ? filterCatalog(thirdParty, catalogQuery, "") : dirPopular;

  const renderDirCard = (tp: CatalogThirdParty) => (
    <DirectoryCard
      key={tp.name}
      entry={tp}
      popular={POPULAR_CONNECTORS.has(tp.name)}
      added={(servers ?? []).some(
        (s) => s.url === tp.url || s.name === tp.name,
      )}
      addedURL={
        (servers ?? []).find((s) => s.url === tp.url || s.name === tp.name)?.url
      }
      busy={busy}
      remoteEnabled={catalog?.remote_mcp_enabled ?? false}
      redirectUri={catalog?.oauth_redirect_uri}
      onAdd={(overrides) => requestAddFromCatalog(tp, overrides)}
      autoOpenForm={spotlight === tp.name}
    />
  );

  // Hidden bundled cards live in the popup. Prefix its anchors and account
  // inputs so the preview and dialog never duplicate DOM ids.
  useEffect(() => {
    if (!catalog || !bundledAnchor) return;
    const id = showBundledDialog ? `all-bundled-${bundledAnchor}` : bundledAnchor;
    document.getElementById(id)?.scrollIntoView({ behavior: "auto", block: "center" });
  }, [catalog, bundledAnchor, showBundledDialog]);

  // Once the directory has loaded, bring the deep-linked card into view. The
  // spotlight card is usually the only hit (the search box was seeded with its
  // name), but scroll anyway: the page has three groups above the directory.
  useEffect(() => {
    if (!spotlight || !catalog) return;
    document
      .querySelector(`[data-testid="dir-card-${CSS.escape(spotlight)}"]`)
      ?.scrollIntoView({ behavior: "auto", block: "center" });
  }, [spotlight, catalog]);

  return (
    <SetSection
      title="Connections"
      intro="Choose the apps and data your chats and tasks can use. You sign in with your own account; your credentials are encrypted and never shared."
    >
      {/* ── Group 1 — Your connections ── */}
      <ConnGroup>
        <ConnGroupHead title="Your connections">
          Everything your chats can use today.
        </ConnGroupHead>

        {catalog && catalog.bundled.length > 0 ? (
          <ConnPanel>
            <ConnPanelHead title="Included with your workspace">
              {catalog.bundled.length > BUNDLED_PREVIEW_COUNT ? (
                <button
                  type="button"
                  aria-haspopup="dialog"
                  onClick={() => setBundledDialogOpen(true)}
                  className={btnClass({ sm: true, reveal: true })}
                >
                  <Icon name="maximize" className="size-[0.85rem]" />
                  Show all ({catalog.bundled.length})
                </button>
              ) : null}
            </ConnPanelHead>
            <ConnPanelSub>
              Choose which ones your chats use.
            </ConnPanelSub>
            <div className="grid grid-cols-2 gap-[0.7rem] max-[860px]:grid-cols-1">
              {catalog.bundled.slice(0, BUNDLED_PREVIEW_COUNT).map((entry) => renderBundledCard(entry))}
            </div>
          </ConnPanel>
        ) : null}

        <ConnPanel>
          <ConnPanelHead title="Added by you">
            <div className="flex flex-wrap items-center gap-2">
              <button
                type="button"
                data-testid="create-custom-connection-btn"
                onClick={() =>
                  navigate(
                    `/chat?draft=${encodeURIComponent(connectorBuilderMessage())}`,
                  )
                }
                className={btnClass({ sm: true, variant: "primary" })}
              >
                Build a connection in chat
              </button>
              <RevealButton
                open={addServerOpen}
                closedLabel="Add by URL"
                onClick={() => setAddServerOpen((o) => !o)}
              />
            </div>
          </ConnPanelHead>
          <ConnPanelSub>
            Connections you added from the directory, by URL, or with the
            connection assistant.
          </ConnPanelSub>
          {addServerOpen ? (
            <form onSubmit={addServer}>
              <ConnForm>
                <ConnField label="Name">
                  <input
                    id="remoteServerName"
                    className={SETTINGS_INPUT}
                    value={name}
                    onChange={(e) => setName(e.target.value)}
                    placeholder="my-server"
                    required
                  />
                </ConnField>
                <ConnField label="Server URL" grow>
                  <input
                    id="remoteServerUrl"
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
                    id="remoteServerAccountLabel"
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
            <div className="grid gap-[0.7rem]">
              {/* One card per connection name; one row per seat (login)
                  inside it (#988). A card with a login that needs signing in
                  takes the warning border so it stands out in a long list. */}
              {groupSeats(servers).map((group) => {
                const multi = group.seats.length > 1;
                const template = group.seats[0];
                const groupAuth = template?.auth_kind || "oauth";
                const groupNeedsSignIn = group.seats.some((s) => needsSignIn(s.status));
                return (
                  <div
                    key={group.name}
                    // The chat Tools library's "Manage in Settings" lands here.
                    id={connectorAnchor(group.name)}
                    data-testid={`remote-group-${group.name}`}
                    className={[
                      "scroll-mt-24 rounded-[var(--radius-md)] border bg-[var(--color-overlay-soft)] px-[0.9rem] py-[0.3rem] target:border-[var(--color-accent)] target:shadow-[0_0_0_1px_var(--color-accent)]",
                      groupNeedsSignIn
                        ? "border-[var(--color-panel-warn-border)]"
                        : "border-[var(--color-border)]",
                    ].join(" ")}
                  >
                    <ConnRows>
                    {group.seats.map((s) => {
                      const enabledForMe = effectiveEnabled(prefs, "remote", s.id);
                      const shareCount = shares[s.id]?.length ?? 0;
                      const customBadge = customTransportBadge(
                        s.transport,
                        s.custom_tools?.length,
                      );
                      const isCustomTransport =
                        s.transport === "http_tools" ||
                        s.transport === "playwright";
                      return (
                        <ConnRow
                          key={s.id}
                          name={
                            <span className="inline-flex flex-wrap items-center gap-[0.55rem]">
                              <span className="text-[0.95rem] font-semibold">{s.name}</span>
                              <ConnBadge title="Account label for this login">
                                {seatLabel(s)}
                              </ConnBadge>
                              {customBadge ? (
                                <ConnBadge title="Custom connector synthesized in Connector Studio">
                                  {customBadge}
                                </ConnBadge>
                              ) : null}
                              {s.is_default ? (
                                <ConnBadge
                                  variant="overridden"
                                  title="Chats and tasks use this login unless a conversation or task picks another."
                                >
                                  Default
                                </ConnBadge>
                              ) : null}
                              <ConnBadge variant={statusVariant(s.status)}>
                                {statusLabel(s.status, s.auth_kind)}
                              </ConnBadge>
                              {s.catalog_drift ? (
                                <ConnBadge variant="warn">Directory changed</ConnBadge>
                              ) : null}
                            </span>
                          }
                          sub={
                            s.catalog_drift ? (
                              <>
                                {s.url}
                                <br />
                                {driftSummary(s.catalog_drift)}
                              </>
                            ) : (
                              s.url
                            )
                          }
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
                                  className={btnClass({
                                    sm: true,
                                    reveal: true,
                                    variant: needsSignIn(s.status) ? "primary" : "ghost",
                                  })}
                                >
                                  Update key
                                </button>
                              ) : s.auth_kind === "open" || isCustomTransport ? null : (
                                <button
                                  type="button"
                                  onClick={() => connect(s.id)}
                                  disabled={busy}
                                  className={btnClass({
                                    sm: true,
                                    reveal: true,
                                    variant: needsSignIn(s.status) ? "primary" : "ghost",
                                  })}
                                >
                                  {s.status === "connected" ? "Sign in again" : "Sign in"}
                                </button>
                              )}
                              {s.auth_kind !== "api_key" &&
                              s.auth_kind !== "open" &&
                              !isCustomTransport &&
                              s.status === "connected" ? (
                                <button
                                  type="button"
                                  onClick={() => signOut(s.id)}
                                  disabled={busy}
                                  title="Sign out of this account and keep the connection. Click Sign in to use it again."
                                  className={btnClass({ sm: true })}
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
                                  className={btnClass({ sm: true })}
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
                                className={btnClass({ sm: true })}
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
                                className={btnClass({ sm: true })}
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
                                  id={`renameSeat-${s.id}`}
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
                                  id={`updateKey-${s.id}`}
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
                                {keySentAsNote(undefined, s.api_key_prefix) ? (
                                  <span className="basis-full font-mono text-[0.66rem] text-[var(--color-text-muted)]">
                                    {keySentAsNote(undefined, s.api_key_prefix)}
                                  </span>
                                ) : null}
                              </div>
                            ) : shareOpenFor === s.id ? (
                              <div className="mt-2 rounded-[var(--radius-md)] border border-[var(--color-border-subtle)] bg-[var(--color-overlay-soft)] px-3 py-2.5">
                                <p className="mb-2 mt-0 text-[0.75rem] leading-[1.5] text-[var(--color-text-muted)]">
                                  People you share with can use this connection in
                                  their chats and scheduled tasks through{" "}
                                  <strong className="text-[var(--color-text-secondary)]">
                                    your
                                  </strong>{" "}
                                  account. They never see your sign-in details, and
                                  removing a share stops their access right away.
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
                                    id={`shareGrantee-${s.id}`}
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
                          setAddSeatURL(group.seats[0]?.url ?? "");
                          setAddSeatFor((cur) =>
                            cur === group.name ? null : group.name,
                          );
                        }}
                        disabled={busy}
                        className={btnClass({ sm: true })}
                      >
                        <Icon
                          name={addSeatFor === group.name ? "close" : "plus"}
                          className="size-[0.85rem]"
                        />
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
                          id={`addSeatLabel-${group.name}`}
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
                        {(() => {
                          const dir = (catalog?.third_party ?? []).find(
                            (e) => e.name === group.name,
                          );
                          const variants = dir?.url_variants ?? [];
                          if (!dir || variants.length === 0) return null;
                          const current = group.seats[0]?.url ?? "";
                          const options = [
                            { url: dir.url, label: `Default (${hostOf(dir.url)})` },
                            ...variants.map((v) => ({
                              url: v.url,
                              label: `${v.label} — ${hostOf(v.url)}`,
                            })),
                          ];
                          if (current && !options.some((o) => o.url === current)) {
                            options.unshift({
                              url: current,
                              label: `This connection's endpoint (${hostOf(current)})`,
                            });
                          }
                          return (
                            <select
                              value={addSeatURL || current}
                              onChange={(e) => setAddSeatURL(e.target.value)}
                              aria-label={`Endpoint for the new ${group.name} login`}
                              data-testid={`add-seat-endpoint-${group.name}`}
                              className="min-w-0 flex-1 basis-[12rem] rounded-[var(--radius-md)] border border-[var(--color-border)] bg-[var(--color-surface-1)] px-3 py-1.5 text-[0.8125rem] text-[var(--color-text-primary)] outline-none focus-visible:border-[var(--color-border-strong)] focus-visible:shadow-[var(--focus-ring)]"
                            >
                              {options.map((o) => (
                                <option key={o.url} value={o.url}>
                                  {o.label}
                                </option>
                              ))}
                            </select>
                          );
                        })()}
                        {groupAuth === "api_key" ? (
                          <input
                            id={`addSeatKey-${group.name}`}
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
                        {groupAuth === "api_key"
                          ? (() => {
                              // The same directory entry addSeat posts the
                              // prefix from, so the note says what is sent.
                              const dir = (catalog?.third_party ?? []).find(
                                (e) => e.name === group.name,
                              );
                              const note = keySentAsNote(
                                dir?.api_key_header,
                                dir?.api_key_prefix,
                              );
                              return note ? (
                                <span
                                  className="basis-full font-mono text-[0.66rem] text-[var(--color-text-muted)]"
                                  data-testid={`add-seat-key-note-${group.name}`}
                                >
                                  {note}
                                </span>
                              ) : null;
                            })()
                          : null}
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
              Nothing added yet. Pick one from the list below, or add one by URL.
            </ConnEmpty>
          )}
        </ConnPanel>

        {sharedWithMe.length > 0 ? (
          <ConnPanel>
            <ConnPanelHead title="Shared with you" />
            <ConnPanelSub>
              Connections other people shared with you. You can use them in your
              chats and scheduled tasks through the owner&apos;s account.
            </ConnPanelSub>
            <ConnRows>
              {sharedWithMe.map((s) => {
                const on = effectiveEnabled(prefs, "remote", s.id);
                const sharedCustomBadge = customTransportBadge(
                  s.transport,
                  s.custom_tools?.length,
                );
                return (
                  <ConnRow
                    key={s.id}
                    name={
                      <span className="inline-flex flex-wrap items-center gap-[0.55rem]">
                        {s.name}
                        <ConnBadge title="The owner's account label for this login">
                          {seatLabel(s)}
                        </ConnBadge>
                        {sharedCustomBadge ? (
                          <ConnBadge title="Custom connector synthesized in Connector Studio">
                            {sharedCustomBadge}
                          </ConnBadge>
                        ) : null}
                        <ConnBadge variant={statusVariant(s.status)}>
                          {statusLabel(s.status, s.auth_kind)}
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
          Named sign-ins your connectors use — e.g. one per client seat. Your
          administrator provisions them on the server; secret values are never
          displayed.
        </ConnGroupHead>
        <CredentialAccountAdmin servers={mcpServers} />
      </ConnGroup>

      {/* ── Group 3 — Connector directory (collapsible) ── */}
      {catalog && catalog.third_party.length > 0 ? (
        <ConnGroup>
          {/* ConnGroupHead's metrics, recomposed so the collapse toggle sits on
              the title row. */}
          <div className="mb-[0.8rem]">
            <div className="flex items-center justify-between gap-4">
              <h3 className="m-0 text-[1.08rem] font-semibold text-[var(--color-text-primary)]">
                Find more connections
              </h3>
              <button
                type="button"
                aria-expanded={catalogOpen}
                onClick={() => setCatalogOpen((v) => !v)}
                className={btnClass({ sm: true, reveal: true })}
              >
                {catalogOpen ? "Hide" : `Browse all ${catalog.third_party.length}`}
              </button>
            </div>
            <p className="mb-0 mt-[0.35rem] text-[0.78rem] leading-[1.55] text-[var(--color-text-muted)]">
              Start with a popular app, or search for the one you use. An app you
              connect sees the parts of your conversations the assistant sends it.
            </p>
          </div>

          {catalogOpen ? (
            <>
              <div className="mb-[1.1rem] grid gap-[0.55rem]">
                <DirSearch
                  value={catalogQuery}
                  onChange={setCatalogQuery}
                  placeholder="Search connections…"
                  label="Search connections"
                />
                <p className="m-0 text-[0.75rem] leading-[1.55] text-[var(--color-text-muted)]">
                  Can’t find what you’re looking for?{" "}
                  <a href="mailto:hello@elcanotek.com?subject=Connector%20request" className="text-[var(--color-link)] underline underline-offset-2">
                    Contact Elcano
                  </a>{" "}
                  if you’re interested in adding a connector.
                </p>
              </div>

              <div>
                {trimmedQuery ? (
                  <p role="status" className="mb-3 mt-0 text-[0.75rem] text-[var(--color-text-muted)]">
                    {dirHits.length === 1
                      ? "1 connector matches"
                      : `${dirHits.length} connectors match`}
                  </p>
                ) : null}
                {dirHits.length === 0 ? (
                  <ConnEmpty>
                    <span>
                      No connectors match “{trimmedQuery}”. Try another name or keyword.
                    </span>
                    <span className="mt-2 block">
                      <button
                        type="button"
                        data-testid="dir-empty-create-custom-btn"
                        onClick={() =>
                          navigate(
                            `/chat?draft=${encodeURIComponent(
                              connectorBuilderMessage({
                                target: trimmedQuery || undefined,
                              }),
                            )}`,
                          )
                        }
                        className={btnClass({ sm: true })}
                      >
                        {trimmedQuery
                          ? `Build a custom “${trimmedQuery}” connection in chat`
                          : "Create your own connection in chat"}
                      </button>
                    </span>
                  </ConnEmpty>
                ) : (
                  <div className="grid grid-cols-2 gap-[0.7rem] max-[860px]:grid-cols-1">
                    {dirHits.map(renderDirCard)}
                  </div>
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

      {/* Escape the settings entrance animation's stacking context so the
          popup and scrim cover the navigation rail as well as the page. */}
      {showBundledDialog && catalog ? createPortal(
        <DialogShell
          label="Included with your workspace"
          scrimLabel="Close included connections"
          layer="rail"
          onDismiss={() => setBundledDialogOpen(false)}
          className="flex max-h-[85dvh] max-w-4xl flex-col p-5"
        >
          <div className="mb-4 flex items-center justify-between gap-4">
            <div>
              <h3 className="m-0 text-[1rem] font-semibold">Included with your workspace</h3>
              <p className="mb-0 mt-1 text-[0.78rem] text-[var(--color-text-muted)]">
                All {catalog.bundled.length} connections. Changes apply to your chats. Scheduled
                tasks keep the connections they were set up with.
              </p>
            </div>
            <CloseButton label="Close" onClick={() => setBundledDialogOpen(false)} />
          </div>
          {errorBanner.text ? (
            <p role="alert" className="mb-3 mt-0 text-[0.78rem] text-[var(--color-danger-text)]">
              {errorBanner.text}
            </p>
          ) : null}
          <div className="min-h-0 overflow-y-auto p-1">
            <div className="grid grid-cols-2 gap-[0.7rem] max-[860px]:grid-cols-1">
              {catalog.bundled.map((entry) => renderBundledCard(entry, "all-bundled-"))}
            </div>
          </div>
        </DialogShell>,
        document.body,
      ) : null}

      {/* Consent step for endpoints not operated by the service's own vendor.
          A badge alone gets scrolled past; connecting sends conversation-
          derived tool traffic to (and often parks a delegated access token
          with) the named operator, so the add is gated on an explicit,
          operator-named confirmation. */}
      {/* Post-add sign-in prompt: an OAuth server was just added and needs a
          login to finish connecting. Offer it here so the user doesn't have
          to scroll up to the "Your connections" row to click Sign in. */}
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
            One step left: sign in so you can use it in your chats. You can also
            do it later with its Sign in button under Your connections.
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
