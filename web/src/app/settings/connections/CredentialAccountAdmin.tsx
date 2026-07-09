"use client";

import { useState } from "react";
import type { McpServer } from "@/app/shared/lib/orchestratorApi";
import { orchestratorApi } from "@/app/shared/lib/orchestratorApi";
import { Icon } from "@/app/shared/ui/Icon";
import {
  ActStatus,
  btnClass,
  InlineConfirmButton,
  RevealButton,
  SETTINGS_INPUT,
} from "../ui/atoms";
import {
  ConnEmpty,
  ConnField,
  ConnForm,
  ConnFormActions,
  ConnPanel,
  ConnPanelHead,
  ConnRow,
  ConnRows,
  SecretsFieldset,
} from "../ui/panels";

// CredentialAccountAdmin — manage MCP credential accounts per server
// (the "Accounts" panel under Settings → Connections → Credential accounts).
//
// CRITICAL SECURITY INVARIANT: secret fields are WRITE-ONLY. The catalog the
// admin reads (server.accounts) is names ONLY — secret values are never
// returned by any read endpoint, so this UI NEVER echoes a secret back. The
// secret <input type="password"> starts empty even when editing an existing
// account; submitting it sets new values. An empty secret field on an existing
// account means "leave unchanged" (we don't send it), never "clear it to ''".
//
// Per migration §6.3: `fleet mcp account set <server> <account> --secret KEY=-`
// is the CLI analogue; this is the dashboard analogue, going through
// POST/PUT /api/orchestrator/mcp-servers/{server}/accounts.

export type CredentialAccountAdminProps = {
  servers: McpServer[];
  onChanged?: () => void;
};

type SecretField = { key: string; value: string };

// Writes go through to the real fleet backend and can fail (bad account name,
// backend down, auth) — outcomes surface as an inline status line rather than
// a toast, so the message stays anchored to the panel that caused it.
type Status = { state: "ok" | "err"; msg: string };

export function CredentialAccountAdmin({ servers, onChanged }: CredentialAccountAdminProps) {
  const [open, setOpen] = useState(false);
  const [server, setServer] = useState<string>(servers[0]?.name ?? "");
  const [account, setAccount] = useState<string>("");
  const [secrets, setSecrets] = useState<SecretField[]>([{ key: "", value: "" }]);
  const [submitting, setSubmitting] = useState(false);
  const [status, setStatus] = useState<Status | null>(null);

  // The server catalog loads async on the page — fall back to the first entry
  // once it arrives so the select never submits an empty server name.
  const selectedServer = server || (servers[0]?.name ?? "");

  const updateSecret = (idx: number, patch: Partial<SecretField>) => {
    setSecrets((prev) => prev.map((s, i) => (i === idx ? { ...s, ...patch } : s)));
  };

  const addSecretRow = () => setSecrets((prev) => [...prev, { key: "", value: "" }]);

  const removeSecretRow = (idx: number) =>
    setSecrets((prev) => prev.filter((_, i) => i !== idx));

  const reset = () => {
    setAccount("");
    setSecrets([{ key: "", value: "" }]);
  };

  const submit = async () => {
    setStatus(null);
    if (!selectedServer || !account.trim()) {
      setStatus({ state: "err", msg: "Server and account name are required" });
      return;
    }
    // Only forward secrets that have BOTH a key and a (non-empty) value.
    // Empty values are dropped, never written as "" — write-only semantics.
    const payload: Record<string, string> = {};
    for (const { key, value } of secrets) {
      if (key.trim() && value !== "") payload[key.trim()] = value;
    }
    if (Object.keys(payload).length === 0) {
      setStatus({ state: "err", msg: "Add at least one secret KEY=value" });
      return;
    }
    setSubmitting(true);
    try {
      await orchestratorApi.createAccount(selectedServer, {
        account: account.trim(),
        secrets: payload,
      });
      setStatus({
        state: "ok",
        msg: `Saved account "${account.trim()}" for ${selectedServer}`,
      });
      reset();
      setOpen(false);
      onChanged?.();
    } catch (err) {
      setStatus({ state: "err", msg: `Failed to save account: ${(err as Error).message}` });
    } finally {
      setSubmitting(false);
    }
  };

  const remove = async (srv: string, acct: string) => {
    setStatus(null);
    setSubmitting(true);
    try {
      await orchestratorApi.deleteAccount(srv, acct);
      setStatus({ state: "ok", msg: `Removed account "${acct}" from ${srv}` });
      onChanged?.();
    } catch (err) {
      setStatus({ state: "err", msg: `Failed to remove account: ${(err as Error).message}` });
    } finally {
      setSubmitting(false);
    }
  };

  const accountRows = servers.flatMap((srv) =>
    (srv.accounts ?? []).map((acct) => ({ srv: srv.name, acct })),
  );

  return (
    <div data-testid="credential-account-admin">
      <ConnPanel>
        <ConnPanelHead title="Accounts">
          <RevealButton
            open={open}
            closedLabel="Add account"
            onClick={() => {
              setStatus(null);
              setOpen((o) => !o);
            }}
            testId="credential-account-add"
          />
        </ConnPanelHead>

        {/* Existing accounts: names ONLY, never secret values. */}
        {accountRows.length > 0 ? (
          <ConnRows>
            {accountRows.map(({ srv, acct }) => (
              <ConnRow
                key={`${srv}:${acct}`}
                name={
                  <span data-testid={`credential-account-${srv}-${acct}`}>
                    <code>{srv}</code> / {acct}
                  </span>
                }
                actions={
                  <InlineConfirmButton
                    label="Delete"
                    onConfirm={() => void remove(srv, acct)}
                    disabled={submitting}
                    testId={`credential-account-delete-${srv}-${acct}`}
                  />
                }
              />
            ))}
          </ConnRows>
        ) : !open ? (
          <ConnEmpty>No accounts yet.</ConnEmpty>
        ) : null}

        {status ? (
          <div className="mt-2">
            <ActStatus state={status.state}>{status.msg}</ActStatus>
          </div>
        ) : null}

        {open ? (
          <div className="mt-3 border-t border-[var(--color-border-subtle)] pt-[0.9rem]">
            <ConnForm>
              <ConnField label="Server">
                <span className="select-wrap block">
                  <select
                    className={`${SETTINGS_INPUT} appearance-none pr-8!`}
                    value={selectedServer}
                    onChange={(e) => setServer(e.target.value)}
                    disabled={submitting}
                  >
                    {servers.map((s) => (
                      <option key={s.name} value={s.name}>
                        {s.name}
                      </option>
                    ))}
                  </select>
                </span>
              </ConnField>
              <ConnField label="Account name" grow>
                <input
                  className={SETTINGS_INPUT}
                  type="text"
                  placeholder="e.g. client_a"
                  value={account}
                  onChange={(e) => setAccount(e.target.value)}
                  disabled={submitting}
                />
              </ConnField>
            </ConnForm>

            <SecretsFieldset>
              {secrets.map((s, idx) => (
                <div key={idx} className="mb-2 flex items-center gap-2">
                  <input
                    className={SETTINGS_INPUT}
                    type="text"
                    placeholder="ENV_KEY"
                    aria-label="Secret key"
                    value={s.key}
                    onChange={(e) => updateSecret(idx, { key: e.target.value })}
                  />
                  <input
                    // WRITE-ONLY: a password field that always starts empty. The
                    // app never reads a stored secret back into here.
                    className={SETTINGS_INPUT}
                    type="password"
                    autoComplete="new-password"
                    placeholder="value (never shown again)"
                    aria-label="Secret value"
                    data-testid={`credential-secret-value-${idx}`}
                    value={s.value}
                    onChange={(e) => updateSecret(idx, { value: e.target.value })}
                  />
                  {secrets.length > 1 ? (
                    <button
                      type="button"
                      aria-label="Remove secret"
                      onClick={() => removeSecretRow(idx)}
                      className="inline-flex size-[2.4rem] shrink-0 items-center justify-center rounded-[var(--radius-md)] border border-[var(--color-border)] text-[var(--color-text-muted)] hover:border-[var(--color-danger)] hover:text-[var(--color-danger)]"
                    >
                      <Icon name="trash" className="size-[0.95rem]" />
                    </button>
                  ) : null}
                </div>
              ))}
              <button
                type="button"
                onClick={addSecretRow}
                className="mt-[0.2rem] inline-flex items-center gap-[0.35rem] py-[0.3rem] text-[0.85rem] font-medium text-[var(--color-accent)]"
              >
                <Icon name="plus" className="size-[0.95rem]" /> Add secret
              </button>
            </SecretsFieldset>

            <ConnFormActions>
              <button
                type="button"
                className={btnClass({ variant: "primary" })}
                disabled={submitting || !account.trim()}
                onClick={() => void submit()}
              >
                {submitting ? "Saving…" : "Save account"}
              </button>
            </ConnFormActions>
          </div>
        ) : null}
      </ConnPanel>
    </div>
  );
}

export default CredentialAccountAdmin;
