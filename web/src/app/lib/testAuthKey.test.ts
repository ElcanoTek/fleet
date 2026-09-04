// @vitest-environment node
import { afterEach, describe, expect, it } from "vitest";
import { spawn, spawnSync, type ChildProcess } from "node:child_process";
import { once } from "node:events";
import fs from "node:fs";
import path from "node:path";
import { pathToFileURL } from "node:url";

const helper = pathToFileURL(path.resolve("e2e/test-auth-key.ts")).href;
const children: ChildProcess[] = [];
const directories: string[] = [];
type Run = { path: string; publicKey: string };

async function mainRun(): Promise<Run> {
  const env = { ...process.env };
  delete env.TEST_WORKER_INDEX;
  delete env.FLEET_E2E_AUTH_KEY_FILE;
  const child = spawn(process.execPath, ["--input-type=module", "-e", `
    const { getTestAuthKey } = await import(${JSON.stringify(helper)});
    const key = getTestAuthKey();
    if (getTestAuthKey().pubkeyStdB64 !== key.pubkeyStdB64) throw new Error("key changed");
    process.send({ path: process.env.FLEET_E2E_AUTH_KEY_FILE, publicKey: key.pubkeyStdB64 });
    process.on("message", () => process.exit(0));
  `], { env, stdio: ["ignore", "ignore", "inherit", "ipc"] });
  children.push(child);
  const [run] = await once(child, "message") as [Run];
  directories.push(path.dirname(run.path));
  return run;
}

function workerKey(run: Run) {
  const result = spawnSync(process.execPath, ["--input-type=module", "-e", `
    const { getTestAuthKey } = await import(${JSON.stringify(helper)});
    process.stdout.write(getTestAuthKey().pubkeyStdB64);
  `], { encoding: "utf8", env: { ...process.env, TEST_WORKER_INDEX: "0", FLEET_E2E_AUTH_KEY_FILE: run.path } });
  expect(result.status, result.stderr).toBe(0);
  return result.stdout;
}

afterEach(async () => {
  for (const child of children.splice(0)) {
    const exited = once(child, "exit");
    child.send("done");
    await exited;
  }
  for (const directory of directories.splice(0)) fs.rmSync(directory, { recursive: true, force: true });
});

describe("Playwright authentication run isolation", () => {
  it("keeps first-run workers on their original key when another main run starts", async () => {
    const first = await mainRun();
    expect(workerKey(first)).toBe(first.publicKey);
    const second = await mainRun();
    expect(second.path).not.toBe(first.path);
    expect(second.publicKey).not.toBe(first.publicKey);
    expect(workerKey(second)).toBe(second.publicKey);
    expect(workerKey(first)).toBe(first.publicKey);
    expect(fs.statSync(path.dirname(first.path)).mode & 0o777).toBe(0o700);
    expect(fs.statSync(first.path).mode & 0o777).toBe(0o600);
  });

  it("fails rather than replacing a missing worker key", () => {
    const result = spawnSync(process.execPath, ["--input-type=module", "-e", `
      const { getTestAuthKey } = await import(${JSON.stringify(helper)});
      getTestAuthKey();
    `], { encoding: "utf8", env: { ...process.env, TEST_WORKER_INDEX: "0", FLEET_E2E_AUTH_KEY_FILE: "" } });
    expect(result.status).not.toBe(0);
    expect(result.stderr).toContain("Playwright worker requires FLEET_E2E_AUTH_KEY_FILE");
  });
});
