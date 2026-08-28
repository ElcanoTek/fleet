import { afterEach, describe, expect, it, vi } from "vitest";
import { render, screen, fireEvent, waitFor, cleanup } from "@testing-library/react";
import { TaskCreateModal } from "./TaskCreateModal";
import type { McpServer, Task, TaskTemplate } from "@/app/shared/lib/orchestratorApi";

// Component tests for the redesigned New Task modal: schedule mode segment,
// launch gating + footer reason, blur validation with the design's error copy,
// the email chip field, the dirty-close guard (with form reset on discard),
// and the create path.

const taskTemplates = vi.fn();
const createTask = vi.fn();
const estimateTask = vi.fn();
const uploadFile = vi.fn();
const updateTask = vi.fn();
const rerunTask = vi.fn();
const prompts = vi.fn();

vi.mock("@/app/shared/lib/orchestratorApi", () => ({
  orchestratorApi: {
    taskTemplates: (...args: unknown[]) => taskTemplates(...args),
    createTask: (...args: unknown[]) => createTask(...args),
    estimateTask: (...args: unknown[]) => estimateTask(...args),
    uploadFile: (...args: unknown[]) => uploadFile(...args),
    updateTask: (...args: unknown[]) => updateTask(...args),
    rerunTask: (...args: unknown[]) => rerunTask(...args),
    prompts: (...args: unknown[]) => prompts(...args),
  },
}));

const SERVERS: McpServer[] = [
  { name: "xandr", description: "Xandr DSP", tool_count: 7, accounts: ["client_a"] },
];

const DEFAULT_ON_SERVERS: McpServer[] = [
  { name: "email", description: "Inbound reports", tool_count: 10, enabled: true },
  ...SERVERS,
];

function renderModal(
  overrides: Partial<Parameters<typeof TaskCreateModal>[0]> = {},
  templateList: TaskTemplate[] = [],
) {
  taskTemplates.mockResolvedValue(templateList);
  const onClose = vi.fn();
  const onCreated = vi.fn();
  const utils = render(
    <TaskCreateModal open servers={SERVERS} onClose={onClose} onCreated={onCreated} {...overrides} />,
  );
  return { ...utils, onClose, onCreated };
}

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("TaskCreateModal — launch gating", () => {
  it("disables Launch with a visible reason only while the prompt is empty", () => {
    renderModal();
    const launch = screen.getByRole("button", { name: "Launch task" });
    expect(launch).toBeDisabled();
    expect(screen.getByText("Add a prompt to launch")).toBeInTheDocument();

    fireEvent.change(screen.getByLabelText("Prompt"), { target: { value: "Do the thing" } });
    expect(launch).toBeEnabled();
    expect(screen.queryByText("Add a prompt to launch")).not.toBeInTheDocument();
  });
});

describe("TaskCreateModal — schedule modes", () => {
  it("defaults to Run now and swaps fields per mode (only one mode's fields exist)", () => {
    renderModal();
    expect(screen.getByRole("radio", { name: "Run now" })).toHaveAttribute("aria-checked", "true");
    expect(screen.getByText("Runs immediately after launch.")).toBeInTheDocument();
    expect(screen.queryByLabelText("Cron expression")).not.toBeInTheDocument();
    expect(screen.queryByLabelText("Schedule date")).not.toBeInTheDocument();

    fireEvent.click(screen.getByRole("radio", { name: "Repeat" }));
    expect(screen.getByLabelText("Repeat frequency")).toBeInTheDocument();
    expect(screen.queryByLabelText("Cron expression")).not.toBeInTheDocument();
    expect(screen.getByText(/Next run/)).toBeInTheDocument();
    expect(screen.queryByText("Weekdays 9am")).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("tab", { name: "Advanced cron" }));
    expect(screen.getByText("Weekdays 9am")).toBeInTheDocument();
    expect(screen.queryByLabelText("Schedule date")).not.toBeInTheDocument();

    fireEvent.click(screen.getByRole("radio", { name: "Run once" }));
    expect(screen.getByLabelText("Schedule date")).toBeInTheDocument();
    expect(screen.queryByLabelText("Cron expression")).not.toBeInTheDocument();
  });

  it("validates cron on blur with the specific field-count copy and counts it in the footer", () => {
    renderModal();
    fireEvent.change(screen.getByLabelText("Prompt"), { target: { value: "Do the thing" } });
    fireEvent.click(screen.getByRole("radio", { name: "Repeat" }));
    fireEvent.click(screen.getByRole("tab", { name: "Advanced cron" }));
    const cron = screen.getByLabelText("Cron expression");
    fireEvent.change(cron, { target: { value: "0 9 * *" } });
    fireEvent.blur(cron);
    expect(
      screen.getByText("Cron needs 5 fields — got 4. Format: min · hour · day · month · weekday."),
    ).toBeInTheDocument();
    expect(screen.getByText("Fix 1 field above")).toBeInTheDocument();
    // Launch stays ENABLED with field errors — only an empty prompt disables it.
    expect(screen.getByRole("button", { name: "Launch task" })).toBeEnabled();

    // A preset fixes the field and clears the error + echo returns.
    fireEvent.click(screen.getByText("Weekdays 9am"));
    expect(screen.queryByText(/Cron needs 5 fields/)).not.toBeInTheDocument();
    expect(screen.getByText(/At 09:00, Monday through Friday/)).toBeInTheDocument();
  });

  it("builds a multi-day weekly cron schedule without requiring cron knowledge", async () => {
    createTask.mockResolvedValue({ id: "t-1" });
    renderModal();
    fireEvent.change(screen.getByLabelText("Prompt"), { target: { value: "Do the thing" } });
    fireEvent.click(screen.getByRole("radio", { name: "Repeat" }));
    fireEvent.change(screen.getByLabelText("Repeat frequency"), { target: { value: "weekly" } });
    const monday = screen.getByRole("button", { name: "Run on Monday" });
    expect(monday).toHaveAttribute("aria-pressed", "true");
    expect(monday).toBeDisabled();
    fireEvent.click(screen.getByRole("button", { name: "Run on Wednesday" }));
    expect(screen.getByRole("button", { name: "Run on Wednesday" })).toHaveAttribute(
      "aria-pressed",
      "true",
    );
    expect(monday).toBeEnabled();
    fireEvent.change(screen.getByLabelText("Repeat time"), { target: { value: "13:30" } });
    fireEvent.click(screen.getByRole("button", { name: "Launch task" }));

    await waitFor(() => expect(createTask).toHaveBeenCalledTimes(1));
    expect(createTask.mock.calls[0][0]).toMatchObject({ recurrence: "30 13 * * 1,3" });
  });

  it("hydrates the friendly controls from a supported multi-day cron expression", () => {
    renderModal();
    fireEvent.click(screen.getByRole("radio", { name: "Repeat" }));
    fireEvent.click(screen.getByRole("tab", { name: "Advanced cron" }));
    fireEvent.change(screen.getByLabelText("Cron expression"), {
      target: { value: "0 9 * * 1,4" },
    });
    fireEvent.click(screen.getByRole("tab", { name: "Simple schedule" }));

    expect(screen.getByLabelText("Repeat frequency")).toHaveValue("weekly");
    expect(screen.getByLabelText("Repeat time")).toHaveValue("09:00");
    expect(screen.getByRole("button", { name: "Run on Monday" })).toHaveAttribute(
      "aria-pressed",
      "true",
    );
    expect(screen.getByRole("button", { name: "Run on Thursday" })).toHaveAttribute(
      "aria-pressed",
      "true",
    );
    expect(screen.getByRole("button", { name: "Run on Wednesday" })).toHaveAttribute(
      "aria-pressed",
      "false",
    );
  });

  it("requires a date in Run once mode at submit and focuses it", async () => {
    renderModal();
    fireEvent.change(screen.getByLabelText("Prompt"), { target: { value: "Do the thing" } });
    fireEvent.click(screen.getByRole("radio", { name: "Run once" }));
    fireEvent.click(screen.getByRole("button", { name: "Launch task" }));
    expect(await screen.findByText("Pick a date and time.")).toBeInTheDocument();
    expect(createTask).not.toHaveBeenCalled();
  });
});

describe("TaskCreateModal — email chip field", () => {
  it("adds valid emails as chips on Enter and names why an address is invalid", () => {
    renderModal();
    const input = screen.getByLabelText(/Email results/);
    fireEvent.change(input, { target: { value: "traders@elcanotek" } });
    fireEvent.keyDown(input, { key: "Enter" });
    expect(
      screen.getByText('Not a valid email: "traders@elcanotek" — missing domain.'),
    ).toBeInTheDocument();

    fireEvent.change(input, { target: { value: "sam@elcanotek.com" } });
    fireEvent.keyDown(input, { key: "Enter" });
    expect(screen.queryByText(/Not a valid email/)).not.toBeInTheDocument();
    expect(screen.getByText("sam@elcanotek.com")).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "Remove sam@elcanotek.com" }));
    expect(screen.queryByText("sam@elcanotek.com")).not.toBeInTheDocument();
  });
});

describe("TaskCreateModal — dirty-close guard", () => {
  it("closes a clean form instantly, guards a dirty one, and resets on Discard", () => {
    const { onClose, rerender } = renderModal();
    // Clean: Cancel closes with no guard.
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(onClose).toHaveBeenCalledTimes(1);
    expect(screen.queryByText("Discard this task?")).not.toBeInTheDocument();

    // Dirty: the guard names what would be lost; Keep editing stays open.
    fireEvent.change(screen.getByLabelText("Prompt"), { target: { value: "Do the thing" } });
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(screen.getByText("Discard this task?")).toBeInTheDocument();
    expect(screen.getByText(/the prompt will be lost if you close/)).toBeInTheDocument();
    expect(onClose).toHaveBeenCalledTimes(1);
    fireEvent.click(screen.getByRole("button", { name: "Keep editing" }));
    expect(screen.queryByText("Discard this task?")).not.toBeInTheDocument();

    // Discard closes AND resets — reopening shows a fresh form.
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    fireEvent.click(screen.getByRole("button", { name: "Discard" }));
    expect(onClose).toHaveBeenCalledTimes(2);
    rerender(
      <TaskCreateModal open={false} servers={SERVERS} onClose={onClose} onCreated={() => {}} />,
    );
    rerender(<TaskCreateModal open servers={SERVERS} onClose={onClose} onCreated={() => {}} />);
    expect((screen.getByLabelText("Prompt") as HTMLTextAreaElement).value).toBe("");
  });

  it("Escape runs the same guard", () => {
    const { onClose } = renderModal();
    fireEvent.change(screen.getByLabelText("Prompt"), { target: { value: "Do the thing" } });
    fireEvent.keyDown(document, { key: "Escape" });
    expect(screen.getByText("Discard this task?")).toBeInTheDocument();
    expect(onClose).not.toHaveBeenCalled();
  });
});

describe("TaskCreateModal — create path", () => {
  it("starts operator-default connectors on and submits the visible selection", async () => {
    createTask.mockResolvedValue({ id: "t-1" });
    renderModal({ servers: DEFAULT_ON_SERVERS });

    fireEvent.click(screen.getByRole("button", { name: /Tools & files/ }));
    expect(screen.getByTestId("mcp-toggle-email")).toBeChecked();
    expect(screen.getByTestId("mcp-toggle-xandr")).not.toBeChecked();

    fireEvent.change(screen.getByLabelText("Prompt"), { target: { value: "Read the inbox" } });
    fireEvent.click(screen.getByRole("button", { name: "Launch task" }));

    await waitFor(() => expect(createTask).toHaveBeenCalledTimes(1));
    expect(createTask.mock.calls[0][0].mcp_selection).toEqual([{ server: "email" }]);
  });

  it("adopts operator defaults that arrive after the modal mounts", () => {
    const { rerender, onClose, onCreated } = renderModal({ servers: [] });
    rerender(
      <TaskCreateModal
        open
        servers={DEFAULT_ON_SERVERS}
        onClose={onClose}
        onCreated={onCreated}
      />,
    );

    fireEvent.click(screen.getByRole("button", { name: /Tools & files/ }));
    expect(screen.getByTestId("mcp-toggle-email")).toBeChecked();
  });

  it("lets the operator replace a default-on connector with an explicit selection", async () => {
    createTask.mockResolvedValue({ id: "t-1" });
    renderModal({ servers: DEFAULT_ON_SERVERS });

    fireEvent.click(screen.getByRole("button", { name: /Tools & files/ }));
    fireEvent.click(screen.getByTestId("mcp-toggle-email"));
    fireEvent.click(screen.getByTestId("mcp-toggle-xandr"));
    fireEvent.change(screen.getByLabelText("Prompt"), { target: { value: "Run Xandr only" } });
    fireEvent.click(screen.getByRole("button", { name: "Launch task" }));

    await waitFor(() => expect(createTask).toHaveBeenCalledTimes(1));
    expect(createTask.mock.calls[0][0].mcp_selection).toEqual([{ server: "xandr" }]);
  });

  it("submits the typed prompt with mode-scoped schedule fields and resets after success", async () => {
    createTask.mockResolvedValue({ id: "t-1" });
    const { onClose, onCreated } = renderModal();
    fireEvent.change(screen.getByLabelText("Prompt"), { target: { value: "Do the thing" } });
    fireEvent.click(screen.getByRole("radio", { name: "Repeat" }));
    fireEvent.click(screen.getByRole("button", { name: "Launch task" }));

    await waitFor(() => expect(createTask).toHaveBeenCalledTimes(1));
    const body = createTask.mock.calls[0][0] as Record<string, unknown>;
    expect(body.prompt).toBe("Do the thing");
    expect(body.recurrence).toBe("0 9 * * 1-5");
    expect(body.scheduled_for).toBeUndefined();
    await waitFor(() => expect(onClose).toHaveBeenCalled());
    expect(onCreated).toHaveBeenCalled();
  });

  it("surfaces the server's reason when the cost estimate is rejected", async () => {
    estimateTask.mockRejectedValue(new Error("prompt cannot exceed 100000 characters"));
    renderModal();
    fireEvent.change(screen.getByLabelText("Prompt"), { target: { value: "A very big protocol" } });
    fireEvent.click(screen.getByRole("button", { name: "Estimate cost" }));

    expect(
      await screen.findByText("Estimate failed: prompt cannot exceed 100000 characters"),
    ).toBeInTheDocument();
  });

  it("updates the Tools & files summary as servers are enabled", () => {
    renderModal();
    expect(screen.getByText("Sandbox only — no tools")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: /Tools & files/ }));
    fireEvent.click(screen.getByTestId("mcp-toggle-xandr"));
    expect(screen.getByText("1 server")).toBeInTheDocument();
  });
});

// ── edit mode ─────────────────────────────────────────────────────────────────

const EDIT_ID = "aaaabbbb-cccc-dddd-eeee-ffff00001111";
const baseEdit: Task = {
  id: EDIT_ID,
  prompt: "Weekly latency report",
  description: "sums p95s",
  tags: ["reports"],
  status: "scheduled",
  model: "z-ai/glm-5.2",
};

describe("TaskCreateModal — edit mode", () => {
  it("keeps a task's saved connector selection instead of applying new defaults", () => {
    renderModal({
      servers: DEFAULT_ON_SERVERS,
      editTask: { ...baseEdit, mcp_selection: [] },
      onUpdated: vi.fn(),
    });

    fireEvent.click(screen.getByRole("button", { name: /Tools & files/ }));
    expect(screen.getByTestId("mcp-toggle-email")).not.toBeChecked();
  });

  it("prefills the form from the task and saves a non-recurring edit via PUT", async () => {
    updateTask.mockResolvedValue({ id: EDIT_ID });
    const onUpdated = vi.fn();
    renderModal({ editTask: baseEdit, onUpdated });

    const promptBox = screen.getByLabelText("Prompt") as HTMLTextAreaElement;
    expect(promptBox.value).toBe("Weekly latency report");
    expect(screen.getByRole("heading", { name: "Edit Task" })).toBeTruthy();

    fireEvent.change(promptBox, { target: { value: "Weekly latency report v2" } });
    fireEvent.click(screen.getByRole("button", { name: /save task changes/i }));

    await waitFor(() => expect(updateTask).toHaveBeenCalledTimes(1));
    const [id, body] = updateTask.mock.calls[0];
    expect(id).toBe(EDIT_ID);
    expect(body.prompt).toBe("Weekly latency report v2");
    // cleared-able collections are always present on an edit (empty = clear)
    expect(Array.isArray(body.mcp_selection)).toBe(true);
    expect(body.tags).toEqual(["reports"]);
    expect(onUpdated).toHaveBeenCalled();
    expect(rerunTask).not.toHaveBeenCalled();
  });

  it("echoes a gate's exit_code_is on edit even though the form has no field for it", async () => {
    updateTask.mockResolvedValue({ id: EDIT_ID });
    renderModal({
      editTask: {
        ...baseEdit,
        run_if: { command: "test -f /tmp/ready", exit_code_is: 2, timeout_seconds: 30 },
      },
      onUpdated: vi.fn(),
    });

    fireEvent.change(screen.getByLabelText("Prompt"), {
      target: { value: "Weekly latency report v2" },
    });
    fireEvent.click(screen.getByRole("button", { name: /save task changes/i }));

    await waitFor(() => expect(updateTask).toHaveBeenCalledTimes(1));
    const [, body] = updateTask.mock.calls[0];
    // A lossy echo (dropping exit_code_is) reads server-side as an attempt to
    // CHANGE the run_if gate and 403s non-admin edits of unrelated fields.
    expect(body.run_if).toEqual({
      command: "test -f /tmp/ready",
      exit_code_is: 2,
      on_error: "run",
      timeout_seconds: 30,
    });
  });

  it("echoes SLA multipliers on edit even though the form has no fields for them", async () => {
    updateTask.mockResolvedValue({ id: EDIT_ID });
    renderModal({
      editTask: {
        ...baseEdit,
        expected_duration_minutes: 30,
        sla_warn_multiplier: 1.2,
        sla_fail_multiplier: 3,
      },
      onUpdated: vi.fn(),
    });

    fireEvent.change(screen.getByLabelText("Prompt"), {
      target: { value: "Weekly latency report v2" },
    });
    fireEvent.click(screen.getByRole("button", { name: /save task changes/i }));

    await waitFor(() => expect(updateTask).toHaveBeenCalledTimes(1));
    const [, body] = updateTask.mock.calls[0];
    // PUT is full-replace server-side: a lossy echo would silently reset
    // API-set thresholds to the defaults.
    expect(body.expected_duration_minutes).toBe(30);
    expect(body.sla_warn_multiplier).toBe(1.2);
    expect(body.sla_fail_multiplier).toBe(3);
  });

  it("asks all-future-runs vs run-once for a recurring task; PUT for the definition", async () => {
    updateTask.mockResolvedValue({ id: EDIT_ID });
    renderModal({
      editTask: { ...baseEdit, recurrence: "0 9 * * 1" },
      onUpdated: vi.fn(),
    });
    fireEvent.click(screen.getByRole("button", { name: /save task changes/i }));
    // no network call yet — the scope chooser interposes
    expect(updateTask).not.toHaveBeenCalled();
    const chooser = await screen.findByTestId("edit-scope-chooser");
    expect(chooser).toHaveTextContent(/every\s+future run/i);
    fireEvent.click(screen.getByTestId("edit-scope-definition"));
    await waitFor(() => expect(updateTask).toHaveBeenCalledTimes(1));
    expect(updateTask.mock.calls[0][1].recurrence).toBe("0 9 * * 1");
    expect(rerunTask).not.toHaveBeenCalled();
  });

  it("run-once on a recurring task resubmits with overrides and leaves the definition alone", async () => {
    rerunTask.mockResolvedValue({ id: "99990000-9999-0000-9999-000000000000" });
    renderModal({
      editTask: { ...baseEdit, recurrence: "0 9 * * 1" },
      onUpdated: vi.fn(),
    });
    const promptBox = screen.getByLabelText("Prompt");
    fireEvent.change(promptBox, { target: { value: "One-off variant" } });
    fireEvent.click(screen.getByRole("button", { name: /save task changes/i }));
    fireEvent.click(await screen.findByTestId("edit-scope-once"));
    await waitFor(() => expect(rerunTask).toHaveBeenCalledTimes(1));
    const [id, overrides] = rerunTask.mock.calls[0];
    expect(id).toBe(EDIT_ID);
    expect(overrides.prompt).toBe("One-off variant");
    expect(updateTask).not.toHaveBeenCalled();
  });

  it("a terminal task resubmits directly (with the resubmit notice, no chooser)", async () => {
    rerunTask.mockResolvedValue({ id: "99990000-9999-0000-9999-000000000000" });
    renderModal({
      editTask: { ...baseEdit, status: "success", recurrence: "" },
      onUpdated: vi.fn(),
    });
    expect(screen.getByText(/already ran — saving resubmits/i)).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: /save task changes/i }));
    await waitFor(() => expect(rerunTask).toHaveBeenCalledTimes(1));
    expect(updateTask).not.toHaveBeenCalled();
    expect(screen.queryByTestId("edit-scope-chooser")).toBeNull();
  });

  it("resubmits a terminal task with the connector selection shown in the editor", async () => {
    rerunTask.mockResolvedValue({ id: "99990000-9999-0000-9999-000000000000" });
    renderModal({
      servers: DEFAULT_ON_SERVERS,
      editTask: { ...baseEdit, status: "success", mcp_selection: [] },
      onUpdated: vi.fn(),
    });

    fireEvent.click(screen.getByRole("button", { name: /Tools & files/ }));
    fireEvent.click(screen.getByTestId("mcp-toggle-email"));
    fireEvent.click(screen.getByRole("button", { name: /save task changes/i }));

    await waitFor(() => expect(rerunTask).toHaveBeenCalledTimes(1));
    expect(rerunTask.mock.calls[0][1].mcp_selection).toEqual([{ server: "email" }]);
  });

  it("closing an untouched edit form does not raise the discard guard", () => {
    const { onClose } = renderModal({ editTask: baseEdit, onUpdated: vi.fn() });
    fireEvent.click(screen.getByRole("button", { name: /^cancel$/i }));
    expect(screen.queryByText(/unsaved changes/i)).toBeNull();
    expect(onClose).toHaveBeenCalled();
  });
});

describe("TaskCreateModal — task templates", () => {
  const TEMPLATES: TaskTemplate[] = [
    {
      name: "Site Watch",
      description: "Drive a browser to a page on a schedule.",
      icon: "👁️",
      variables: ["url", "what"],
      task: {
        prompt: "Open {url}, extract {what}, compare to last run. Today is {date}.",
        model: "anthropic/claude-sonnet-4.5",
        allow_network: true,
        tags: ["monitoring"],
        expected_duration_minutes: 10,
      },
    },
    {
      name: "Plain Summary",
      description: "No custom variables.",
      variables: [],
      task: { prompt: "Summarize today's inbox. Today is {date}." },
    },
  ];
  const TODAY = new Date().toISOString().slice(0, 10);

  it("renders the template cards with name and description", async () => {
    renderModal({}, TEMPLATES);
    await screen.findByTestId("task-template-section");
    expect(screen.getByText("Site Watch")).toBeInTheDocument();
    expect(screen.getByText("Drive a browser to a page on a schedule.")).toBeInTheDocument();
    expect(screen.getByText("Plain Summary")).toBeInTheDocument();
  });

  it("opens the inline variable fill (no native prompt), humanizing the labels", async () => {
    const promptSpy = vi.spyOn(window, "prompt").mockImplementation(() => null);
    renderModal({}, TEMPLATES);
    await screen.findByTestId("task-template-section");
    fireEvent.click(screen.getByText("Site Watch"));

    expect(await screen.findByTestId("template-var-fill")).toBeInTheDocument();
    expect(screen.getByText("Fill in: Site Watch")).toBeInTheDocument();
    expect(screen.getByLabelText("Url")).toBeInTheDocument();
    expect(screen.getByLabelText("What")).toBeInTheDocument();
    // The card grid swaps out while the fill is open.
    expect(screen.queryByTestId("task-template-section")).not.toBeInTheDocument();
    expect(promptSpy).not.toHaveBeenCalled();
    promptSpy.mockRestore();
  });

  it("applies filled values, keeps blank placeholders visible, and seeds the form", async () => {
    renderModal({}, TEMPLATES);
    await screen.findByTestId("task-template-section");
    fireEvent.click(screen.getByText("Site Watch"));

    fireEvent.change(await screen.findByLabelText("Url"), {
      target: { value: "https://example.com" },
    });
    // "What" is deliberately left blank — its {what} placeholder must survive.
    fireEvent.keyDown(screen.getByLabelText("What"), { key: "Enter" });

    const promptBox = screen.getByLabelText("Prompt") as HTMLTextAreaElement;
    expect(promptBox.value).toContain("Open https://example.com");
    expect(promptBox.value).toContain("{what}");
    expect(promptBox.value).toContain(`Today is ${TODAY}.`);
    // Non-prompt fields seed too, and the cards return for further editing.
    expect((screen.getByLabelText("Tags") as HTMLInputElement).value).toBe("monitoring");
    expect(screen.getByTestId("task-template-section")).toBeInTheDocument();
  });

  it("Back returns to the cards without seeding the form", async () => {
    renderModal({}, TEMPLATES);
    await screen.findByTestId("task-template-section");
    fireEvent.click(screen.getByText("Site Watch"));
    fireEvent.click(await screen.findByTestId("template-var-back"));

    expect(screen.getByTestId("task-template-section")).toBeInTheDocument();
    expect((screen.getByLabelText("Prompt") as HTMLTextAreaElement).value).toBe("");
  });

  it("a template with no custom variables applies immediately", async () => {
    renderModal({}, TEMPLATES);
    await screen.findByTestId("task-template-section");
    fireEvent.click(screen.getByText("Plain Summary"));

    expect(screen.queryByTestId("template-var-fill")).not.toBeInTheDocument();
    const promptBox = screen.getByLabelText("Prompt") as HTMLTextAreaElement;
    expect(promptBox.value).toBe(`Summarize today's inbox. Today is ${TODAY}.`);
  });
});

// ── title ─────────────────────────────────────────────────────────────────────

describe("TaskCreateModal — title", () => {
  it("sends the trimmed title with the create payload", async () => {
    createTask.mockReset();
    createTask.mockResolvedValue({ id: "t-1" });
    renderModal();
    fireEvent.change(screen.getByLabelText("Title"), {
      target: { value: "  Daily pacing summary  " },
    });
    fireEvent.change(screen.getByLabelText("Prompt"), { target: { value: "Do the thing" } });
    fireEvent.click(screen.getByRole("button", { name: "Launch task" }));

    await waitFor(() => expect(createTask).toHaveBeenCalledTimes(1));
    const body = createTask.mock.calls[0][0] as Record<string, unknown>;
    expect(body.title).toBe("Daily pacing summary");
  });

  it("omits title entirely when left blank, so the task stays untitled", async () => {
    createTask.mockReset();
    createTask.mockResolvedValue({ id: "t-1" });
    renderModal();
    fireEvent.change(screen.getByLabelText("Prompt"), { target: { value: "Do the thing" } });
    fireEvent.click(screen.getByRole("button", { name: "Launch task" }));

    await waitFor(() => expect(createTask).toHaveBeenCalledTimes(1));
    const body = createTask.mock.calls[0][0] as Record<string, unknown>;
    expect(body.title).toBeUndefined();
  });

  it("rejects an over-long title in the form instead of round-tripping a 400", async () => {
    createTask.mockReset();
    renderModal();
    fireEvent.change(screen.getByLabelText("Title"), { target: { value: "a".repeat(121) } });
    fireEvent.change(screen.getByLabelText("Prompt"), { target: { value: "Do the thing" } });
    fireEvent.click(screen.getByRole("button", { name: "Launch task" }));

    expect(await screen.findByTestId("error-title")).toBeInTheDocument();
    expect(createTask).not.toHaveBeenCalled();
  });

  it("prefills the title when editing a titled task", () => {
    renderModal({ editTask: { ...baseEdit, title: "Weekly latency" } });
    expect(screen.getByLabelText("Title")).toHaveValue("Weekly latency");
  });

  it("seeds the title from the template's name", async () => {
    renderModal({}, [
      {
        name: "Plain Summary",
        description: "No custom variables.",
        variables: [],
        task: { prompt: "Summarize today's inbox." },
      },
    ]);
    await screen.findByTestId("task-template-section");
    fireEvent.click(screen.getByText("Plain Summary"));
    expect(screen.getByLabelText("Title")).toHaveValue("Plain Summary");
  });
});

describe("TaskCreateModal — title from the prompt library", () => {
  const LIBRARY_ENTRY = {
    id: "git:Reklaim_Daily_DoD_Health_Scan.yaml",
    name: "Reklaim daily day-over-day campaign health scan",
    description: "The daily half of the DoD/WoW pair.",
    content: "name: Reklaim daily day-over-day campaign health scan\ngoal: produce the scan",
    source: "git",
    visibility: "workspace",
    read_only: true,
    path: "prompts/Reklaim_Daily_DoD_Health_Scan.yaml",
  };

  const openLibraryAndUse = async () => {
    fireEvent.click(screen.getByRole("button", { name: "Open prompt library" }));
    await screen.findByRole("button", { name: "Use prompt" });
    fireEvent.click(screen.getByRole("button", { name: "Use prompt" }));
  };

  it("names the task after the library entry it inserted", async () => {
    prompts.mockResolvedValue([LIBRARY_ENTRY]);
    renderModal();
    await openLibraryAndUse();

    await waitFor(() =>
      expect(screen.getByLabelText("Title")).toHaveValue(LIBRARY_ENTRY.name),
    );
    // The prompt still receives the entry's exact content.
    expect(screen.getByLabelText("Prompt")).toHaveValue(LIBRARY_ENTRY.content);
  });

  it("does not overwrite a title the operator already typed", async () => {
    prompts.mockResolvedValue([LIBRARY_ENTRY]);
    renderModal();
    fireEvent.change(screen.getByLabelText("Title"), { target: { value: "My own name" } });
    await openLibraryAndUse();

    await waitFor(() =>
      expect(screen.getByLabelText("Prompt")).toHaveValue(LIBRARY_ENTRY.content),
    );
    expect(screen.getByLabelText("Title")).toHaveValue("My own name");
  });
});

describe("TaskCreateModal — prompt editor sizing", () => {
  const LONG_PROMPT = Array.from({ length: 60 }, (_, i) => `line ${i + 1}`).join("\n");

  afterEach(() => {
    try {
      window.localStorage.clear();
    } catch {
      /* ignore */
    }
  });

  it("grows a prefilled prompt instead of leaving it in a three-row box", () => {
    renderModal({ editTask: { ...baseEdit, prompt: LONG_PROMPT } });
    const el = screen.getByLabelText("Prompt") as HTMLTextAreaElement;
    // jsdom reports scrollHeight 0, so the exact px cannot be asserted — what
    // matters is that the prefill was measured at all (an inline height set),
    // which is precisely what never happened before: edit mode only ever ran
    // auto-grow from onChange, so an untouched prefill kept rows={3}.
    expect(el.style.height).not.toBe("");
  });

  it("toggles the tall editing pane and remembers the choice", () => {
    const { unmount } = renderModal();
    const toggle = screen.getByTestId("prompt-expand-toggle");
    expect(toggle).toHaveAttribute("aria-pressed", "false");
    expect(screen.getByLabelText("Prompt").className).not.toContain("is-expanded");

    fireEvent.click(toggle);
    expect(toggle).toHaveAttribute("aria-pressed", "true");
    const expanded = screen.getByLabelText("Prompt");
    expect(expanded.className).toContain("is-expanded");
    // The class rule owns the height while expanded, so no inline value may
    // shadow it.
    expect((expanded as HTMLTextAreaElement).style.height).toBe("");

    // A fresh mount restores the preference.
    unmount();
    renderModal();
    expect(screen.getByTestId("prompt-expand-toggle")).toHaveAttribute("aria-pressed", "true");
    expect(screen.getByLabelText("Prompt").className).toContain("is-expanded");
  });

  it("collapsing hands sizing back to auto-grow", () => {
    renderModal();
    const toggle = screen.getByTestId("prompt-expand-toggle");
    fireEvent.click(toggle);
    fireEvent.click(toggle);
    expect(toggle).toHaveAttribute("aria-pressed", "false");
    expect(screen.getByLabelText("Prompt").className).not.toContain("is-expanded");
  });
});

// ── sub-agent delegation (#1043) ──────────────────────────────────────────────
// Default-on with an Advanced opt-out: the toggle starts checked, an untouched
// form omits the field (server default = true), and switching it off sends the
// explicit false.

describe("TaskCreateModal — sub-agent delegation", () => {
  const openAdvanced = () =>
    fireEvent.click(screen.getByRole("button", { name: /Advanced/ }));

  it("defaults the toggle ON and omits allow_delegation from the create payload", async () => {
    createTask.mockResolvedValue({ id: "t-1" });
    renderModal();
    fireEvent.change(screen.getByLabelText("Prompt"), { target: { value: "Do it" } });
    openAdvanced();
    expect(
      screen.getByRole("checkbox", { name: /Allow sub-agent delegation/ }),
    ).toBeChecked();
    fireEvent.click(screen.getByRole("button", { name: "Launch task" }));
    await waitFor(() => expect(createTask).toHaveBeenCalledTimes(1));
    const body = createTask.mock.calls[0][0] as Record<string, unknown>;
    expect(body.allow_delegation).toBeUndefined();
  });

  it("sends the explicit opt-out when toggled off", async () => {
    createTask.mockResolvedValue({ id: "t-2" });
    renderModal();
    fireEvent.change(screen.getByLabelText("Prompt"), { target: { value: "Do it" } });
    openAdvanced();
    fireEvent.click(screen.getByRole("checkbox", { name: /Allow sub-agent delegation/ }));
    fireEvent.click(screen.getByRole("button", { name: "Launch task" }));
    await waitFor(() => expect(createTask).toHaveBeenCalledTimes(1));
    const body = createTask.mock.calls[0][0] as Record<string, unknown>;
    expect(body.allow_delegation).toBe(false);
  });

  it("prefills an edited task's explicit opt-out (and an old payload as on)", () => {
    renderModal({ editTask: { ...baseEdit, allow_delegation: false } });
    openAdvanced();
    expect(
      screen.getByRole("checkbox", { name: /Allow sub-agent delegation/ }),
    ).not.toBeChecked();
    cleanup();
    vi.clearAllMocks();
    // A pre-#1043 payload has no field at all: treat as the default (on).
    renderModal({ editTask: baseEdit });
    openAdvanced();
    expect(
      screen.getByRole("checkbox", { name: /Allow sub-agent delegation/ }),
    ).toBeChecked();
  });
});

// ── Accessibility of the dialog shell ────────────────────────────────────────
// These pin the behavior that the jsx-a11y pass moved around, so a later
// "silence the linter" edit can't quietly take it away again: the scrim is
// presentational but still closes on click, the drop zone is still the dialog
// PANEL (its handlers now live on the scrim and are scoped by containment),
// every switch is named by its title line rather than by title+paragraph, and
// the cost figure is a real focusable control with the breakdown attached.

function dropFiles(node: Element, files: File[]) {
  fireEvent.drop(node, {
    dataTransfer: { files, types: ["Files"], items: [] },
  });
}

describe("TaskCreateModal — dialog shell a11y", () => {
  it("closes a clean form on Escape and on a backdrop click", () => {
    const { onClose } = renderModal();
    fireEvent.keyDown(document, { key: "Escape" });
    expect(onClose).toHaveBeenCalledTimes(1);

    const overlay = screen.getByRole("dialog").parentElement as HTMLElement;
    // The scrim carries no semantics of its own — the panel is the dialog.
    expect(overlay).toHaveAttribute("role", "presentation");
    fireEvent.mouseDown(overlay);
    fireEvent.click(overlay);
    expect(onClose).toHaveBeenCalledTimes(2);
  });

  it("keeps the dialog panel as the attachment drop zone, not the whole scrim", () => {
    renderModal();
    const tools = () => screen.getByRole("button", { name: /Tools & files/ });
    const panel = screen.getByRole("dialog");
    const overlay = panel.parentElement as HTMLElement;
    expect(tools()).toHaveAttribute("aria-expanded", "false");

    // Dropping on the backdrop is outside the panel: nothing happens.
    dropFiles(overlay, [new File(["x"], "ignored.txt", { type: "text/plain" })]);
    expect(tools()).toHaveAttribute("aria-expanded", "false");

    // Dropping on the panel reveals Tools & files, exactly as before.
    dropFiles(panel, [new File(["x"], "notes.txt", { type: "text/plain" })]);
    expect(tools()).toHaveAttribute("aria-expanded", "true");
  });

  it("names each Advanced switch by its title, with the paragraph as a description", () => {
    renderModal();
    fireEvent.click(screen.getByRole("button", { name: /Advanced/ }));
    // Exact-name lookups: before the fix the accessible name was the title AND
    // the whole explanatory paragraph run together.
    const network = screen.getByRole("checkbox", { name: "Allow network egress" });
    expect(network).toHaveAccessibleName("Allow network egress");
    expect(network).toHaveAccessibleDescription(
      "Off = sealed — the sandbox has no internet access.",
    );
    for (const name of [
      "Persistent memory · Captain's Log",
      "Allow sub-agent delegation",
      "Carry context across runs",
    ]) {
      expect(screen.getByRole("checkbox", { name })).toHaveAccessibleName(name);
    }
  });

  it("exposes the cost figure as a focusable button describing the full forecast", async () => {
    estimateTask.mockResolvedValue({
      model: "test/model",
      estimated_prompt_tokens: 1200,
      system_prompt_tokens: 800,
      tool_definitions_tokens: 400,
      avg_output_tokens: 500,
      max_iterations: 12,
      pricing_known: true,
      per_iteration_cost_usd: 0.01,
      estimated_total_cost_usd: 0.12,
      estimated_total_cost_range: { min: 0.08, max: 0.2 },
      max_cost_ceiling_usd: 5,
      would_hit_ceiling: false,
      note: "",
    });
    renderModal();
    fireEvent.change(screen.getByLabelText("Prompt"), { target: { value: "Do the thing" } });
    fireEvent.click(screen.getByRole("button", { name: "Estimate cost" }));

    const figure = await screen.findByRole("button", { name: /per run/ });
    expect(figure).toHaveAttribute("type", "button");
    // Focusable as a real control rather than via a tabIndex on a <span>…
    figure.focus();
    expect(figure).toHaveFocus();
    // …and the hover-only breakdown is reachable without hovering.
    expect(figure).toHaveAccessibleDescription(/Estimated prompt/);
  });
});
