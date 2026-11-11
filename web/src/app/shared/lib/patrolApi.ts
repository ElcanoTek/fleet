import { t as tr } from "@/app/lib/i18n";
// patrolApi — the in-chat patrol (schedule) routes, one typed wrapper each:
// POST /api/conversations/<id>/schedules, POST …/schedules/<sid>/run-now,
// PATCH …/schedules/<sid> {action}, DELETE …/schedules/<sid>.
//
// Chat's Patrol bar, the Patrol Launchpad and Settings → Personas all arm and
// steer patrols; they share these so every id goes through the
// conversationApiPath gate and every refusal becomes a message the same way
// (responseErrorText). Each call resolves to the server's reply — the
// conversation's schedules after the change and its auto-approve flag, when
// sent — or rejects: with a PatrolApiError carrying the HTTP status for a
// refused request (or an id the URL gate rejects, status 0), and with fetch's
// own error when the server could not be reached.

import type { ChatSchedule } from "@/app/chat/ui/history";
import { conversationApiPath } from "@/app/lib/conversationApiUrl";
import { responseErrorText } from "@/app/lib/responseError";

export type PatrolResponse = {
  /** The schedule a create made. */
  schedule?: ChatSchedule;
  /** The conversation's schedules after the change. */
  schedules?: ChatSchedule[];
  auto_approve?: boolean;
};

export type CreateScheduleBody = {
  title: string;
  prompt: string;
  cron?: string;
  timezone?: string;
  duration_seconds?: number;
  auto_approve?: boolean;
};

export type ScheduleAction = "pause" | "resume" | "cancel";

export class PatrolApiError extends Error {
  status: number;
  constructor(message: string, status: number) {
    super(message);
    this.name = "PatrolApiError";
    this.status = status;
  }
}

async function send(
  path: string | null,
  init: RequestInit,
  fallback: string,
): Promise<PatrolResponse> {
  if (!path) throw new PatrolApiError(tr("Invalid conversation or schedule id."), 0);
  const res = await fetch(path, init);
  if (!res.ok) throw new PatrolApiError(await responseErrorText(res, fallback), res.status);
  // A 2xx with no JSON body (an older server) still counts as done.
  return ((await res.json().catch(() => ({}))) ?? {}) as PatrolResponse;
}

const JSON_HEADERS = { "Content-Type": "application/json" };

export function createSchedule(
  conversationId: string,
  body: CreateScheduleBody,
  fallback = tr("Couldn't schedule patrol"),
): Promise<PatrolResponse> {
  return send(
    conversationApiPath(conversationId, "schedules"),
    { method: "POST", headers: JSON_HEADERS, body: JSON.stringify(body) },
    fallback,
  );
}

export function runScheduleNow(
  conversationId: string,
  scheduleId: string,
  fallback = tr("Couldn't trigger patrol"),
): Promise<PatrolResponse> {
  return send(
    conversationApiPath(conversationId, "schedules", scheduleId, "run-now"),
    { method: "POST" },
    fallback,
  );
}

export function setScheduleAction(
  conversationId: string,
  scheduleId: string,
  action: ScheduleAction,
  fallback = tr("Couldn't {0} patrol", [tr(action)]),
): Promise<PatrolResponse> {
  return send(
    conversationApiPath(conversationId, "schedules", scheduleId),
    { method: "PATCH", headers: JSON_HEADERS, body: JSON.stringify({ action }) },
    fallback,
  );
}

export function deleteSchedule(
  conversationId: string,
  scheduleId: string,
  fallback = tr("Couldn't cancel patrol"),
): Promise<PatrolResponse> {
  return send(
    conversationApiPath(conversationId, "schedules", scheduleId),
    { method: "DELETE" },
    fallback,
  );
}
