import Link from "next/link";
// Shown when a user has a valid session minted elsewhere (the shared
// elcano_auth cookie, or central Auth via OIDC) but their email isn't on
// this deployment's user-list yet. They're signed in, just not a member — so
// this is a dead-end with a way out (the same sign-out every surface uses,
// which also ends a central-Auth session), not a redirect back to login. The
// real boundary is chat-server, which 403s every API call for non-members;
// this page just makes that legible.
export default function NoAccessPage() {
  return (
    <main className="flex min-h-screen items-center justify-center bg-[var(--gradient-bg-home-signature)] px-6 py-10">
      <div className="w-full max-w-sm rounded-[1.5rem] border border-[var(--color-border)] bg-[var(--composer-surface)] p-6 shadow-[var(--composer-shadow)]">
        <h1 className="text-[1.25rem] font-semibold text-[var(--color-text-primary)]">
          No access yet
        </h1>
        <p className="mt-2 text-[0.875rem] text-[var(--color-text-secondary)]">
          You&rsquo;re signed in, but this account hasn&rsquo;t been added to
          this workspace yet. Ask an administrator to add you, then sign in
          again.
        </p>

        <Link
          href="/chat"
          className="mt-6 block w-full rounded-xl bg-[var(--color-primary)] px-4 py-2.5 text-center text-sm font-medium text-[var(--color-on-primary)] transition hover:bg-[var(--color-primary-hover)] focus-visible:outline-none focus-visible:[box-shadow:var(--focus-ring)]"
        >
          Try again
        </Link>
        <form action="/api/auth/logout" method="post" className="mt-3">
          <button
            type="submit"
            className="w-full rounded-xl border border-[var(--color-border)] px-4 py-2.5 text-sm font-medium text-[var(--color-text-primary)] transition hover:bg-[var(--color-overlay-soft)] focus-visible:outline-none focus-visible:[box-shadow:var(--focus-ring)]"
          >
            Sign out
          </button>
        </form>
      </div>
    </main>
  );
}
