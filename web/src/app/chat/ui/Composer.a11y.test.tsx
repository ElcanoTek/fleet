import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { useEffect, useRef, useState } from "react";
import { Composer } from "./Composer";

// Focus + Escape behavior of the composer's model picker.
//
// The search field used to be focused by `autoFocus`; the jsx-a11y pass moved
// that onto an effect keyed on the picker being open. Escape is still handled
// on the (now presentational) wrapper around the chip + popover, because it has
// to work from the trigger button AND from the search field. Both are behavior
// a linter change could silently take away, so both are asserted here.

const MODELS = [
  { slug: "vendor/one", name: "Model One" },
  { slug: "vendor/two", name: "Model Two" },
];

// A minimal host that owns the state Composer normally gets from
// ChatExperience — enough for the model picker to open, focus and close.
// `overrides` lets a test hand in a tools roster and spy handlers.
function Host({
  overrides,
}: {
  overrides?: Partial<Parameters<typeof Composer>[0]>;
}) {
  const [prompt, setPrompt] = useState("");
  const [modelPickerOpen, setModelPickerOpen] = useState(false);
  const [modelSearchQuery, setModelSearchQuery] = useState("");
  const [selectedModel, setSelectedModel] = useState("vendor/one");
  const [personaPickerOpen, setPersonaPickerOpen] = useState(false);
  const [mcpPickerOpen, setMcpPickerOpen] = useState(false);
  const promptRef = useRef<HTMLTextAreaElement | null>(null);
  const modelPickerRef = useRef<HTMLDivElement | null>(null);
  const modelInputRef = useRef<HTMLInputElement | null>(null);
  const personaPickerRef = useRef<HTMLDivElement | null>(null);
  const mcpPickerRef = useRef<HTMLDivElement | null>(null);
  // Mirrors ChatExperience's outside-click close for the tools popover
  // (same ref check), so a test can prove a control INSIDE the popover keeps
  // it open and a mousedown outside closes it.
  useEffect(() => {
    const onPointerDown = (event: MouseEvent) => {
      const target = event.target;
      if (!(target instanceof Node)) return;
      if (mcpPickerRef.current?.contains(target)) return;
      if (target instanceof Element && target.closest("[data-seat-menu]")) return;
      setMcpPickerOpen(false);
    };
    window.addEventListener("mousedown", onPointerDown);
    return () => window.removeEventListener("mousedown", onPointerDown);
  }, []);
  const fileInputRef = useRef<HTMLInputElement | null>(null);
  const dragCounterRef = useRef(0);
  const activeConversationIdRef = useRef<string | null>(null);
  const abortControllersRef = useRef<Map<string, AbortController>>(new Map());
  const noop = () => {};
  const props = {
    prompt,
    setPrompt,
    promptPlaceholder: "Ask anything",
    promptRef,
    submitPrompt: noop,
    sealed: false,
    isStreaming: false,
    isUploadingAttachments: false,
    isDraggingOver: false,
    setIsDraggingOver: noop,
    dragCounterRef,
    fileInputRef,
    addAttachmentFiles: noop,
    pendingAttachments: [],
    attachmentError: null,
    removePendingAttachment: noop,
    uploadSizeWarning: null,
    spreadsheetNudge: { show: false },
    setSpreadsheetNudgeDismissed: noop,
    personas: ["default"],
    selectedPersona: "default",
    setSelectedPersona: noop,
    personaPickerOpen,
    setPersonaPickerOpen,
    personaPickerRef,
    selectedModel,
    setSelectedModel,
    selectedModelLabel: "Model One",
    selectedModelPrices: null,
    modelError: null,
    modelPickerOpen,
    setModelPickerOpen,
    modelPickerRef,
    modelInputRef,
    modelSearchQuery,
    setModelSearchQuery,
    filteredRankedModels: MODELS,
    isLoadingRankedModels: false,
    isLoadingCatalog: false,
    loadRankedModels: noop,
    loadCatalogModels: noop,
    skills: [],
    mcpServers: [],
    mcpPickerOpen,
    setMcpPickerOpen,
    mcpPickerRef,
    isLoadingMcpServers: false,
    loadMcpServerCatalog: noop,
    toggleMcpServer: noop,
    setMcpServersEnabled: noop,
    mcpCategories: [],
    appName: "Elcano",
    isAdmin: false,
    setMcpServerAccount: noop,
    activeConversationId: null,
    messages: [],
    contextUsage: null,
    isSummarizing: false,
    compactToastVisible: false,
    setConfirmSummarize: noop,
    activeConversationIdRef,
    abortControllersRef,
    isPendingKey: () => false,
  } as unknown as Parameters<typeof Composer>[0];
  return (
    <>
      <Composer {...props} {...overrides} />
      {/* Mirrors the host's selectedModel so a test can assert what the
          picker committed (or, on a dismiss, left alone). */}
      <span data-testid="host-selected-model">{selectedModel}</span>
    </>
  );
}

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe("Composer — model picker focus and Escape", () => {
  it("focuses the search field when the picker opens, and Escape closes it back to the chip", async () => {
    render(<Host />);
    const chip = screen.getByRole("button", { name: /Model One/ });
    expect(screen.queryByRole("combobox", { name: "Model" })).not.toBeInTheDocument();

    fireEvent.click(chip);
    const search = await screen.findByRole("combobox", { name: "Model" });
    // Was autoFocus; now an effect keyed on the picker being open. Same result.
    await waitFor(() => expect(search).toHaveFocus());

    // Escape is delegated to the presentational wrapper so it works from the
    // search field as well as from the chip itself.
    fireEvent.keyDown(search, { key: "Escape" });
    await waitFor(() =>
      expect(screen.queryByRole("combobox", { name: "Model" })).not.toBeInTheDocument(),
    );
    expect(chip).toHaveFocus();
  });
});

// Multi-login seats in the tools popover (#988): a server with labeled seats
// gets a compact Account menu in its row. Choosing from it must reach
// setMcpServerAccount and must NOT flip the row, while the row itself still
// toggles. The default seat reads as the connection's named default, or the
// deployment's name when it has none.
describe("Composer — tools popover seat menu", () => {
  const SEATED = [
    {
      name: "gamma",
      display_name: "Gamma",
      description: "Decks and docs.",
      tools: ["create_deck"],
      tool_count: 1,
      enabled: true,
      accounts: ["personal", "work"],
      default_account: "work",
      remote: true,
    },
    {
      name: "ix",
      display_name: "Index Exchange",
      description: "PMP deals.",
      tools: ["ix_get_deal"],
      tool_count: 1,
      enabled: false,
      accounts: ["OMG_USD", "OMG_GBP"],
    },
    {
      name: "solo",
      description: "No seats.",
      tools: [],
      tool_count: 2,
      enabled: false,
    },
  ];

  it("choosing a seat calls setMcpServerAccount without toggling the row", async () => {
    const toggleMcpServer = vi.fn();
    const setMcpServerAccount = vi.fn();
    render(<Host overrides={{ mcpServers: SEATED, toggleMcpServer, setMcpServerAccount }} />);
    fireEvent.click(screen.getByRole("button", { name: "Tools" }));
    // Only servers with seats get a menu.
    expect(screen.queryByTestId("mcp-seat-solo")).not.toBeInTheDocument();
    const trigger = await screen.findByTestId("mcp-seat-gamma");
    expect(trigger).toHaveTextContent("work");

    fireEvent.click(trigger);
    const list = screen.getByRole("listbox", { name: "Account for Gamma" });
    // The named default is listed once, first, marked Default.
    expect(Array.from(list.querySelectorAll('[role="option"]')).map((o) => o.textContent)).toEqual([
      "workDefault",
      "personal",
    ]);
    expect(toggleMcpServer).not.toHaveBeenCalled();
    fireEvent.mouseDown(screen.getByRole("option", { name: /personal/ }));
    fireEvent.click(screen.getByRole("option", { name: /personal/ }));
    expect(setMcpServerAccount).toHaveBeenCalledWith(null, "gamma", "personal");
    expect(toggleMcpServer).not.toHaveBeenCalled();
    // The popover is still open.
    expect(screen.getByTestId("chat-tools-popover")).toBeInTheDocument();

    // The row's switch is still the toggle it always was.
    fireEvent.click(screen.getByRole("switch", { name: "Gamma" }));
    expect(toggleMcpServer).toHaveBeenCalledWith(null, "gamma");
  });

  it("labels a bundled connector's default seat with the deployment name", async () => {
    render(<Host overrides={{ mcpServers: SEATED }} />);
    fireEvent.click(screen.getByRole("button", { name: "Tools" }));
    const trigger = await screen.findByTestId("mcp-seat-ix");
    expect(trigger).toHaveTextContent("Elcano");
    fireEvent.click(trigger);
    expect(
      Array.from(screen.getByRole("listbox").querySelectorAll('[role="option"]')).map((o) => o.textContent),
    ).toEqual(["ElcanoDefault", "OMG_USD", "OMG_GBP"]);
  });

  it("Escape in the seat menu closes only the menu", async () => {
    render(<Host overrides={{ mcpServers: SEATED }} />);
    fireEvent.click(screen.getByRole("button", { name: "Tools" }));
    fireEvent.click(await screen.findByTestId("mcp-seat-ix"));
    const list = screen.getByRole("listbox");
    fireEvent.keyDown(list, { key: "Escape" });
    expect(screen.queryByRole("listbox")).not.toBeInTheDocument();
    expect(screen.getByTestId("chat-tools-popover")).toBeInTheDocument();
  });
});

// "All on" / "All off" in the tools popover: scoped to the toggleable rows
// shown, never touching always-on rows, and never offered when there is
// nothing to flip. The visible text IS the accessible name (WCAG 2.5.3): no
// aria-label restates it.
describe("Composer — all connectors on / off", () => {
  const ROWS = [
    {
      name: "email",
      display_name: "Email",
      description: "",
      tools: [],
      tool_count: 3,
      enabled: true,
      always_on: true,
    },
    { name: "gamma", description: "", tools: [], tool_count: 2, enabled: true },
    { name: "xandr", description: "", tools: [], tool_count: 5, enabled: false },
  ];
  const allOn = () => screen.getByRole("button", { name: "All on" });
  const allOff = () => screen.getByRole("button", { name: "All off" });

  it("offers both actions, shows the on count, and sends the toggleable rows", () => {
    const setMcpServersEnabled = vi.fn();
    render(<Host overrides={{ mcpServers: ROWS, setMcpServersEnabled }} />);
    fireEvent.click(screen.getByRole("button", { name: "Tools" }));
    // Always-on counts as on.
    expect(screen.getByTestId("chat-mcp-all-actions")).toHaveTextContent("2 on");
    expect(allOn()).not.toHaveAttribute("aria-disabled");
    expect(allOff()).not.toHaveAttribute("aria-disabled");
    fireEvent.click(allOn());
    expect(setMcpServersEnabled).toHaveBeenCalledWith(null, expect.arrayContaining(["gamma", "xandr"]), true);
    fireEvent.click(allOff());
    expect(setMcpServersEnabled).toHaveBeenLastCalledWith(null, expect.arrayContaining(["gamma", "xandr"]), false);
    expect(setMcpServersEnabled.mock.calls[0][1]).toHaveLength(2);
    expect(setMcpServersEnabled).toHaveBeenCalledTimes(2);
  });

  it("marks the action that would change nothing aria-disabled and ignores its click", () => {
    const setMcpServersEnabled = vi.fn();
    const everyOn = ROWS.map((r) => (r.always_on ? r : { ...r, enabled: true }));
    const { unmount } = render(<Host overrides={{ mcpServers: everyOn, setMcpServersEnabled }} />);
    fireEvent.click(screen.getByRole("button", { name: "Tools" }));
    expect(allOn()).toHaveAttribute("aria-disabled", "true");
    expect(allOff()).not.toHaveAttribute("aria-disabled");
    fireEvent.click(allOn());
    expect(setMcpServersEnabled).not.toHaveBeenCalled();
    unmount();

    const everyOff = ROWS.map((r) => (r.always_on ? r : { ...r, enabled: false }));
    render(<Host overrides={{ mcpServers: everyOff, setMcpServersEnabled }} />);
    fireEvent.click(screen.getByRole("button", { name: "Tools" }));
    expect(allOn()).not.toHaveAttribute("aria-disabled");
    expect(allOff()).toHaveAttribute("aria-disabled", "true");
    fireEvent.click(allOff());
    expect(setMcpServersEnabled).not.toHaveBeenCalled();
  });

  it("keeps a no-op action focusable so Escape still closes the popover and returns focus", () => {
    const everyOn = ROWS.map((r) => (r.always_on ? r : { ...r, enabled: true }));
    render(<Host overrides={{ mcpServers: everyOn }} />);
    const trigger = screen.getByRole("button", { name: "Tools" });
    fireEvent.click(trigger);
    const button = allOn();
    expect(button).toHaveAttribute("aria-disabled", "true");
    button.focus();
    expect(document.activeElement).toBe(button);
    fireEvent.keyDown(button, { key: "Escape" });
    expect(screen.queryByTestId("chat-tools-popover")).not.toBeInTheDocument();
    expect(document.activeElement).toBe(trigger);
  });

  it("stays open on a mousedown inside the bar and closes on one outside", () => {
    render(<Host overrides={{ mcpServers: ROWS }} />);
    fireEvent.click(screen.getByRole("button", { name: "Tools" }));
    fireEvent.mouseDown(allOff());
    expect(screen.getByTestId("chat-tools-popover")).toBeInTheDocument();
    fireEvent.mouseDown(document.body);
    expect(screen.queryByTestId("chat-tools-popover")).not.toBeInTheDocument();
  });

  it("is not offered when every row is always-on", () => {
    render(<Host overrides={{ mcpServers: ROWS.filter((r) => r.always_on) }} />);
    fireEvent.click(screen.getByRole("button", { name: "Tools" }));
    expect(screen.getByTestId("chat-mcp-always-on-email")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "All on" })).not.toBeInTheDocument();
  });
});

// The redesigned popover: search pinned and focused, the "On" section first,
// static category headings, ranked search, dimmed needs-setup rows, and
// Escape that clears before it closes.
describe("Composer — tools popover search and grouping", () => {
  const CATS = [
    { id: "ssps", name: "SSPs" },
    { id: "deal-management", name: "Deal Management" },
    { id: "misc", name: "Misc Tools" },
  ];
  const ROWS = [
    {
      name: "ix",
      display_name: "Index Exchange",
      description: "Create and edit PMP deals.",
      tools: ["ix_get_deal", "ix_clone_deal"],
      tool_count: 2,
      enabled: true,
      category: "ssps",
    },
    {
      name: "magnite",
      display_name: "Magnite",
      description: "Streaming and DV+ deals.",
      tools: ["mg_get_deal"],
      tool_count: 1,
      enabled: false,
      category: "ssps",
    },
    {
      name: "dealsheet",
      display_name: "Deal Sheet",
      description: "Branded XLSX deal sheets.",
      tools: [],
      tool_count: 12,
      enabled: true,
      always_on: true,
      category: "deal-management",
    },
    {
      name: "bq",
      display_name: "BigQuery Reporting",
      description: "Query the reporting warehouse.",
      tools: ["bq_query"],
      tool_count: 1,
      enabled: false,
      category: "misc",
      status: "needs_setup" as const,
    },
  ];

  const open = (overrides = {}) => {
    render(<Host overrides={{ mcpServers: ROWS, mcpCategories: CATS, ...overrides }} />);
    fireEvent.click(screen.getByRole("button", { name: "Tools" }));
    return screen.getByRole("textbox", { name: "Search tools" });
  };
  const groups = () => screen.getAllByRole("group").map((g) => g.getAttribute("aria-label"));

  it("focuses search, pins the On section, then static headings, hiding needs-setup", async () => {
    const search = open();
    await waitFor(() => expect(search).toHaveFocus());
    expect(groups()).toEqual(["On", "SSPs"]);
    expect(screen.getByTestId("chat-tools-row-ix")).toBeInTheDocument();
    expect(screen.queryByTestId("chat-tools-row-bq")).not.toBeInTheDocument();
  });

  it("the badge counts always-on and holds while open", () => {
    const toggleMcpServer = vi.fn();
    render(<Host overrides={{ mcpServers: ROWS, mcpCategories: CATS, toggleMcpServer }} />);
    expect(screen.getByTestId("chat-tools-badge")).toHaveTextContent("2");
    expect(screen.getByRole("button", { name: "Tools" })).toHaveAttribute("data-tip-top", "Tools, 2 on");
    // Open, the tooltip would sit over the menu (and stick after a tap).
    fireEvent.click(screen.getByRole("button", { name: "Tools" }));
    expect(screen.getByRole("button", { name: "Tools" })).not.toHaveAttribute("data-tip-top");
  });

  it("search reveals needs-setup dimmed with member copy, and shows inner tool hits", () => {
    const search = open();
    fireEvent.change(search, { target: { value: "bigquery" } });
    expect(screen.getByTestId("chat-tools-row-bq")).toHaveTextContent("Needs admin setup");
    expect(screen.queryByRole("switch", { name: "BigQuery Reporting" })).not.toBeInTheDocument();

    fireEvent.change(search, { target: { value: "clone" } });
    expect(screen.getByTestId("chat-tools-row-ix")).toHaveTextContent("↳ ix_clone_deal");
    expect(screen.getByTestId("chat-mcp-all-actions")).toHaveTextContent("1 result · 2 on");
    expect(screen.getByRole("button", { name: "All 1 on" })).toHaveAttribute("title", "Applies to 1 connection shown");
  });

  it("admins get Set up › which opens the library on that connection", () => {
    const search = open({ isAdmin: true });
    fireEvent.change(search, { target: { value: "bigquery" } });
    fireEvent.click(screen.getByRole("button", { name: /Set up/ }));
    expect(screen.queryByTestId("chat-tools-popover")).not.toBeInTheDocument();
    const detail = screen.getByTestId("chat-tools-library-detail");
    expect(detail).toHaveTextContent("BigQuery Reporting");
    expect(detail).toHaveTextContent("Needs setup before it can be used.");
    expect(screen.getByRole("link", { name: /Set up in Settings/ })).toHaveAttribute(
      "href",
      "/settings/connections#connector-bq",
    );
  });

  it("arrow keys move the active row and Enter toggles it", () => {
    const toggleMcpServer = vi.fn();
    const search = open({ toggleMcpServer });
    fireEvent.change(search, { target: { value: "magn" } });
    fireEvent.keyDown(search, { key: "Enter" });
    expect(toggleMcpServer).toHaveBeenCalledWith(null, "magnite");
  });

  it("Escape clears the search first, then closes", () => {
    const search = open();
    fireEvent.change(search, { target: { value: "ix" } });
    fireEvent.keyDown(search, { key: "Escape" });
    expect(search).toHaveValue("");
    expect(screen.getByTestId("chat-tools-popover")).toBeInTheDocument();
    fireEvent.keyDown(search, { key: "Escape" });
    expect(screen.queryByTestId("chat-tools-popover")).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Tools" })).toHaveFocus();
  });

  it("no results offers the library and clear search", () => {
    const search = open();
    fireEvent.change(search, { target: { value: "xyz" } });
    expect(screen.getByText("No tools match ‘xyz’")).toBeInTheDocument();
    fireEvent.click(screen.getByText("Clear search"));
    expect(search).toHaveValue("");
  });

  it("the library counts every connection and filters by status", () => {
    open({ isAdmin: true });
    fireEvent.click(screen.getByRole("button", { name: "Open library" }));
    const library = screen.getByTestId("chat-tools-library");
    expect(library).toHaveTextContent("2 of 4 on");
    expect(screen.getByRole("link", { name: /Manage connections in Settings/ })).toBeInTheDocument();
    fireEvent.click(within(screen.getByRole("navigation", { name: "Status" })).getByRole("button", { name: /Needs setup/ }));
    expect(screen.getByTestId("chat-tools-library-row-bq")).toBeInTheDocument();
    expect(screen.queryByTestId("chat-tools-library-row-ix")).not.toBeInTheDocument();
  });

  it("members do not see the admin Settings link in the library", () => {
    open();
    fireEvent.click(screen.getByRole("button", { name: "Open library" }));
    expect(screen.queryByRole("link", { name: /Manage connections in Settings/ })).not.toBeInTheDocument();
  });
});

describe("Composer — per-chat Auto-approve toggle", () => {
  it("renders the Auto-approve toggle with aria-pressed and remains clickable while streaming", () => {
    const toggleAutoApprove = vi.fn();
    render(
      <Host
        overrides={{
          isAutoApprove: true,
          toggleAutoApprove,
          isStreaming: true,
        }}
      />,
    );
    const btn = screen.getByRole("button", {
      name: /Auto-approve tools in this chat is on/,
    });
    expect(btn).toHaveAttribute("aria-pressed", "true");
    expect(btn).not.toBeDisabled();
    fireEvent.click(btn);
    expect(toggleAutoApprove).toHaveBeenCalledTimes(1);
  });
});
