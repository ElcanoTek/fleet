"use client";

// Checklist renders a task_tracker plan as a todo→done list in the Operations
// Center (#518). The chat client already renders the same task_tracker tool
// result live (ToolChips taskTrackerDisplayForMessage); this brings the live,
// updating checklist to scheduled runs. It parses the STRUCTURED tool result
// (never scrapes assistant prose) and mirrors the chat parser's shape, so both
// surfaces agree on statuses and item ordering.

export type ChecklistTask = {
  title: string;
  status: "todo" | "in_progress" | "done";
  notes?: string;
};

export type ChecklistState = {
  tasks: ChecklistTask[];
  summary: { total: number; todo: number; in_progress: number; done: number };
  activeTask: string;
};

// parseTaskTrackerOutput parses a task_tracker tool result's JSON into a
// checklist, or returns null when the payload carries no usable task list (the
// caller then falls back to rendering the raw text). It accepts either the
// result `tasks` array or the input `task_list` array, mirroring the chat
// client's parseTaskList.
export function parseTaskTrackerOutput(raw: string): ChecklistState | null {
  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch {
    return null;
  }
  if (!parsed || typeof parsed !== "object") return null;
  const obj = parsed as Record<string, unknown>;
  const resultTasks = Array.isArray(obj.tasks) ? (obj.tasks as unknown[]) : [];
  const inputTasks = Array.isArray(obj.task_list) ? (obj.task_list as unknown[]) : [];
  const source = resultTasks.length > 0 ? resultTasks : inputTasks;

  const tasks: ChecklistTask[] = source.flatMap((entry) => {
    const t = (entry ?? {}) as Record<string, unknown>;
    if (typeof t.title !== "string" || !t.title.trim()) return [];
    const status = t.status;
    if (status !== "todo" && status !== "in_progress" && status !== "done") return [];
    return [
      {
        title: t.title.trim(),
        status,
        notes: typeof t.notes === "string" && t.notes.trim() ? t.notes.trim() : undefined,
      },
    ];
  });
  if (tasks.length === 0) return null;

  const summary = { total: tasks.length, todo: 0, in_progress: 0, done: 0 };
  for (const t of tasks) summary[t.status] += 1;
  const activeTask =
    tasks.find((t) => t.status === "in_progress")?.title ??
    tasks.find((t) => t.status !== "done")?.title ??
    "";

  return { tasks, summary, activeTask };
}

const GLYPH: Record<ChecklistTask["status"], string> = {
  todo: "[ ]",
  in_progress: "[~]",
  done: "[✓]",
};

// Checklist renders the parsed plan. `live` marks the sticky top progress panel
// (updated each task_tracker result) vs an inline history entry.
export function Checklist({ state, live }: { state: ChecklistState; live?: boolean }) {
  const { tasks, summary } = state;
  return (
    <div className={`checklist${live ? " checklist--live" : ""}`} data-testid={live ? "live-checklist" : "checklist"}>
      <div className="checklist-summary">
        <span className="checklist-summary-label">{live ? "Progress" : "Checklist"}</span>
        <span className="checklist-summary-count" data-testid="checklist-summary">
          {summary.done}/{summary.total} done
          {summary.in_progress > 0 ? ` · ${summary.in_progress} in progress` : ""}
        </span>
      </div>
      <ul className="checklist-items">
        {tasks.map((t, i) => (
          <li key={i} className={`checklist-item checklist-item--${t.status}`} data-testid="checklist-item">
            <span className="checklist-glyph" aria-hidden="true">
              {GLYPH[t.status]}
            </span>
            <span className="checklist-title">{t.title}</span>
          </li>
        ))}
      </ul>
    </div>
  );
}
