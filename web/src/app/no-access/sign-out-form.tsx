"use client";

import { signOut } from "@/app/shared/signOut";

// The no-access page's way out, through the one signOut every other surface
// uses, so it lets go of this browser's push subscription before it posts the
// logout form. A non-member cannot turn notifications on, so a subscription
// this browser holds was made by an earlier account whose session ended
// without a sign-out (it lapsed, or was revoked), or by this one before it
// lost its membership; posting the form directly left those alerts arriving
// here. The server refuses a non-member's delete, but the browser's own
// unsubscribe is what stops delivery, and the relay's 404/410 then retires
// the row. It stays a real form posting to the same route, so a click before
// the page has hydrated still signs out, without the clean-up.
export function SignOutForm() {
  return (
    <form
      action="/api/auth/logout"
      method="post"
      className="mt-3"
      onSubmit={(event) => {
        event.preventDefault();
        void signOut();
      }}
    >
      <button
        type="submit"
        className="w-full rounded-xl border border-[var(--color-border)] px-4 py-2.5 text-sm font-medium text-[var(--color-text-primary)] transition hover:bg-[var(--color-overlay-soft)] focus-visible:outline-none focus-visible:[box-shadow:var(--focus-ring)]"
      >
        Sign out
      </button>
    </form>
  );
}
