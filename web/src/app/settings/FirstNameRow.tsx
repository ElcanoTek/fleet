"use client";

// Settings → General → "Your first name" (ADR-0087). Connectors that address
// the user by name (an email that opens "Hi Sam,", say) receive it as the
// bundle token ${FLEET_USER_NAME}. Fleet's sign-in carries an email and no
// name, and a name guessed from an email is often wrong, so this saved value
// is the only source: blank means the token is unset and each connector uses
// its own no-name wording.
//
// Stored server-side per account (PUT /api/me/name → users.given_name), so it
// follows the user across browsers. The server applies the character rule and
// its refusal text is shown as-is.

import { useCallback, useEffect, useState } from "react";

import { btnClass, SETTINGS_INPUT, ActStatus } from "./ui/atoms";

type MeName = { given_name?: string };

export function FirstNameRow() {
  const [saved, setSaved] = useState<string | null>(null);
  const [draft, setDraft] = useState("");
  const [busy, setBusy] = useState(false);
  const [loadFailed, setLoadFailed] = useState(false);
  const [status, setStatus] = useState<{ state: "ok" | "err"; text: string } | null>(null);

  const load = useCallback(async () => {
    setLoadFailed(false);
    setStatus(null);
    try {
      const res = await fetch("/api/me/name", { cache: "no-store" });
      if (res.status === 401) {
        window.location.href = "/login";
        return;
      }
      if (!res.ok) throw new Error(await readError(res, `Couldn’t read your name (${res.status}).`));
      const me = (await res.json()) as MeName;
      const name = me.given_name ?? "";
      setSaved(name);
      setDraft(name);
    } catch (err) {
      setLoadFailed(true);
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
      if (res.status === 403) {
        throw new Error("Your account is read-only, so it can’t change this.");
      }
      if (!res.ok) throw new Error(await readError(res, `Save failed (${res.status}).`));
      const me = (await res.json()) as MeName;
      const name = me.given_name ?? "";
      setSaved(name);
      setDraft(name);
      setStatus({ state: "ok", text: name ? "Saved." : "Cleared." });
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
          Connectors that address you by name use this, for example an email that opens
          “Hi Sam,”. Leave it blank and they use their own wording instead. Fleet never
          guesses it from your email address.
        </span>
      </div>
      <div className="flex flex-wrap items-center gap-[0.55rem]">
        <input
          id="firstName"
          className={`${SETTINGS_INPUT} max-w-[18rem] flex-1`}
          placeholder="Not set"
          // The server's limit is 64 characters; a browser counts UTF-16 code
          // units, so this cap leaves room for characters outside the BMP.
          maxLength={128}
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
        {loadFailed ? (
          <button type="button" className={btnClass({ sm: true })} onClick={() => void load()}>
            Retry
          </button>
        ) : (
          <button
            type="button"
            className={btnClass({ variant: "primary", sm: true })}
            disabled={busy || !dirty}
            onClick={() => void save()}
          >
            {busy ? "Saving…" : "Save"}
          </button>
        )}
        {status ? <ActStatus state={status.state}>{status.text}</ActStatus> : null}
      </div>
    </div>
  );
}

// readError returns the server's refusal text: a plain-text body as-is, or the
// "error" field of a JSON body (the web tier's own 4xx shape), else fallback.
async function readError(res: Response, fallback: string): Promise<string> {
  const text = (await res.text()).trim();
  if (text.startsWith("{")) {
    try {
      const parsed = JSON.parse(text) as { error?: unknown };
      return typeof parsed.error === "string" && parsed.error ? parsed.error : fallback;
    } catch {
      return fallback;
    }
  }
  return text.length > 0 && text.length <= 300 ? text : fallback;
}
