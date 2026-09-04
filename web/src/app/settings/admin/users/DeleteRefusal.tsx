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
// The 409 body is PLAIN TEXT, and it carries project NAMES only:
//
//   this account still owns team-shared projects (alpha, beta) — transfer them
//   to another member first, then delete the account
//
// (`internal/httpapi/admin_users.go` joins `store.OwnsSharedProjectsError.Projects`,
// which `store.TeamSharedProjectsOwnedBy` selects as `name`.) With no project
// **id** in the response there is no direct link to build: every transfer
// surface is keyed by id (`GET /projects/{id}/members` backs the picker, and the
// collapsed "Transfer ownership…" control lives in that project's settings
// dialog on the project home). Resolving name → id from this page is not
// possible either — `GET /projects` is scoped to the *caller's* own and
// team-visible projects, and an admin is usually neither the owner nor a member
// of the project they are being asked to have transferred.
//
// So the link is honest about what it can do: it opens the Projects surface in
// chat, one link per named project, and says the one manual step left. A direct
// deep link needs two things this repo does not have yet, both noted rather
// than faked: the 409 body would have to carry `{id, name}` pairs (a JSON body,
// or the names plus ids), and chat would need a deep link that opens a
// project's settings dialog (the only param it reads today is `?c=`).

import Link from "next/link";

import { NoticeBanner } from "@/app/shared/ui/NoticeBanner";

// Where a "Transfer <project>" link can honestly point today: the Projects
// surface in chat, which is where the per-project settings dialog (and its
// collapsed "Transfer ownership…" control) is reached from.
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
export function DeleteRefusal({ message }: { message: string }) {
  const projects = parseOwnedSharedProjects(message);
  return (
    <NoticeBanner
      tone="danger"
      role="alert"
      className="grid gap-[0.35rem] px-[0.6rem]! py-[0.5rem]! text-[0.72rem]! leading-[1.45]"
    >
      <p className="m-0 [overflow-wrap:anywhere]">{message}</p>
      {projects.length > 0 ? (
        <>
          <p className="m-0 flex flex-wrap gap-x-[0.6rem] gap-y-[0.2rem]">
            {projects.map((name) => (
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
