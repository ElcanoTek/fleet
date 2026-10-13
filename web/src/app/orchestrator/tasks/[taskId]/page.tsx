import { redirect } from "next/navigation";

// The link a task notification carries — the browser push, the email, and the
// webhook's log_url — is /orchestrator/tasks/<id>, built by the runner under
// FLEET_PUBLIC_URL. The Operations Center is a single page, so this route
// forwards to it with ?task=<id>, which opens that task's log on load. The
// proxy has already required a session by the time this runs.
export default async function TaskLinkPage({
  params,
}: {
  params: Promise<{ taskId: string }>;
}) {
  const { taskId } = await params;
  redirect(`/orchestrator?task=${encodeURIComponent(taskId)}`);
}
