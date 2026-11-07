import { afterEach, describe, expect, it, vi } from "vitest";
import {
  createSchedule,
  deleteSchedule,
  PatrolApiError,
  runScheduleNow,
  setScheduleAction,
} from "./patrolApi";

const CONV = "11111111-1111-1111-1111-111111111111";

function stub(res: Response) {
  const fetchMock = vi.fn(async (_url: string, _init?: RequestInit) => res);
  vi.stubGlobal("fetch", fetchMock);
  return fetchMock;
}

afterEach(() => vi.unstubAllGlobals());

describe("patrolApi", () => {
  it("creates a schedule with a JSON body and returns the reply", async () => {
    const fetchMock = stub(
      new Response(JSON.stringify({ schedules: [], auto_approve: true }), { status: 200 }),
    );
    const out = await createSchedule(CONV, { title: "t", prompt: "p", cron: "0 9 * * *" });
    expect(out).toEqual({ schedules: [], auto_approve: true });
    const [url, init] = fetchMock.mock.calls[0];
    expect(url).toBe(`/api/conversations/${CONV}/schedules`);
    expect(init?.method).toBe("POST");
    expect(JSON.parse(String(init?.body))).toMatchObject({ title: "t", cron: "0 9 * * *" });
  });

  it("routes each action to its verb and path", async () => {
    const fetchMock = stub(new Response("{}", { status: 200 }));
    await runScheduleNow(CONV, "s1");
    await setScheduleAction(CONV, "s1", "pause");
    await deleteSchedule(CONV, "s1");
    expect(fetchMock.mock.calls.map(([u, i]) => [u, i?.method, i?.body])).toEqual([
      [`/api/conversations/${CONV}/schedules/s1/run-now`, "POST", undefined],
      [`/api/conversations/${CONV}/schedules/s1`, "PATCH", JSON.stringify({ action: "pause" })],
      [`/api/conversations/${CONV}/schedules/s1`, "DELETE", undefined],
    ]);
  });

  it("throws a PatrolApiError with the server's reason and status", async () => {
    stub(new Response("queue full", { status: 409 }));
    const err = await runScheduleNow(CONV, "s1").catch((e: unknown) => e);
    expect(err).toBeInstanceOf(PatrolApiError);
    expect((err as PatrolApiError).status).toBe(409);
    expect((err as PatrolApiError).message).toBe("queue full");
  });

  it("falls back to the caller's text when the body is empty", async () => {
    stub(new Response("", { status: 500 }));
    await expect(setScheduleAction(CONV, "s1", "resume")).rejects.toThrow("Couldn't resume patrol (500)");
  });

  it("never sends a request for an id the URL gate refuses", async () => {
    const fetchMock = stub(new Response("{}", { status: 200 }));
    const err = await deleteSchedule(CONV, "../x").catch((e: unknown) => e);
    expect(err).toBeInstanceOf(PatrolApiError);
    expect((err as PatrolApiError).status).toBe(0);
    expect(fetchMock).not.toHaveBeenCalled();
  });
});
