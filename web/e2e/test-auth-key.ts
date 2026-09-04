import crypto from "node:crypto";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";

// Runtime-generated, throwaway Ed25519 keypair for the mocked Playwright suite.
//
// Why a file? Playwright loads playwright.config.ts in the MAIN process (to read
// webServer.env) AND re-imports it in every worker process (which runs the
// specs + e2e/mocked/_session.ts). If we generated a keypair in module scope, a
// worker would mint elcano_auth cookies with a DIFFERENT private key than the
// public key the server was started with — the Ed25519 login would never
// verify. So the keypair is generated exactly ONCE per `playwright test` run and
// persisted to a throwaway file outside the repo; every process reads the same
// material from it:
//   - the PUBLIC key (standard base64 of the raw 32 bytes) → AUTH_SIGNING_PUBKEY
//     for the Next server (mirrors auth-admin keygen / home/server.js).
//   - the PRIVATE key (PKCS8 PEM) → e2e/mocked/_session.ts, to mint a token the
//     real verifier (verifyElcanoToken) accepts.
// NO key literal is committed to the repo, and neither value protects anything
// real. Each main process owns a private directory; its workers inherit the
// exact path through the environment. Concurrent Playwright/demo runs cannot
// replace each other's signing material.
const KEY_FILE_ENV = "FLEET_E2E_AUTH_KEY_FILE";
let cachedMaterial: TestAuthKeyMaterial | undefined;

type TestAuthKeyMaterial = {
  // AUTH_SIGNING_PUBKEY: standard base64 of the raw 32-byte Ed25519 public key.
  pubkeyStdB64: string;
  // PKCS8 PEM of the matching private key (signer side).
  privateKeyPem: string;
};

function rawPublicKeyStdBase64(publicKey: crypto.KeyObject): string {
  const jwk = publicKey.export({ format: "jwk" }) as { x?: string };
  if (!jwk.x) throw new Error("expected an Ed25519 public JWK with an `x` member");
  return Buffer.from(jwk.x, "base64url").toString("base64");
}

function generateTestAuthKey(keyFile: string): TestAuthKeyMaterial {
  const { publicKey, privateKey } = crypto.generateKeyPairSync("ed25519");
  const material: TestAuthKeyMaterial = {
    pubkeyStdB64: rawPublicKeyStdBase64(publicKey),
    privateKeyPem: privateKey.export({ format: "pem", type: "pkcs8" }).toString(),
  };
  fs.writeFileSync(keyFile, JSON.stringify(material), {
    encoding: "utf8",
    mode: 0o600,
    flag: "wx",
  });
  return material;
}

export function getTestAuthKey(): TestAuthKeyMaterial {
  if (cachedMaterial) return cachedMaterial;
  // TEST_WORKER_INDEX is set only in Playwright spec workers. A missing run
  // file is an initialization error: generating a substitute would silently
  // sign cookies with a key the already-started server cannot verify.
  if (process.env.TEST_WORKER_INDEX !== undefined) {
    const keyFile = process.env[KEY_FILE_ENV];
    if (!keyFile) throw new Error(`Playwright worker requires ${KEY_FILE_ENV}`);
    const parsed = JSON.parse(fs.readFileSync(keyFile, "utf8")) as TestAuthKeyMaterial;
    if (!parsed.pubkeyStdB64 || !parsed.privateKeyPem) {
      throw new Error("Invalid Playwright authentication key material");
    }
    cachedMaterial = parsed;
    return parsed;
  }

  const directory = fs.mkdtempSync(path.join(os.tmpdir(), "fleet-e2e-auth-"));
  fs.chmodSync(directory, 0o700);
  const keyFile = path.join(directory, "key.json");
  cachedMaterial = generateTestAuthKey(keyFile);
  process.env[KEY_FILE_ENV] = keyFile;
  // The main Playwright process outlives its workers and web server. Normal
  // completion removes only this run's directory, never another run's keys.
  process.once("exit", () => fs.rmSync(directory, { recursive: true, force: true }));
  return cachedMaterial;
}
