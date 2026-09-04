"use client";

// The delete-refusal notice for Settings → Admin → Users.
//
// `DELETE /admin/users/{email}` fails closed with a 409 when the account still
// owns team-shared projects: deleting it would take those projects — and every
// team learning in them — from people who are still here (ADR-0057, and
// docs/TEAM-SHARING.md "Ownership transfer"). The refusal is a *missing step*,
// not a failure, so it renders where the admin took the action — inside the
// row-edit panel, directly under Confirm delete — and names the next step.
//
// The 409 body is JSON:
//
//   {"error": "this account still owns team-shared projects (alpha, beta) —
//             transfer them to another member first, then delete the account",
//    "owns_shared_projects": [{"id": "…", "name": "alpha"}, …]}
//
// The ids are the point, and the transfer happens RIGHT HERE rather than
// through a link. A link cannot work for the caller this is written for: an
// admin is usually neither the project's owner nor a member of its team, so
// every membership-gated surface — the chat rail's project list, the project
// home — answers them with a 404, and a "Transfer alpha" link would land on a
// page that cannot show the project, let alone its transfer control. The two
// routes that DO authorize an admin (GET /projects/{id}/members for the
// picker, POST /projects/{id}/transfer for the handover) are called from this
// panel directly, so the refusal carries its own resolution.
//
// The prose is still parsed as a FALLBACK, for a server that predates the
// structured body: names without ids render as plain text with the manual
// route spelled out. Better a refusal that explains itself than one replaced
// by "Delete failed (409)."

import { useCallback, useState } from "react";

import { NoticeBanner } from "@/app/shared/ui/NoticeBanner";

// One project's identity as the refusal reports it.
export type OwnedSharedProject = { id: string; name: string };

// The parenthesised list in the 409 body. Two known limits, both inherent to
// parsing a prose sentence rather than a structured body: a project name
// containing ")" truncates the list, and one containing ", " splits into two.
// Both disappear the moment the API returns the projects as data.
const REFUSAL_PROJECTS_RE = /still owns team-shared projects \(([^)]+)\)/;

// parseOwnedSharedProjects extracts the project names the refusal blames, or
// [] for any other error text (a plain "Delete failed (500)." renders as a
// message with no transfer links, which is correct — there is nothing to
// transfer).
export function parseOwnedSharedProjects(message: string): string[] {
  const match = REFUSAL_PROJECTS_RE.exec(message);
  if (!match) return [];
  return match[1]
    .split(", ")
    .map((name) => name.trim())
    .filter((name) => name.length > 0);
}

// DeleteRefusal renders any delete failure for one account: the server's
// sentence, wrapped (`overflow-wrap: anywhere` — a project name can be one long
// unbroken token), plus one inline transfer control per project the fail-closed
// guard named.
export function DeleteRefusal({
  message,
  projects,
  currentOwner,
  onTransferred,
}: {
  message: string;
  // Absent (older server, or an unrelated failure) → fall back to the names in
  // the prose. Those name what to transfer but cannot offer to do it: without
  // an id there is no route to call.
  projects?: OwnedSharedProject[];
  // The account being deleted — i.e. the projects' current owner, who is
  // always in the member list and must not be offered as its own successor:
  // handing the project back would be a no-op that leaves the delete blocked.
  currentOwner?: string;
  // Called after a successful handover, so the panel can retry the delete —
  // the transfer exists here only to unblock it.
  onTransferred?: (project: OwnedSharedProject) => void;
}) {
  const linked = projects ?? [];
  const namesOnly = linked.length > 0 ? [] : parseOwnedSharedProjects(message);
  return (
    <NoticeBanner
      tone="danger"
      role="alert"
      className="grid gap-[0.35rem] px-[0.6rem]! py-[0.5rem]! text-[0.72rem]! leading-[1.45]"
    >
      <p className="m-0 [overflow-wrap:anywhere]">{message}</p>
      {linked.length > 0 ? (
        <div className="grid gap-[0.3rem]">
          {linked.map((p) => (
            <TransferProject
              key={p.id}
              project={p}
              currentOwner={currentOwner}
              onTransferred={onTransferred}
            />
          ))}
        </div>
      ) : namesOnly.length > 0 ? (
        <p className="m-0 text-[var(--color-text-muted)] [overflow-wrap:anywhere]">
          Transfer {namesOnly.join(", ")} from the project&rsquo;s settings
          dialog, then delete the account.
        </p>
      ) : null}
    </NoticeBanner>
  );
}

// TransferProject is the refusal's resolution, inline: pick a new owner, hand
// the project over, and the caller retries the delete.
//
// The member list is fetched lazily — only when the admin opens this control —
// because it enumerates every account in the project's team, and an admin
// reading a refusal has not yet asked for that.
function TransferProject({
  project,
  currentOwner,
  onTransferred,
}: {
  project: OwnedSharedProject;
  currentOwner?: string;
  onTransferred?: (project: OwnedSharedProject) => void;
}) {
  const [open, setOpen] = useState(false);
  const [members, setMembers] = useState<string[] | null>(null);
  const [choice, setChoice] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const load = useCallback(async () => {
    setError(null);
    try {
      const res = await fetch(
        `/api/projects/${encodeURIComponent(project.id)}/members`,
        { cache: "no-store" },
      );
      if (!res.ok) {
        setMembers([]);
        setError(`Couldn't load candidates (${res.status}).`);
        return;
      }
      const body = (await res.json()) as { members?: string[] };
      setMembers(body.members ?? []);
    } catch {
      setMembers([]);
      setError("Couldn't load candidates — network error.");
    }
  }, [project.id]);

  // The current owner is always in the list (ProjectMemberEmails unions them
  // in), and handing the project back to them is a no-op that leaves the
  // delete blocked — so they are not offered.
  const owner = (currentOwner ?? "").trim().toLowerCase();
  const candidates = (members ?? []).filter(
    (m) => m.trim().toLowerCase() !== owner,
  );

  const transfer = async () => {
    if (!choice) return;
    setBusy(true);
    setError(null);
    try {
      const res = await fetch(
        `/api/projects/${encodeURIComponent(project.id)}/transfer`,
        {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ to_email: choice }),
        },
      );
      if (!res.ok) {
        const text = (await res.text()).trim();
        setError(text.length > 0 && text.length <= 300 ? text : `Transfer failed (${res.status}).`);
        return;
      }
      onTransferred?.(project);
    } catch {
      setError("Transfer failed — network error.");
    } finally {
      setBusy(false);
    }
  };

  if (!open) {
    return (
      <button
        type="button"
        className="justify-self-start text-[var(--color-danger-soft)] underline underline-offset-2 [overflow-wrap:anywhere]"
        onClick={() => {
          setOpen(true);
          // Fetched from the event that opens the control, not from an effect
          // watching `open`: the list is external state this click is asking
          // for, and reacting to our own state change would just add a render.
          if (members === null) void load();
        }}
      >
        Transfer {project.name}
      </button>
    );
  }
  return (
    <div className="grid gap-[0.25rem]">
      <label className="grid gap-[0.2rem]">
        <span className="[overflow-wrap:anywhere]">
          Hand {project.name} to
        </span>
        <select
          className="w-full max-w-[18rem] rounded-[var(--radius-sm)] border border-[var(--color-border-strong)] bg-[var(--color-surface-1)] px-[0.4rem] py-[0.25rem] text-[0.72rem]"
          value={choice}
          disabled={busy || members === null}
          onChange={(e) => setChoice(e.target.value)}
        >
          <option value="">
            {members === null ? "Loading…" : "Choose a member…"}
          </option>
          {candidates.map((m) => (
            <option key={m} value={m}>
              {m}
            </option>
          ))}
        </select>
      </label>
      <div className="flex flex-wrap items-center gap-[0.5rem]">
        <button
          type="button"
          className="text-[var(--color-danger-soft)] underline underline-offset-2 disabled:opacity-50"
          disabled={busy || !choice}
          onClick={() => void transfer()}
        >
          {busy ? "Transferring…" : "Transfer"}
        </button>
        <button
          type="button"
          className="text-[var(--color-text-muted)] underline underline-offset-2"
          onClick={() => setOpen(false)}
        >
          Cancel
        </button>
      </div>
      {members !== null && members.length === 0 && !error ? (
        <p className="m-0 text-[var(--color-text-muted)] [overflow-wrap:anywhere]">
          No one else is in this project&rsquo;s team — add a member first, or
          make the project personal.
        </p>
      ) : null}
      {error ? (
        <p className="m-0 [overflow-wrap:anywhere]">{error}</p>
      ) : null}
    </div>
  );
}
