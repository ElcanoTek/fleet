"use client";

import { useState } from "react";

import type { PermissionOption, PermissionRequest } from "./history";

// PermissionCard renders an EXTERNAL ACP agent's session/request_permission as
// an inline allow/deny prompt. The agent (Claude Code / Goose) self-executes in
// a locked sandbox; this is the human-in-the-loop gate for whatever it deems
// sensitive. The agent's turn is BLOCKED server-side until the user decides —
// or the server's default-deny timeout fires (the card then flips to "denied"
// via the permission.resolved SSE event). There is NO "approve all": each
// request is its own decision, and only the agent's offered allow-shaped
// option(s) are surfaced as allow buttons.
export function PermissionCard({
  request,
  conversationId,
  onResolved,
}: {
  request: PermissionRequest;
  conversationId: string;
  onResolved: (next: PermissionRequest) => void;
}) {
  const [submitting, setSubmitting] = useState<"allow" | "deny" | null>(null);

  // Allow-shaped options the human may pick (allow_once / allow_always). We do
  // NOT render allow_always as a distinct "approve all" — it is treated as a
  // one-time allow, decided on this request alone.
  const allowOptions = request.options.filter(
    (o) => o.kind === "allow_once" || o.kind === "allow_always",
  );

  const resolve = async (allowed: boolean, optionId?: string) => {
    if (submitting || request.status !== "pending" || !conversationId) return;
    setSubmitting(allowed ? "allow" : "deny");
    try {
      const response = await fetch(
        `/api/conversations/${encodeURIComponent(conversationId)}/permissions/${encodeURIComponent(request.id)}`,
        {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ allowed, option_id: optionId ?? "" }),
        },
      );
      if (!response.ok) {
        // A failed POST leaves the card pending; the server default-deny
        // timeout is the backstop and will resolve it via SSE.
        setSubmitting(null);
        return;
      }
      onResolved({ ...request, status: allowed ? "allowed" : "denied" });
    } catch {
      setSubmitting(null);
    }
  };

  const statusStyle: React.CSSProperties =
    request.status === "allowed"
      ? { borderColor: "var(--color-success-border)", color: "var(--color-success)" }
      : request.status === "denied"
        ? { borderColor: "var(--color-border-strong)", color: "var(--color-text-muted)" }
        : { borderColor: "var(--color-accent)", color: "var(--color-text-primary)" };

  const allowButtons: PermissionOption[] =
    allowOptions.length > 0
      ? allowOptions
      : [{ optionId: "", name: "Allow", kind: "allow_once" }];

  return (
    <div
      data-testid="permission-card"
      className="rounded-[0.95rem] border bg-[color-mix(in_srgb,var(--color-overlay-soft)_55%,transparent)] px-3 py-2.5 text-[0.8125rem] leading-[1.5]"
      style={statusStyle}
    >
      <div className="mb-2 flex items-center gap-2">
        <span aria-hidden>🔐</span>
        <span className="font-medium">
          {request.status === "pending"
            ? "The agent is requesting permission"
            : request.status === "allowed"
              ? "Permission allowed"
              : "Permission denied"}
        </span>
      </div>

      <div className="text-[var(--color-text-primary)]">{request.title}</div>

      {request.locations && request.locations.length > 0 ? (
        <div className="mt-1 grid gap-0.5 text-[0.72rem] text-[var(--color-text-muted)]">
          {request.locations.map((loc, i) => (
            <div key={i} className="break-all font-mono">
              {loc}
            </div>
          ))}
        </div>
      ) : null}

      {request.status === "pending" ? (
        <div className="mt-3 flex flex-wrap items-center gap-2">
          {allowButtons.map((opt) => (
            <button
              key={opt.optionId || "allow"}
              type="button"
              data-testid="permission-allow"
              className="rounded-full bg-[var(--color-primary)] px-3 py-1.5 text-[0.75rem] font-medium text-white transition hover:opacity-90 disabled:opacity-50"
              disabled={submitting !== null}
              onClick={() => resolve(true, opt.optionId)}
            >
              {submitting === "allow" ? "Allowing…" : opt.name || "Allow"}
            </button>
          ))}
          <button
            type="button"
            data-testid="permission-deny"
            className="rounded-full border border-[var(--color-border-strong)] px-3 py-1.5 text-[0.75rem] text-[var(--color-text-secondary)] transition hover:text-[var(--color-text-primary)] disabled:opacity-50"
            disabled={submitting !== null}
            onClick={() => resolve(false)}
          >
            {submitting === "deny" ? "Denying…" : "Deny"}
          </button>
        </div>
      ) : null}
    </div>
  );
}
