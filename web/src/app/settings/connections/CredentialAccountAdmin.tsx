import { rich } from "@/app/lib/i18n/rich";
import { t as tr } from "@/app/lib/i18n";
import { useState } from "react";
import type { McpServer } from "@/app/shared/lib/orchestratorApi";
import { Icon } from "@/app/shared/ui/Icon";
import { btnClass } from "../ui/atoms";
import { ConnEmpty, ConnPanel, ConnPanelHead, ConnRow, ConnRows } from "../ui/panels";

// CredentialAccountAdmin — the "Accounts" panel under Settings → Connections →
// Credential accounts: the MCP credential-account seats the server has
// provisioned, per connector.
//
// READ-ONLY by design. The list is the account NAMES from the orchestrator's
// /mcp-servers catalog — secret values are never returned by any endpoint, so
// nothing here can echo one. Accounts are not added or removed from the
// browser: their secrets live host-side in the server env file as suffixed
// keys (<VAR>_<ACCOUNT>), which the credential broker reads at boot, so an
// operator provisions them on the server with `fleet mcp account set` and
// restarts fleet (docs/OPERATORS.md). The add/delete form this panel used to
// carry posted to a moc-only orchestrator route fleet never served, so every
// save and delete answered 404.

export type CredentialAccountAdminProps = {
  servers: McpServer[];
};

export function CredentialAccountAdmin({ servers }: CredentialAccountAdminProps) {
  const [accountsExpanded, setAccountsExpanded] = useState(false);

  const accountRows = servers.flatMap((srv) =>
    (srv.accounts ?? []).map((acct) => ({ srv: srv.name, acct })),
  );

  return (
    <div data-testid="credential-account-admin">
      <ConnPanel>
        <ConnPanelHead title={tr("Accounts")} />

        {/* Existing accounts: names ONLY, never secret values. */}
        {accountRows.length > 0 ? (
          <div id="credential-account-list">
            <ConnRows>
              {(accountsExpanded ? accountRows : accountRows.slice(0, 4)).map(({ srv, acct }) => (
                <ConnRow
                  key={`${srv}:${acct}`}
                  name={
                    <span data-testid={`credential-account-${srv}-${acct}`}>
                      <code>{srv}</code> / {acct}
                    </span>
                  }
                />
              ))}
            </ConnRows>
          </div>
        ) : (
          <ConnEmpty>{tr("No accounts yet.")}</ConnEmpty>
        )}

        {accountRows.length > 4 ? (
          <button
            type="button"
            aria-expanded={accountsExpanded}
            aria-controls="credential-account-list"
            onClick={() => setAccountsExpanded((expanded) => !expanded)}
            className={`${btnClass({ sm: true })} mt-3`}
          >
            {accountsExpanded ? tr("Show less") : tr("Show all ({0})", [accountRows.length])}
            <Icon name="chevron-down" className={`size-3.5 transition-transform ${accountsExpanded ? "rotate-180" : ""}`} />
          </button>
        ) : null}

        <p
          className="mt-3 mb-0 text-[0.85rem] text-[var(--color-text-muted)]"
          data-testid="credential-account-howto"
        >
        {rich("Your administrator adds and removes accounts on the fleet server — for example {0}, then a restart. Secrets stay on the server and are never shown here.", <code>fleet mcp account set &lt;server&gt; &lt;account&gt; --secret KEY=-</code>)}
      </p>
      </ConnPanel>
    </div>
  );
}

export default CredentialAccountAdmin;
