"use client";

import { useCallback, useRef, useState } from "react";
import type { McpServer } from "@/app/shared/lib/orchestratorApi";
import { useDialogA11y } from "@/app/shared/ui/useDialogA11y";
import { ConcurrencyCapSetting } from "./ConcurrencyCapSetting";
import { CredentialAccountAdmin } from "./CredentialAccountAdmin";

// SettingsModal — the Operations Center settings, lifted out of the old inline
// page section into a modal that shares the New Task modal's chrome (#169
// design handoff): overlay, header, scrolling body, dividers, and a fixed
// footer. Section-level saves stay inline with their field (the concurrency cap
// keeps its own Save); the modal's footer owns the single primary action — the
// Credential "Save account" button — moved out of the body and right-aligned,
// hugged to its content (never full-width).

export type SettingsModalProps = {
  open: boolean;
  servers: McpServer[];
  onClose: () => void;
  onChanged?: () => void;
};

export function SettingsModal({ open, servers, onClose, onChanged }: SettingsModalProps) {
  const modalRef = useRef<HTMLDivElement | null>(null);
  // The footer's Save button triggers CredentialAccountAdmin's submit, which it
  // registers here; busy mirrors that admin's in-flight state.
  const submitRef = useRef<(() => void) | null>(null);
  const [busy, setBusy] = useState(false);

  useDialogA11y(open, modalRef, onClose);

  const registerSubmit = useCallback((fn: () => void) => {
    submitRef.current = fn;
  }, []);

  if (!open) return null;

  return (
    <div
      className="modal-overlay is-open"
      role="dialog"
      aria-modal="true"
      aria-label="Settings"
      data-testid="settings-modal"
    >
      <div className="modal modal-lg" ref={modalRef} tabIndex={-1}>
        <div className="modal-header">
          <h3>Settings</h3>
          <button type="button" className="icon-action modal-close" aria-label="Close modal" onClick={onClose}>
            ×
          </button>
        </div>
        <div className="modal-body" data-testid="settings-section">
          <ConcurrencyCapSetting />
          <div className="settings-divider" />
          <CredentialAccountAdmin
            servers={servers}
            onChanged={onChanged}
            hideSubmit
            registerSubmit={registerSubmit}
            onBusyChange={setBusy}
          />
        </div>
        <div className="modal-footer">
          <button
            type="button"
            className="btn btn-primary"
            disabled={busy}
            onClick={() => submitRef.current?.()}
          >
            {busy ? "Saving…" : "Save account"}
          </button>
        </div>
      </div>
    </div>
  );
}

export default SettingsModal;
