import type { TurnStreamDeps } from "./useTurnStream";
import type { HistoryEntry, Message } from "./history";

// The real useTurnStream, wired to an in-memory transcript store instead of
// React state — shared by the hook's tests so each one states only what its
// scenario is about (the fetch it answers, the transcript it starts from)
// rather than re-declaring the ~50 dependencies the chat page passes in.
//
// The component's messagesByConvRef is a Map (a conversation id is
// server-issued, so it must not be used as an object property name — CodeQL
// js/remote-property-injection). The harness mirrors that shape.
export type TranscriptStore = Map<string, Message[]>;

export type TurnStreamHarness = {
  deps: TurnStreamDeps;
  store: TranscriptStore;
  // Conversation ids loadConversation was asked for, in order.
  loadConversationCalls: string[];
  // Conversations the hook currently marks as streaming.
  streaming: Set<string>;
};

export function createTurnStreamHarness(opts: {
  conversationId: string | null;
  initial?: Message[];
  // What loadConversation adopts (the persisted transcript).
  persisted?: HistoryEntry[];
  overrides?: Partial<TurnStreamDeps>;
}): TurnStreamHarness {
  const store: TranscriptStore = new Map();
  if (opts.conversationId) store.set(opts.conversationId, opts.initial ?? []);
  const messagesByConvRef = { current: store };
  const loadConversationCalls: string[] = [];
  const streaming = new Set<string>();

  const setConvMessages = (
    convId: string,
    updater: Message[] | ((prev: Message[]) => Message[]),
  ) => {
    const prev = store.get(convId) ?? [];
    store.set(convId, typeof updater === "function" ? updater(prev) : updater);
  };

  const patchAssistantMessage = (
    convId: string,
    assistantId: number,
    updater: (m: Message) => Message,
  ) => {
    store.set(
      convId,
      (store.get(convId) ?? []).map((m) => (m.id === assistantId ? updater(m) : m)),
    );
  };

  const loadConversation = async (convId: string) => {
    loadConversationCalls.push(convId);
    const { historyToMessages } = await import("./history");
    store.set(convId, historyToMessages(opts.persisted ?? []));
  };

  const noop = () => {};
  const asyncNoop = async () => {};

  const deps: TurnStreamDeps = {
    setConvMessages,
    getConvMessages: (convId: string) => store.get(convId) ?? [],
    renameConvKey: noop,
    patchAssistantMessage,
    startThinkingCrossfade: noop,
    refreshConversations: asyncNoop,
    loadConversation,
    loadMemories: asyncNoop,
    loadRankedModels: asyncNoop,
    loadCatalogModels: asyncNoop,
    nextPendingKey: () => "__pending__:1",
    isPendingKey: (key: string | null) => !!key && key.startsWith("__pending__"),
    setPromptForKey: noop,
    setPendingAttachmentsForKey: noop,
    setAttachmentErrorForKey: noop,
    markConvUploading: noop,
    markConvUploadDone: noop,
    getPendingAttachmentsForKey: () => [],
    promoteComposerKey: noop,
    setMessagesByConv: (updater) => {
      const next = typeof updater === "function" ? updater(store) : updater;
      for (const [convId, messages] of next) store.set(convId, messages);
    },
    setConversations: noop,
    setActiveConversationId: noop,
    setSelectedPersona: noop,
    setSelectedModel: noop,
    setModelPickerOpen: noop,
    setModelSearchQuery: noop,
    setPendingLockdown: noop,
    setSidebarOpen: noop,
    setSpreadsheetNudgeDismissed: noop,
    activeConversationIdRef: { current: opts.conversationId },
    messagesByConvRef,
    pendingApprovalScrollRef: { current: null },
    selectedModel: "test-model",
    selectedPersona: "default",
    mcpServers: [],
    pendingLockdown: false,
    userEmail: "tester@example.com",
    modelError: null,
    markConvStreaming: (k: string) => streaming.add(k),
    markConvIdle: (k: string) => streaming.delete(k),
    abortControllersRef: { current: new Map() },
    attachedConvIdsRef: { current: new Set<string>() },
    lastEventIdByConvRef: { current: new Map() },
    currentTurnIdByConvRef: { current: new Map() },
    reattachInFlightRef: { current: new Set<string>() },
    streamPulseRef: { current: new Map() },
    serverHeartbeatMsRef: { current: 0 },
    supersededStreamsRef: { current: new WeakSet<AbortController>() },
    livenessInFlightRef: { current: new Set<string>() },
    promoteStreamKey: noop,
    streamingConvsRef: { current: new Set<string>() },
    isStreaming: false,
    ...opts.overrides,
  };

  return { deps, store, loadConversationCalls, streaming };
}

// A response body that delivers `chunks` and then closes, as a finished turn's
// stream does.
export function closedStream(chunks: string[]): ReadableStream<Uint8Array> {
  const encoder = new TextEncoder();
  let i = 0;
  return new ReadableStream<Uint8Array>({
    pull(controller) {
      if (i < chunks.length) {
        controller.enqueue(encoder.encode(chunks[i++]));
        return;
      }
      controller.close();
    },
  });
}
