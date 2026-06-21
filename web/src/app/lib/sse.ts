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
