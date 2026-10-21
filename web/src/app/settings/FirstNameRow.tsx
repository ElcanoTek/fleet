"use client";

// Settings → General → "Your first name" (ADR-0087). The name a connector
// greets you by — the deal-sheet email's "Hi Elyse," — reaches it as the
// bundle token ${FLEET_USER_NAME}. Fleet's sign-in carries an email and no
// name, and a name guessed from an email is often wrong, so this saved value is
// the only source: blank means connectors say "Hi team,".
//
// Stored server-side per account (PUT /api/me/name → users.given_name), so it
// follows the user across browsers. The server applies the character rule and
// its 400 text is shown as-is.

import { useCallback, useEffect, useState } from "react";

import { btnClass, SETTINGS_INPUT, ActStatus } from "./ui/atoms";

type MeName = { given_name?: string };

export function FirstNameRow() {
  const [saved, setSaved] = useState<string | null>(null);
  const [draft, setDraft] = useState("");
  const [busy, setBusy] = useState(false);
  const [status, setStatus] = useState<{ state: "ok" | "err"; text: string } | null>(null);

  const load = useCallback(async () => {
    try {
      const res = await fetch("/api/me/name", { cache: "no-store" });
      if (!res.ok) throw new Error(await readError(res, `Couldn’t read your name (${res.status}).`));
      const me = (await res.json()) as MeName;
      const name = me.given_name ?? "";
      setSaved(name);
      setDraft(name);
    } catch (err) {
      setStatus({ state: "err", text: err instanceof Error ? err.message : "Couldn’t read your name." });
    }
  }, []);

  useEffect(() => {
    // queueMicrotask keeps the first setState out of the effect body itself
    // (react-hooks/set-state-in-effect), the same shape Settings → Team uses.
    let cancelled = false;
    queueMicrotask(() => {
      if (!cancelled) void load();
    });
    return () => {
      cancelled = true;
    };
  }, [load]);

  const save = async () => {
    const value = draft.trim();
    setBusy(true);
    setStatus(null);
    try {
      const res = await fetch("/api/me/name", {
        method: "PUT",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ given_name: value }),
      });
      if (res.status === 401) {
        window.location.href = "/login";
        return;
      }
      if (!res.ok) throw new Error(await readError(res, `Save failed (${res.status}).`));
      const me = (await res.json()) as MeName;
      const name = me.given_name ?? "";
      setSaved(name);
      setDraft(name);
      setStatus({ state: "ok", text: name ? "Saved." : "Cleared — connectors will say “Hi team,”." });
    } catch (err) {
      setStatus({ state: "err", text: err instanceof Error ? err.message : "Save failed." });
    } finally {
      setBusy(false);
    }
  };

  const dirty = saved !== null && draft.trim() !== saved;

  return (
    <div className="flex flex-col gap-[0.55rem] border-b border-[var(--color-border-subtle)] py-4 last:border-b-0">
      <div className="grid gap-[0.3rem]">
        <label htmlFor="firstName" className="text-[0.9rem] font-semibold text-[var(--color-text-primary)]">
          Your first name
        </label>
        <span className="max-w-[32rem] text-[0.78rem] leading-[1.5] text-[var(--color-text-muted)]">
          What connectors call you, for example the “Hi Elyse,” at the top of a deal-sheet email.
          Leave it blank and they say “Hi team,” instead. Fleet never guesses it from your email.
        </span>
      </div>
      <div className="flex flex-wrap items-center gap-[0.55rem]">
        <input
          id="firstName"
          className={`${SETTINGS_INPUT} max-w-[18rem] flex-1`}
          placeholder="Not set"
          maxLength={64}
          value={draft}
          disabled={busy || saved === null}
          onChange={(e) => {
            setDraft(e.target.value);
            setStatus(null);
          }}
          onKeyDown={(e) => {
            if (e.key === "Enter" && dirty && !busy) void save();
          }}
        />
        <button
          type="button"
          className={btnClass({ variant: "primary", sm: true })}
          disabled={busy || !dirty}
          onClick={() => void save()}
        >
          {busy ? "Saving…" : "Save"}
        </button>
        {status ? <ActStatus state={status.state}>{status.text}</ActStatus> : null}
      </div>
    </div>
  );
}

async function readError(res: Response, fallback: string): Promise<string> {
  const text = (await res.text()).trim();
  return text.length > 0 && text.length <= 300 ? text : fallback;
}
