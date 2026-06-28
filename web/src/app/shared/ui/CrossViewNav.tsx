import Link from "next/link";

// CrossViewNav holds the two links that move between the app's two views —
// /chat and /orchestrator. The one Next middleware gates both off the same
// session cookie, so crossing between them never re-prompts for login.
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
  children = "Operations Center →",
}: {
  className?: string;
  children?: React.ReactNode;
}) {
  return (
    <Link href="/orchestrator" data-testid={NAV_TO_ORCHESTRATOR_TESTID} className={className}>
      {children}
    </Link>
  );
}

export function NavToChat({
  className,
  children = "Go to Chat",
}: {
  className?: string;
  children?: React.ReactNode;
}) {
  return (
    <Link href="/chat" data-testid={NAV_TO_CHAT_TESTID} className={className}>
      {children}
    </Link>
  );
}
