import { getLanguage, t as tr } from "@/app/lib/i18n";
import { Button } from "@/components/ui/button";
import { signOut } from "@/app/shared/signOut";

export function SignOutForm() {
  return (
    <form
      action={`/api/auth/logout?lang=${getLanguage()}`}
      method="post"
      className="mt-3"
      onSubmit={(event) => {
        event.preventDefault();
        void signOut();
      }}
    >
      <Button
        type="submit"
        variant="outline"
        className="w-full rounded-xl"
      >
        {tr("Sign out")}
      </Button>
    </form>
  );
}
