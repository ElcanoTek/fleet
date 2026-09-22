import { describe, expect, it, vi } from "vitest";
import {
  createRecoveryElection,
  type RecoveryLockManager,
  type RecoveryRelayChannel,
} from "./recoveryElection";

// The election that lets one tab wait out a recovery outage for all of them
// (#1595). Everything here is driven through injected fakes, because the two
// platform primitives it rests on are exactly the two a test environment does
// not have: Web Locks needs a secure context, and a BroadcastChannel in one
// process cannot model a tab that dies without closing.

const CONV = "conv-1";

// A released Web Lock is granted to the next waiter a microtask later — the
// holder releases by resolving a promise — so anything that asserts on a
// hand-over has to let the queue drain first.
const settle = (): Promise<void> =>
  new Promise<void>((resolve) => setTimeout(resolve, 0));

// A lock manager with the one property the whole design rests on: a lock held
// by a tab that goes away is released without that tab's cooperation, and the
// next waiter is granted it. `kill` is that, and nothing else can model it.
const makeLockBus = () => {
  type Waiter = { callback: () => Promise<void>; signal?: AbortSignal };
  const waiting = new Map<string, Waiter[]>();
  const held = new Map<string, number>();
  let tokens = 0;

  const pump = (name: string): void => {
    if (held.has(name)) return;
    const queue = waiting.get(name) ?? [];
    while (queue.length > 0) {
      const waiter = queue.shift();
      if (!waiter || waiter.signal?.aborted) continue;
      const token = ++tokens;
      held.set(name, token);
      void waiter.callback().then(() => {
        // A holder that resolves after being killed is releasing a lock some
        // other tab now owns; ignore it, exactly as the browser would.
        if (held.get(name) !== token) return;
        held.delete(name);
        pump(name);
      });
      return;
    }
  };

  const manager: RecoveryLockManager = {
    request: (name, options, callback) => {
      const queue = waiting.get(name) ?? [];
      queue.push({ callback, signal: options.signal });
      waiting.set(name, queue);
      pump(name);
      return new Promise((_resolve, reject) => {
        options.signal?.addEventListener("abort", () => {
          reject(new DOMException("aborted", "AbortError"));
        });
      });
    },
  };

  return {
    manager,
    holders: () => [...held.keys()],
    // The tab holding this lock is gone.
    kill: (name: string) => {
      if (!held.delete(name)) return;
      pump(name);
    },
  };
};

// A BroadcastChannel's defining behavior: every other channel of the same name
// hears the message, and the sender does not hear its own.
const makeRelayBus = () => {
  const listeners = new Set<(message: unknown) => void>();
  const open = (): RecoveryRelayChannel => {
    let mine: ((message: unknown) => void) | null = null;
    return {
      postMessage: (message: unknown) => {
        for (const listener of [...listeners]) {
          if (listener !== mine) listener(message);
        }
      },
      listen: (handler: (message: unknown) => void) => {
        if (mine) listeners.delete(mine);
        mine = handler;
        listeners.add(handler);
      },
      close: () => {
        if (mine) listeners.delete(mine);
        mine = null;
      },
    };
  };
  return { open, count: () => listeners.size };
};

// A clock the test steps by hand, so "the beat keeps the deadline it was
// booked with" is an assertion rather than a race.
const makeClock = () => {
  let now = 0;
  let nextId = 1;
  const timers = new Map<number, { at: number; run: () => void }>();
  return {
    now: () => now,
    setTimer: (run: () => void, ms: number): number => {
      const id = nextId++;
      timers.set(id, { at: now + ms, run });
      return id;
    },
    clearTimer: (id: number): void => {
      timers.delete(id);
    },
    advance: (ms: number): void => {
      const until = now + ms;
      for (;;) {
        let due: [number, { at: number; run: () => void }] | null = null;
        for (const entry of timers) {
          if (entry[1].at <= until && (due === null || entry[1].at < due[1].at))
            due = entry;
        }
        if (due === null) break;
        timers.delete(due[0]);
        now = due[1].at;
        due[1].run();
      }
      now = until;
    },
  };
};

const makeTab = (
  bus: ReturnType<typeof makeLockBus>,
  relay: ReturnType<typeof makeRelayBus>,
  clock: ReturnType<typeof makeClock>,
  opts: { hidden?: () => boolean } = {},
) =>
  createRecoveryElection({
    locks: bus.manager,
    openChannel: relay.open,
    isHidden: opts.hidden ?? (() => false),
    onVisibilityChange: () => () => {},
    now: clock.now,
    setTimer: clock.setTimer,
    clearTimer: clock.clearTimer,
  });

describe("the election only exists where the primitives can keep it", () => {
  it("is independent when navigator.locks is absent", () => {
    // jsdom has no navigator.locks, which is also what an insecure context
    // gives a real browser. Nothing is claimed there: every tab keeps its own
    // chain, which is correct and merely duplicative.
    const election = createRecoveryElection();
    expect(election.mode).toBe("independent");
    expect(election.book(CONV, 1000, () => {})).toBe("local");
    expect(election.booked(CONV)).toBe(false);
    election.close();
  });

  it("is independent when the relay channel cannot be opened", () => {
    // A lock without a relay would elect a leader that cannot tell the others
    // anything — the frozen-spinner failure. Both halves or neither.
    const bus = makeLockBus();
    const election = createRecoveryElection({
      locks: bus.manager,
      openChannel: () => null,
    });
    expect(election.mode).toBe("independent");
    expect(election.book(CONV, 1000, () => {})).toBe("local");
    expect(bus.holders()).toEqual([]);
    election.close();
  });
});

describe("a tab stands down only for an outage it has seen itself", () => {
  it("books its own beat until its own probe fails to reach the server", () => {
    const bus = makeLockBus();
    const relay = makeRelayBus();
    const clock = makeClock();
    const tab = makeTab(bus, relay, clock);

    // No probe has failed yet: this tab is resolving its own slot, which no
    // other tab can do for it.
    expect(tab.book(CONV, 1000, () => {})).toBe("local");
    expect(bus.holders()).toEqual([]);

    tab.report(CONV, false);
    expect(tab.book(CONV, 1000, () => {})).toBe("local");
    // …because it is now the tab doing the asking.
    expect(bus.holders()).toHaveLength(1);

    tab.close();
  });

  it("parks the beat of a second tab and runs it on the leader's answer", () => {
    const bus = makeLockBus();
    const relay = makeRelayBus();
    const clock = makeClock();
    const leader = makeTab(bus, relay, clock);
    const follower = makeTab(bus, relay, clock);

    leader.report(CONV, false);
    expect(leader.book(CONV, 30_000, () => {})).toBe("local");

    const ran: number[] = [];
    follower.report(CONV, false);
    expect(follower.book(CONV, 30_000, () => ran.push(clock.now()))).toBe(
      "elected",
    );
    expect(follower.booked(CONV)).toBe(true);

    // The leader keeps finding the server unreachable: there is nothing to
    // tell anyone, and the follower spends nothing.
    clock.advance(120_000);
    leader.report(CONV, false);
    expect(ran).toEqual([]);

    // The leader reaches the server. The follower does not inherit what it
    // learned — it is simply woken, at once, to go and ask for itself.
    leader.report(CONV, true);
    expect(ran).toEqual([120_000]);
    expect(follower.booked(CONV)).toBe(false);

    leader.close();
    follower.close();
  });

  it("puts a woken tab back on its own beat", () => {
    const bus = makeLockBus();
    const relay = makeRelayBus();
    const clock = makeClock();
    const leader = makeTab(bus, relay, clock);
    const follower = makeTab(bus, relay, clock);

    leader.report(CONV, false);
    leader.book(CONV, 30_000, () => {});
    follower.report(CONV, false);
    follower.book(CONV, 30_000, () => {});
    leader.report(CONV, true);

    // The relay cleared the follower's outage, so its next beat is its own
    // again: from here it resolves its own slot on its own ladder.
    expect(follower.book(CONV, 1000, () => {})).toBe("local");

    leader.close();
    follower.close();
  });

  it("ignores a relay for another conversation, and anything malformed", () => {
    const bus = makeLockBus();
    const relay = makeRelayBus();
    const clock = makeClock();
    const leader = makeTab(bus, relay, clock);
    const follower = makeTab(bus, relay, clock);
    const other = relay.open();
    other.listen(() => {});

    leader.report(CONV, false);
    leader.book(CONV, 30_000, () => {});
    let ran = 0;
    follower.report(CONV, false);
    follower.book(CONV, 30_000, () => {
      ran += 1;
    });

    other.postMessage({ kind: "fleet.chat.recovery.answered", conv: "other" });
    other.postMessage("not an object");
    other.postMessage({ kind: "something-else", conv: CONV });
    expect(ran).toBe(0);

    leader.report(CONV, true);
    expect(ran).toBe(1);

    other.close();
    leader.close();
    follower.close();
  });
});

describe("a tab that dies has its beat picked up", () => {
  it("runs the parked beat when the lock comes to it", () => {
    const bus = makeLockBus();
    const relay = makeRelayBus();
    const clock = makeClock();
    const leader = makeTab(bus, relay, clock);
    const follower = makeTab(bus, relay, clock);

    leader.report(CONV, false);
    leader.book(CONV, 30_000, () => {});
    let ran = 0;
    follower.report(CONV, false);
    expect(
      follower.book(CONV, 30_000, () => {
        ran += 1;
      }),
    ).toBe("elected");

    clock.advance(10_000);
    expect(ran).toBe(0);

    // The leading tab is gone — crashed, closed, navigated away. It never got
    // to say so; the browser released its lock.
    bus.kill("fleet.chat.recovery:conv-1");

    // The beat keeps the deadline it was booked with rather than restarting
    // the wait it has already served: 20s of its 30s remain.
    clock.advance(19_999);
    expect(ran).toBe(0);
    clock.advance(1);
    expect(ran).toBe(1);

    leader.close();
    follower.close();
  });

  it("frees the lock when the leading tab's chain ends", async () => {
    const bus = makeLockBus();
    const relay = makeRelayBus();
    const clock = makeClock();
    const leader = makeTab(bus, relay, clock);
    const follower = makeTab(bus, relay, clock);

    leader.report(CONV, false);
    leader.book(CONV, 30_000, () => {});
    let ran = 0;
    follower.report(CONV, false);
    follower.book(CONV, 30_000, () => {
      ran += 1;
    });

    leader.leave(CONV);
    await settle();
    // Granted straight away, and its beat's deadline has already passed.
    clock.advance(30_000);
    expect(ran).toBe(1);

    leader.close();
    follower.close();
  });

  it("leads itself when the lock manager refuses", () => {
    // A refusal must degrade to what a tab did before this file existed.
    // Waiting on a lock that will never be granted would strand the chain.
    const relay = makeRelayBus();
    const clock = makeClock();
    const election = createRecoveryElection({
      locks: {
        request: () => {
          throw new Error("no locks for you");
        },
      },
      openChannel: relay.open,
      isHidden: () => false,
      onVisibilityChange: () => () => {},
      now: clock.now,
      setTimer: clock.setTimer,
      clearTimer: clock.clearTimer,
    });

    election.report(CONV, false);
    let ran = 0;
    expect(
      election.book(CONV, 30_000, () => {
        ran += 1;
      }),
    ).toBe("local");
    expect(ran).toBe(0);
    election.close();
  });
});

describe("a hidden tab does not stand for election", () => {
  it("keeps its own (free) beat and holds no lock", async () => {
    const bus = makeLockBus();
    const relay = makeRelayBus();
    const clock = makeClock();
    let hidden = false;
    const tab = makeTab(bus, relay, clock, { hidden: () => hidden });

    tab.report(CONV, false);
    tab.book(CONV, 30_000, () => {});
    expect(bus.holders()).toHaveLength(1);

    hidden = true;
    // A hidden tab's chain reschedules without probing, so it has no business
    // holding the lock away from a tab that can actually ask.
    expect(tab.book(CONV, 30_000, () => {})).toBe("local");
    await settle();
    expect(bus.holders()).toEqual([]);

    tab.close();
  });

  it("hands the lock over and rejoins its own ladder when it hides", async () => {
    const bus = makeLockBus();
    const relay = makeRelayBus();
    const clock = makeClock();
    let hidden = false;
    let onHide = () => {};
    const leader = createRecoveryElection({
      locks: bus.manager,
      openChannel: relay.open,
      isHidden: () => hidden,
      onVisibilityChange: (listener) => {
        onHide = listener;
        return () => {};
      },
      now: clock.now,
      setTimer: clock.setTimer,
      clearTimer: clock.clearTimer,
    });
    const other = makeTab(bus, relay, clock);

    leader.report(CONV, false);
    let leaderRan = 0;
    leader.book(CONV, 30_000, () => {
      leaderRan += 1;
    });
    let otherRan = 0;
    other.report(CONV, false);
    expect(
      other.book(CONV, 30_000, () => {
        otherRan += 1;
      }),
    ).toBe("elected");

    hidden = true;
    onHide();
    await settle();
    // The lock moved to the tab that can still ask…
    clock.advance(30_000);
    expect(otherRan).toBe(1);
    // …and the hidden tab kept nothing parked: its beat went back to itself.
    expect(leader.booked(CONV)).toBe(false);
    expect(leaderRan).toBe(0);

    leader.close();
    other.close();
  });
});

describe("the tab a person is looking at never waits on another tab", () => {
  it("takes its next beat back on a nudge", () => {
    const bus = makeLockBus();
    const relay = makeRelayBus();
    const clock = makeClock();
    const leader = makeTab(bus, relay, clock);
    const follower = makeTab(bus, relay, clock);

    leader.report(CONV, false);
    leader.book(CONV, 30_000, () => {});
    follower.report(CONV, false);
    expect(follower.book(CONV, 30_000, () => {})).toBe("elected");

    follower.claimNextBeat(CONV);
    expect(follower.book(CONV, 1000, () => {})).toBe("local");
    expect(follower.booked(CONV)).toBe(false);

    leader.close();
    follower.close();
  });
});

describe("closing", () => {
  it("drops the lock, the beat and the relay listener", async () => {
    const bus = makeLockBus();
    const relay = makeRelayBus();
    const clock = makeClock();
    const tab = makeTab(bus, relay, clock);

    tab.report(CONV, false);
    tab.book(CONV, 30_000, () => {});
    expect(bus.holders()).toHaveLength(1);
    expect(relay.count()).toBe(1);

    tab.close();
    await settle();
    expect(bus.holders()).toEqual([]);
    expect(relay.count()).toBe(0);
    expect(tab.book(CONV, 1000, () => {})).toBe("local");
  });

  it("survives a relay channel that cannot post", () => {
    // A relay that fails costs the other tabs a wake-up, not an outcome.
    const bus = makeLockBus();
    const clock = makeClock();
    const post = vi.fn(() => {
      throw new Error("channel closing");
    });
    const tab = createRecoveryElection({
      locks: bus.manager,
      openChannel: () => ({ postMessage: post, listen: () => {}, close: () => {} }),
      isHidden: () => false,
      onVisibilityChange: () => () => {},
      now: clock.now,
      setTimer: clock.setTimer,
      clearTimer: clock.clearTimer,
    });

    tab.report(CONV, false);
    expect(() => tab.report(CONV, true)).not.toThrow();
    expect(post).toHaveBeenCalled();
    tab.close();
  });
});
