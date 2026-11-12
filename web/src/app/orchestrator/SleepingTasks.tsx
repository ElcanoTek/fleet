import { rich } from "@/app/lib/i18n/rich";
import { t as tr } from "@/app/lib/i18n";
import { useCallback } from "react";
import type { Task } from "@/app/shared/lib/orchestratorApi";
import { orchestratorApi } from "@/app/shared/lib/orchestratorApi";
import { useCancellableFetch } from "@/app/shared/hooks/useCancellableFetch";
import { formatTimeFirst } from "@/app/shared/lib/format";
import { Badge } from "@/components/ui/badge";
import { taskRunLabel } from "./taskDisplay";

// SleepingTasks — thin list of tasks parked in paused_awaiting_wake
// (docs/SELF-WAKE.md). Fire-event lives on the log viewer; this is just the
// operator queue so sleeping work is visible without a status-filter click.
//
// It shows the first SLEEPING_LIMIT sleepers and the server's TOTAL, and it
// refetches whenever `refreshKey` changes — the dashboard passes its reload
// nonce, so the queue moves with the table instead of freezing at mount (a
// task that woke, or fell asleep, used to stay wrong here until a reload).

export const SLEEPING_LIMIT = 20;

export type SleepingTasksProps = {
  onOpen: (task: Task) => void;
  // Any value whose change should trigger a refetch (the dashboard's
  // refreshNonce). Optional so the component still works standalone.
  refreshKey?: number;
};

export function SleepingTasks({ onOpen, refreshKey }: SleepingTasksProps) {
  const { data } = useCancellableFetch(
    useCallback(
      (signal: AbortSignal) =>
        orchestratorApi.tasks(`status=paused_awaiting_wake&limit=${SLEEPING_LIMIT}&offset=0`, signal),
      [],
    ),
    [refreshKey],
  );
  const tasks = data?.data ?? [];
  const total = data?.total ?? tasks.length;
  if (tasks.length === 0) return null;
  return (
    <div className="sleeping-tasks" data-testid="sleeping-tasks" role="region" aria-label={tr("Sleeping tasks")}>
      <div className="sleeping-tasks-head">
        <h3>{tr("Sleeping")}</h3>
        <Badge variant="outline" className="sleeping-tasks-count" data-testid="sleeping-tasks-count">
          {total}
        </Badge>
        {total > tasks.length ? (
          <span className="sleeping-tasks-note" data-testid="sleeping-tasks-truncated">
        {rich("showing the first {0} — filter the table by status to see them all", tasks.length)}
      </span>
        ) : null}
      </div>
      <ul className="sleeping-tasks-list">
        {tasks.map((t) => (
          <li key={t.id}>
            <button
              type="button"
              className="sleeping-task-row"
              data-testid="sleeping-task-row"
              onClick={() => onOpen(t)}
            >
              <span className="sleeping-task-title">{taskRunLabel(t, 60)}</span>
              <span className="sleeping-task-meta">
                {t.wake_event_key
                  ? tr("waiting for “{0}”", [t.wake_event_key])
                  : "timer"}
                {t.wake_at ? tr(" · wakes {0}", [formatTimeFirst(t.wake_at)]) : ""}
              </span>
            </button>
          </li>
        ))}
      </ul>
    </div>
  );
}
