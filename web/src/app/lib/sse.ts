export type ServerEvent = {
  event: string;
  data: string;
  // Monotonic per-turn id assigned server-side. Absent for legacy frames
  // (unlikely, but guard against older servers). Parsed as a string
  // because we never do arithmetic on it and uint64 safely serializes
  // past 2^53; the client only needs equality + "latest wins".
  id?: string;
};

export function parseSseChunk(chunk: string) {
  // Frame delimiter accepts CRLF: our Go server emits LF, but any proxy
  // that normalizes line endings would otherwise produce a stream with
  // no "\n\n" at all — the whole turn would accumulate as remainder and
  // zero events would ever be emitted. (A chunk ending mid-delimiter,
  // e.g. "…\r\n\r", stays in the remainder until the next chunk
  // completes it, so nothing is lost across chunk boundaries.)
  const frames = chunk.split(/\r?\n\r?\n/);
  const completeFrames = frames.slice(0, -1);
  const remainder = frames.at(-1) ?? "";

  const events = completeFrames
    .map((frame): ServerEvent | null => {
      let event = "message";
      let id: string | undefined;
      const data: string[] = [];

      for (const line of frame.split(/\r?\n/)) {
        if (!line || line.startsWith(":")) {
          continue;
        }

        if (line.startsWith("event:")) {
          event = line.slice(6).trim();
          continue;
        }

        if (line.startsWith("id:")) {
          const v = line.slice(3).trim();
          if (v) {
            id = v;
          }
          continue;
        }

        if (line.startsWith("data:")) {
          data.push(line.slice(5).trimStart());
        }
      }

      if (data.length === 0) {
        return null;
      }

      // id is only set on the result object when actually parsed, so
      // the optional-property shape of ServerEvent is preserved and
      // the type predicate below narrows cleanly.
      const out: ServerEvent = { event, data: data.join("\n") };
      if (id !== undefined) {
        out.id = id;
      }
      return out;
    })
    .filter((ev): ev is ServerEvent => ev !== null);

  return { events, remainder };
}

// StreamDedupState is the per-conversation idempotency state the SSE pump
// carries across events: the highest event id applied for the current turn and
// the id of that turn. It is the ref-free, independently-testable core of the
// turn-boundary-reset + monotonic-dedup guard that lives inline in the chat
// component's pumpStreamResponse.
export type StreamDedupState = {
  lastEventId: number;
  currentTurnId: string | undefined;
};

// initialStreamDedupState is the state for a conversation not yet seen (no turn,
// no applied id). lastEventId:0 folds in the `?? 0` default the inline code uses.
export function initialStreamDedupState(): StreamDedupState {
  return { lastEventId: 0, currentTurnId: undefined };
}

// stepStreamDedup reduces one incoming SSE event against the per-conversation
// dedup state and returns the next state plus whether the caller should DROP the
// event (already applied) or APPLY it. It mirrors pumpStreamResponse exactly:
//
//   1. Turn boundary FIRST: a `turn.started` carrying a NEW turn_id resets the
//      monotonic counter to 0, so the fresh turn's id=1 is not dropped against
//      the prior turn's final id. A `turn.started` with no turn_id, or the same
//      turn_id, does not reset.
//   2. Then dedup: an event whose numeric id is <= the last applied id is
//      dropped (the reattach replay overlap). A non-finite or absent id never
//      advances the counter and is always applied.
//
// Order is load-bearing — the reset must run before the dedup check.
export function stepStreamDedup(
  state: StreamDedupState,
  event: Pick<ServerEvent, "event" | "id">,
  payload: unknown,
): { state: StreamDedupState; drop: boolean } {
  let { lastEventId, currentTurnId } = state;

  if (event.event === "turn.started") {
    const p = payload as { turn_id?: string };
    if (p.turn_id && currentTurnId !== p.turn_id) {
      currentTurnId = p.turn_id;
      lastEventId = 0;
    }
  }

  if (event.id !== undefined) {
    const eid = Number(event.id);
    if (Number.isFinite(eid)) {
      if (eid <= lastEventId) {
        return { state: { lastEventId, currentTurnId }, drop: true };
      }
      lastEventId = eid;
    }
  }

  return { state: { lastEventId, currentTurnId }, drop: false };
}
