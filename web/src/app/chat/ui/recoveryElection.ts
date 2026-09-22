// One tab at a time waits out a chat-recovery outage (#1595).
//
// Two tabs open on the same conversation used to run two independent recovery
// chains: both probed /inflight on the same ladder, both reattached, both
// adopted the same canonical transcript. Correct — every path is idempotent
// and the server is the single source of truth — but during a long outage it
// is twice the requests for an answer that has not changed.
//
// What is deduplicated here is EXACTLY the waiting, and nothing else. While a
// tab's own last probe could not reach the server, every tab would learn the
// same nothing, so one of them asks on the others' behalf. The moment any tab
// gets an ANSWER — its own, or one another tab relays — every tab goes back to
// its own ladder, because from there recovery stops being a shared question:
// each tab has its own slot, its own submission id (#1592), its own attach and
// its own transcript to fill. So a tab is never left waiting on another tab
// for content it could have fetched itself.
//
// Three properties this module is built to keep.
//
//   * **A tab that stands down is never worse off than one that does not.**
//     It stands down only after ITS OWN probe proved the server unreachable —
//     the state in which its ladder can only re-learn "still nothing" — and it
//     is put back to work by any of: a relayed answer from the tab that is
//     asking, the lock arriving (that tab finished or died), the user coming
//     back to this tab, or this tab going hidden, where its ladder deliberately
//     spends nothing anyway. The relay is a WAKE-UP, never a verdict: a woken
//     tab asks the server itself and applies its own rules to the reply, so
//     nothing here can settle, adopt or fail a turn on another tab's say-so.
//
//   * **A leader that dies is picked up.** Election is `navigator.locks`
//     precisely because the browser releases the lock when the holding tab
//     goes away — crash, close, or navigate. The next tab in the queue is
//     granted it and runs the beat it had parked.
//
//   * **No guarantee is claimed that the primitive cannot keep.**
//     `navigator.locks` is secure-context-only and `BroadcastChannel` can be
//     absent; without BOTH, `mode` is `"independent"` and every tab keeps its
//     own chain exactly as it did before this file existed. A `localStorage`
//     lease is deliberately NOT used as a substitute: it has no atomic
//     compare-and-set, so two tabs can both read a lapsed lease and both write
//     themselves in, and it would promise a single owner it cannot deliver.

// What this tab may do with a conversation's next recovery beat.
//   "local"   — book it yourself, exactly as before this file existed.
//   "elected" — this module has taken the beat; it will run it when the
//               conversation's outage is worth re-asking about.
export type RecoveryBeat = "local" | "elected";

// "independent" is the honest name for "no election here": every tab runs its
// own chain, which is what the page did before #1595 and is still correct.
export type RecoveryElectionMode = "elected" | "independent";

// The slice of `navigator.locks` this uses, named structurally so a test can
// drive the election with a fake lock manager whose held locks it can kill.
export interface RecoveryLockManager {
  request(
    name: string,
    options: { mode?: "exclusive" | "shared"; signal?: AbortSignal },
    callback: () => Promise<void>,
  ): Promise<unknown>;
}

// The slice of `BroadcastChannel` this uses, same reason. `listen` is handed
// the message rather than an event, so a test's relay is two plain objects
// passing each other a value.
export interface RecoveryRelayChannel {
  postMessage(message: unknown): void;
  listen(handler: (message: unknown) => void): void;
  close(): void;
}

export interface RecoveryElectionEnv {
  locks?: RecoveryLockManager | null;
  openChannel?: () => RecoveryRelayChannel | null;
  isHidden?: () => boolean;
  // Returns its own unsubscribe, so `close` leaves no listener behind.
  onVisibilityChange?: (listener: () => void) => () => void;
  now?: () => number;
  setTimer?: (run: () => void, delayMs: number) => number;
  clearTimer?: (handle: number) => void;
}

export interface RecoveryElection {
  readonly mode: RecoveryElectionMode;
  // Book this conversation's next chain tick, `delayMs` from now. "local"
  // means the caller owns the timer as before; "elected" means this module
  // holds the beat and will run it.
  book(convId: string, delayMs: number, run: () => void): RecoveryBeat;
  // True while this module is holding a beat for the conversation — the
  // elected half of "the chain has a next tick booked".
  booked(convId: string): boolean;
  // What this tab's own /inflight probe learned. `false` is what licenses this
  // tab to stand down; `true` puts it back on its own ladder and tells the
  // other tabs there is something to ask about.
  report(convId: string, answered: boolean): void;
  // The user came back to THIS tab: its next beat is its own, whatever another
  // tab is doing.
  claimNextBeat(convId: string): void;
  // The chain is over (settled, adopted, stopped): drop the beat and the lock
  // so a tab still waiting is granted it.
  leave(convId: string): void;
  close(): void;
}

// One channel for the hook; the conversation is named in the message. One lock
// per conversation, because that is the unit tabs contend for.
const relayChannelName = "fleet.chat.recovery";
const relayKind = "fleet.chat.recovery.answered";
const lockNameFor = (convId: string): string =>
  `fleet.chat.recovery:${convId}`;

type Seat = {
  // The beat this tab has parked, and when it was due. The deadline is kept
  // so a tab granted the lock late does not restart the wait it has already
  // served — a takeover from a dead tab runs at once rather than a rung later.
  run: (() => void) | null;
  deadline: number;
  timer: number | undefined;
  // This tab's own last probe could not reach the server. ONLY this licenses
  // standing down: it is the one state in which another tab's answer is as
  // good as our own, because there is no answer.
  outage: boolean;
  // Resolves the granted lock's callback promise — set only while we hold it.
  release: (() => void) | null;
  // Aborts a request still queued — set only while we are waiting for a grant.
  abort: AbortController | null;
  // The lock manager refused us. Leading is what a tab did before #1595, so
  // that is what a refusal degrades to; being stuck is not an option.
  failedOpen: boolean;
};

const browserLocks = (): RecoveryLockManager | null => {
  if (typeof navigator === "undefined") return null;
  // Typed as always-present by lib.dom, absent at runtime outside a secure
  // context — which is exactly the case this has to detect rather than assume.
  const locks = (navigator as Navigator & { locks?: LockManager }).locks;
  if (!locks || typeof locks.request !== "function") return null;
  return {
    request: (name, options, callback) =>
      locks.request(name, options, callback),
  };
};

const browserChannel = (): RecoveryRelayChannel | null => {
  if (typeof BroadcastChannel === "undefined") return null;
  try {
    // Adapted rather than handed over directly: a MessageEvent carries far
    // more than this reads.
    const bus = new BroadcastChannel(relayChannelName);
    return {
      postMessage: (message: unknown) => bus.postMessage(message),
      listen: (handler: (message: unknown) => void) => {
        bus.onmessage = (event: MessageEvent) => handler(event.data);
      },
      close: () => {
        bus.onmessage = null;
        bus.close();
      },
    };
  } catch {
    return null;
  }
};

export function createRecoveryElection(
  env: RecoveryElectionEnv = {},
): RecoveryElection {
  const locks = env.locks !== undefined ? env.locks : browserLocks();
  const openChannel = env.openChannel ?? browserChannel;
  const now = env.now ?? (() => Date.now());
  const setTimer =
    env.setTimer ??
    ((run: () => void, ms: number) => window.setTimeout(run, ms));
  const clearTimer =
    env.clearTimer ?? ((handle: number) => window.clearTimeout(handle));
  const isHidden =
    env.isHidden ??
    (() =>
      typeof document !== "undefined" &&
      document.visibilityState === "hidden");
  const subscribeVisibility =
    env.onVisibilityChange ??
    ((listener: () => void) => {
      if (typeof document === "undefined") return () => {};
      document.addEventListener("visibilitychange", listener);
      return () => document.removeEventListener("visibilitychange", listener);
    });

  // Both halves or neither. Electing without a relay would leave a tab that
  // stood down waiting on a lock release for content the leading tab already
  // has — the frozen-spinner failure this design exists to avoid.
  const channel = locks ? openChannel() : null;
  if (!locks || !channel) {
    const independent: RecoveryElection = {
      mode: "independent",
      book: () => "local",
      booked: () => false,
      report: () => {},
      claimNextBeat: () => {},
      leave: () => {},
      close: () => {
        channel?.close();
      },
    };
    return independent;
  }

  // A Map keyed by conversation id, never an object: the id is remote input,
  // and a computed property on a plain object is the shape CodeQL flags as
  // js/remote-property-injection.
  const seats = new Map<string, Seat>();
  let closed = false;

  const seatFor = (convId: string): Seat => {
    const existing = seats.get(convId);
    if (existing) return existing;
    const seat: Seat = {
      run: null,
      deadline: 0,
      timer: undefined,
      outage: false,
      release: null,
      abort: null,
      failedOpen: false,
    };
    seats.set(convId, seat);
    return seat;
  };

  const clearBeat = (seat: Seat): void => {
    if (seat.timer !== undefined) clearTimer(seat.timer);
    seat.timer = undefined;
    seat.run = null;
  };

  // runBeat hands the parked tick back to the chain. `atOnce` is for the two
  // events that mean "there is something new to learn now" — a relayed answer,
  // and this tab going hidden (where the tick costs nothing and only needs to
  // rejoin its own ladder). Otherwise the beat keeps the deadline it was
  // booked with, so being granted the lock does not reset a wait already
  // served, nor cut one short.
  const runBeat = (seat: Seat, atOnce: boolean): void => {
    const run = seat.run;
    if (!run) return;
    if (seat.timer !== undefined) clearTimer(seat.timer);
    seat.timer = undefined;
    if (atOnce) {
      seat.run = null;
      run();
      return;
    }
    const wait = Math.max(0, seat.deadline - now());
    seat.timer = setTimer(() => {
      seat.timer = undefined;
      seat.run = null;
      run();
    }, wait);
  };

  const dropLock = (seat: Seat): void => {
    const release = seat.release;
    const abort = seat.abort;
    seat.release = null;
    seat.abort = null;
    // Resolving the granted callback's promise is what releases a held lock;
    // aborting the signal is what leaves a queue we are still waiting in.
    if (release) release();
    if (abort) abort.abort();
  };

  // queueForLock keeps exactly one outstanding request per seat: a second
  // would queue behind our own held lock and never be granted.
  const queueForLock = (convId: string, seat: Seat): void => {
    if (closed || seat.release || seat.abort || seat.failedOpen) return;
    const controller = new AbortController();
    seat.abort = controller;
    const failOpen = (): void => {
      seat.failedOpen = true;
      runBeat(seat, false);
    };
    let request: Promise<unknown>;
    try {
      request = locks.request(
        lockNameFor(convId),
        { mode: "exclusive", signal: controller.signal },
        () =>
          new Promise<void>((resolve) => {
            seat.abort = null;
            seat.release = resolve;
            // Granted: either we were first, or the tab that held it finished
            // or died. Run whatever this tab had parked.
            runBeat(seat, false);
          }),
      );
    } catch {
      seat.abort = null;
      failOpen();
      return;
    }
    void request.catch(() => {
      // Our own abort (leave, or the tab going hidden) is ordinary and has
      // already cleared the seat. Anything else is a lock manager that will
      // not answer, and a chain must never hang on it.
      if (seat.abort !== controller) return;
      seat.abort = null;
      failOpen();
    });
  };

  const relay = (convId: string): void => {
    if (closed) return;
    try {
      channel.postMessage({ kind: relayKind, conv: convId });
    } catch {
      // A relay that cannot be sent costs the other tabs a wake-up, not an
      // outcome: they still hold their slots open and still have the lock,
      // the user's return and their own hidden-tab ladder.
    }
  };

  channel.listen((data: unknown) => {
    if (closed) return;
    if (typeof data !== "object" || data === null) return;
    const message = data as { kind?: unknown; conv?: unknown };
    if (message.kind !== relayKind || typeof message.conv !== "string") return;
    const seat = seats.get(message.conv);
    if (!seat) return;
    // Another tab reached the server, so this tab's outage is over as far as
    // waiting goes. It does not inherit what that tab learned — it goes and
    // asks for itself, which is the whole reason a relay can never make a
    // conversation wrong.
    seat.outage = false;
    runBeat(seat, true);
  });

  const unsubscribeVisibility = subscribeVisibility(() => {
    if (closed || !isHidden()) return;
    // A hidden tab's chain deliberately spends nothing (it reschedules without
    // probing), so it has no business holding the lock away from a tab that
    // can actually ask. Hand it over and put every parked beat back on this
    // tab's own — free — ladder.
    for (const seat of seats.values()) {
      dropLock(seat);
      runBeat(seat, true);
    }
  });

  return {
    mode: "elected",
    book(convId, delayMs, run) {
      if (closed) return "local";
      const seat = seatFor(convId);
      clearBeat(seat);
      if (isHidden()) {
        dropLock(seat);
        return "local";
      }
      // Not in an outage: the beat is this tab's own, and it does not stand
      // for election at all — a tab whose probes are being answered is
      // resolving its own slot, which no other tab can do for it.
      if (!seat.outage) return "local";
      queueForLock(convId, seat);
      // Leading (or refused a lock): the beat is this tab's.
      if (seat.release || seat.failedOpen) return "local";
      seat.run = run;
      seat.deadline = now() + delayMs;
      return "elected";
    },
    booked(convId) {
      const seat = seats.get(convId);
      return !!seat && (seat.run !== null || seat.timer !== undefined);
    },
    report(convId, answered) {
      // seatFor, not a lookup: this records what a probe learned about the
      // server, which is true whether or not a beat happens to be booked for
      // the conversation right now. A seat carries no lock until it books one.
      const seat = seatFor(convId);
      seat.outage = !answered;
      if (answered) relay(convId);
    },
    claimNextBeat(convId) {
      const seat = seats.get(convId);
      if (seat) seat.outage = false;
    },
    leave(convId) {
      const seat = seats.get(convId);
      if (!seat) return;
      seats.delete(convId);
      clearBeat(seat);
      dropLock(seat);
    },
    close() {
      if (closed) return;
      closed = true;
      for (const seat of seats.values()) {
        clearBeat(seat);
        dropLock(seat);
      }
      seats.clear();
      unsubscribeVisibility();
      channel.close();
    },
  };
}
