import { getLanguage, t as tr } from "@/app/lib/i18n";
import { Link } from "@/app/lib/Link";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { SignOutForm } from "./sign-out-form";

export default function NoAccessPage() {
  return (
    <main className="flex min-h-screen items-center justify-center bg-[var(--gradient-bg-home-signature)] px-6 py-10">
      <Card className="block w-full max-w-sm gap-0 rounded-[1.5rem] border-[var(--color-border)] bg-[var(--composer-surface)] p-6 shadow-[var(--composer-shadow)]">
        <h1 className="text-[1.25rem] font-semibold text-[var(--color-text-primary)]">
          {tr("No access yet")}
        </h1>
        <p className="mt-2 text-[0.875rem] text-[var(--color-text-secondary)]">
          {tr("You’re signed in, but this account hasn’t been added to this workspace yet. Ask an administrator to add you, then try again.")}
        </p>

        <Button asChild className="mt-6 w-full rounded-xl">
          <Link href={`/chat?lang=${getLanguage()}`}>
            {tr("Try again")}
          </Link>
        </Button>
        <SignOutForm />
      </Card>
    </main>
  );
}
