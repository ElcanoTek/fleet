"use client";

import { useEffect, useState } from "react";
import type { McpServer } from "@/app/shared/lib/orchestratorApi";
import { orchestratorApi } from "@/app/shared/lib/orchestratorApi";
import { useToast } from "@/app/shared/ui/Toast";

// CredentialAccountAdmin — manage MCP credential accounts per server.
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
  // When this admin renders inside the Settings modal, the modal's fixed footer
  // owns the primary "Save account" action: it hides the inline button
  // (hideSubmit), registers our submit handler so the footer can trigger it
  // (registerSubmit), and mirrors our busy state to label/disable that button
  // (onBusyChange). Rendered standalone, none are passed and the inline button
  // shows as before.
  hideSubmit?: boolean;
  registerSubmit?: (submit: () => void) => void;
  onBusyChange?: (busy: boolean) => void;
};

type SecretField = { key: string; value: string };

export function CredentialAccountAdmin({
  servers,
  onChanged,
  hideSubmit,
  registerSubmit,
  onBusyChange,
}: CredentialAccountAdminProps) {
  const { showToast } = useToast();
  const [server, setServer] = useState<string>(servers[0]?.name ?? "");
  const [account, setAccount] = useState<string>("");
  const [secrets, setSecrets] = useState<SecretField[]>([{ key: "", value: "" }]);
  const [submitting, setSubmitting] = useState(false);

  const updateSecret = (idx: number, patch: Partial<SecretField>) => {
    setSecrets((prev) => prev.map((s, i) => (i === idx ? { ...s, ...patch } : s)));
  };

  const addSecretRow = () => setSecrets((prev) => [...prev, { key: "", value: "" }]);

  const reset = () => {
    setAccount("");
    setSecrets([{ key: "", value: "" }]);
  };

  const submit = async () => {
    if (!server || !account.trim()) {
      showToast("Server and account name are required", "error");
      return;
    }
    // Only forward secrets that have BOTH a key and a (non-empty) value.
    // Empty values are dropped, never written as "" — write-only semantics.
    const payload: Record<string, string> = {};
    for (const { key, value } of secrets) {
      if (key.trim() && value !== "") payload[key.trim()] = value;
    }
    if (Object.keys(payload).length === 0) {
      showToast("Add at least one secret KEY=value", "error");
      return;
    }
    setSubmitting(true);
    try {
      await orchestratorApi.createAccount(server, { account: account.trim(), secrets: payload });
      showToast(`Saved account "${account.trim()}" for ${server}`, "success");
      reset();
      onChanged?.();
    } catch (err) {
      showToast(`Failed to save account: ${(err as Error).message}`, "error");
    } finally {
      setSubmitting(false);
    }
  };

  const remove = async (srv: string, acct: string) => {
    setSubmitting(true);
    try {
      await orchestratorApi.deleteAccount(srv, acct);
      showToast(`Removed account "${acct}" from ${srv}`, "success");
      onChanged?.();
    } catch (err) {
      showToast(`Failed to remove account: ${(err as Error).message}`, "error");
    } finally {
      setSubmitting(false);
    }
  };

  // Re-register submit on every render so the modal footer always invokes the
  // latest closure (it captures the current server/account/secrets state).
  useEffect(() => {
    registerSubmit?.(() => {
      void submit();
    });
  });

  // Mirror busy state so the modal footer button can label/disable itself.
  useEffect(() => {
    onBusyChange?.(submitting);
  }, [submitting, onBusyChange]);

  return (
    <div className="credential-account-admin" data-testid="credential-account-admin">
      <h3>MCP Credential Accounts</h3>
      <p className="advanced-setting-meta">
        Secret values are write-only — they are never displayed after saving.
      </p>

      {/* Existing accounts: names ONLY, never secret values. */}
      <ul className="credential-account-list">
        {servers.flatMap((srv) =>
          (srv.accounts ?? []).map((acct) => (
            <li key={`${srv.name}:${acct}`} data-testid={`credential-account-${srv.name}-${acct}`}>
              <span className="credential-account-name">
                {srv.name} / {acct}
              </span>
              <button
                type="button"
                className="btn btn-ghost"
                aria-label={`Delete ${srv.name} account ${acct}`}
                disabled={submitting}
                onClick={() => void remove(srv.name, acct)}
              >
                Delete
              </button>
            </li>
          )),
        )}
      </ul>

      <div className="credential-account-form">
        <div className="form-group">
          <label htmlFor="credAdminServer">Server</label>
          <select
            id="credAdminServer"
            value={server}
            onChange={(e) => setServer(e.target.value)}
          >
            {servers.map((s) => (
              <option key={s.name} value={s.name}>
                {s.name}
              </option>
            ))}
          </select>
        </div>

        <div className="form-group">
          <label htmlFor="credAdminAccount">Account name</label>
          <input
            id="credAdminAccount"
            type="text"
            placeholder="e.g. client_a"
            value={account}
            onChange={(e) => setAccount(e.target.value)}
          />
        </div>

        <fieldset className="credential-account-secrets">
          <legend>Secrets (write-only)</legend>
          {secrets.map((s, idx) => (
            <div key={idx} className="credential-account-secret-row">
              <input
                type="text"
                placeholder="ENV_KEY"
                aria-label="Secret key"
                value={s.key}
                onChange={(e) => updateSecret(idx, { key: e.target.value })}
              />
              <input
                // WRITE-ONLY: a password field that always starts empty. The
                // app never reads a stored secret back into here.
                type="password"
                autoComplete="new-password"
                placeholder="value (never shown again)"
                aria-label="Secret value"
                data-testid={`credential-secret-value-${idx}`}
                value={s.value}
                onChange={(e) => updateSecret(idx, { value: e.target.value })}
              />
            </div>
          ))}
          <button type="button" className="btn btn-ghost" onClick={addSecretRow}>
            + Add secret
          </button>
        </fieldset>

        {hideSubmit ? null : (
          <button
            type="button"
            className="btn btn-primary"
            disabled={submitting}
            onClick={() => void submit()}
          >
            {submitting ? "Saving…" : "Save account"}
          </button>
        )}
      </div>
    </div>
  );
}

export default CredentialAccountAdmin;
