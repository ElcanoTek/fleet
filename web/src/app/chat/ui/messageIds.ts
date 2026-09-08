// Message ids are numbers that read as timestamps — Date.now() at creation —
// but they are also the KEY every patch is applied by (patchAssistantMessage,
// the user.message adjacency dedup, settleStreamedSlot), so within a
// conversation they must be unique. Date.now() alone is not: two messages
// created in the same millisecond collide, and an id-keyed patch then lands on
// both rows. That is not hypothetical — a queued follow-up turn attaches the
// instant the previous one ends, and under fake timers the clock never moves.
// Seen as a flaky useTurnStream.queueDrain test: the second turn's assistant
// slot got the first turn's id, its user bubble was deduped against the wrong
// neighbour, and its deltas were appended to both slots.
//
// allocMessageIds hands out `count` consecutive ids that are never smaller
// than the clock and always greater than anything issued before, so ids keep
// reading as timestamps and a caller that needs a (user, assistant) pair gets
// base and base+1 — the "user bubble is assistantId - 1" convention stays valid.

let lastIssued = 0;

export const allocMessageIds = (count = 1): number => {
  const base = Math.max(Date.now(), lastIssued + 1);
  lastIssued = base + count - 1;
  return base;
};
