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
// The ids are the point. A name-only refusal was a dead end: it told the admin
// to transfer the projects and gave them no route to the control that does it,
// and the name could not be resolved to an id from this page either —
// `GET /projects` is scoped to the *caller's* own and team-visible projects,
// and an admin is usually neither the owner nor a member of the project they
// are being asked to have transferred. So each project links straight at its
// own settings dialog, where the collapsed "Transfer ownership…" control lives
// (`GET /projects/{id}/members` backs its picker).
//
// The prose is still parsed as a FALLBACK, for a server that predates the
// structured body: names without ids still render, as text rather than links.
// Better a refusal that explains itself than one replaced by "Delete failed
// (409)."

import Link from "next/link";

import { NoticeBanner } from "@/app/shared/ui/NoticeBanner";

// One project's identity as the refusal reports it.
export type OwnedSharedProject = { id: string; name: string };

// The deep link chat reads on boot (chat-experience.tsx): open this project's
// home with its settings dialog already open — the surface holding "Transfer
// ownership…". `settings=1` is what distinguishes it from merely opening the
// project.
export function transferHref(projectID: string): string {
  return `/chat?project=${encodeURIComponent(projectID)}&settings=1`;
}

// Where a name-only refusal (a pre-structured-body server) can honestly point:
// the Projects surface, with the remaining manual step spelled out.
export const PROJECTS_SURFACE_HREF = "/chat";

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
// unbroken token), plus a "Transfer <project>" link per project the fail-closed
// guard named.
export function DeleteRefusal({
  message,
  projects,
}: {
  message: string;
  // Absent (older server, or an unrelated failure) → fall back to the names in
  // the prose, which link nowhere but still name what to transfer.
  projects?: OwnedSharedProject[];
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
        <>
          <p className="m-0 flex flex-wrap gap-x-[0.6rem] gap-y-[0.2rem]">
            {linked.map((p) => (
              <Link
                key={p.id}
                href={transferHref(p.id)}
                title={`Open ${p.name} → project settings → Transfer ownership…`}
                className="text-[var(--color-danger-soft)] underline underline-offset-2 [overflow-wrap:anywhere]"
              >
                Transfer {p.name}
              </Link>
            ))}
          </p>
          <p className="m-0 text-[var(--color-text-muted)] [overflow-wrap:anywhere]">
            Opens the project&rsquo;s settings, where Transfer ownership lives.
          </p>
        </>
      ) : namesOnly.length > 0 ? (
        <>
          <p className="m-0 flex flex-wrap gap-x-[0.6rem] gap-y-[0.2rem]">
            {namesOnly.map((name) => (
              <Link
                key={name}
                href={PROJECTS_SURFACE_HREF}
                title={`Open Projects in chat, then ${name} → project settings → Transfer ownership…`}
                className="text-[var(--color-danger-soft)] underline underline-offset-2 [overflow-wrap:anywhere]"
              >
                Transfer {name}
              </Link>
            ))}
          </p>
          <p className="m-0 text-[var(--color-text-muted)] [overflow-wrap:anywhere]">
            Opens Projects in chat — the transfer control is in that
            project&rsquo;s settings dialog.
          </p>
        </>
      ) : null}
    </NoticeBanner>
  );
}
