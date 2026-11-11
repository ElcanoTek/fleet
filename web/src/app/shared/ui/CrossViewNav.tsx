import { t as tr } from "@/app/lib/i18n";
import { Link } from "@/app/lib/Link";

// CrossViewNav holds the links that move between the app's surfaces —
// /chat and /orchestrator. (The user guides at /help are reached from the
// account menu, AccountMenu.tsx.) The one Next middleware
// gates them off the same session cookie, so crossing between them never
// re-prompts for login.
//
// These used to be hand-rolled separately in each view's chrome. Centralizing
// them here keeps the shared shell's cross-view affordance — and its stable
// data-testid hooks (nav-to-orchestrator / nav-to-chat), which the live and
// mocked cross-view specs drive — defined in one place. The `className` and
// `children` (link label) stay overridable so each surface can keep its own
// existing styling and copy; only the destination + testid are fixed.

const NAV_TO_ORCHESTRATOR_TESTID = "nav-to-orchestrator";
const NAV_TO_CHAT_TESTID = "nav-to-chat";

export function NavToOrchestrator({
  className,
  children = tr("Background tasks →"),
  ariaCurrent,
  dataTip,
}: {
  className?: string;
  children?: React.ReactNode;
  ariaCurrent?: "page";
  // CSS tooltip text (globals.css [data-tip]) — the collapsed rail labels its
  // icon-only nav items this way.
  dataTip?: string;
}) {
  return (
    <Link
      href="/orchestrator"
      data-testid={NAV_TO_ORCHESTRATOR_TESTID}
      className={className}
      aria-current={ariaCurrent}
      data-tip={dataTip}
    >
      {children}
    </Link>
  );
}

export function NavToChat({
  className,
  children = tr("Go to Chat"),
  ariaCurrent,
  dataTip,
}: {
  className?: string;
  children?: React.ReactNode;
  ariaCurrent?: "page";
  dataTip?: string;
}) {
  return (
    <Link
      href="/chat"
      data-testid={NAV_TO_CHAT_TESTID}
      className={className}
      aria-current={ariaCurrent}
      data-tip={dataTip}
    >
      {children}
    </Link>
  );
}
