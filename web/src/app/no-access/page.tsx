// Shown when a user has a valid elcano_auth cookie (their identity is proven
// by the auth service) but their email isn't on chat's user-list yet. They're
// signed in, just not authorized for chat — so this is a dead-end with a way
// out, not a redirect back to login. The real boundary is chat-server, which
// 403s every API call for non-members; this page just makes that legible.
export default function NoAccessPage() {
  return (
    <main className="flex min-h-screen items-center justify-center bg-[var(--gradient-bg-home-signature)] px-6 py-10">
      <div className="w-full max-w-sm rounded-[1.5rem] border border-[var(--color-border)] bg-[var(--composer-surface)] p-6 shadow-[var(--composer-shadow)]">
        <h1 className="text-[1.25rem] font-semibold text-[var(--color-text-primary)]">No access yet</h1>
        <p className="mt-2 text-[0.875rem] text-[var(--color-text-secondary)]">
          You&rsquo;re signed in with your Elcano email, but this account
          isn&rsquo;t on chat&rsquo;s access list yet. Ask an admin to add you,
          then sign in again.
        </p>

        <form action="/api/auth/logout" method="post" className="mt-6">
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
