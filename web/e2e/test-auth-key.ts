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
// real. globalSetup regenerates the file fresh each run.

// Stable per-run path (one Playwright run reuses one PLAYWRIGHT process tree, so
// the OS temp dir + a fixed name is a reliable rendezvous). The file is created
// atomically by the first process to need it.
const KEY_FILE = path.join(os.tmpdir(), "fleet-e2e-auth-key.json");

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

// generateTestAuthKey makes a fresh keypair and atomically writes it to KEY_FILE,
// overwriting any stale file from a previous run.
export function generateTestAuthKey(): TestAuthKeyMaterial {
  const { publicKey, privateKey } = crypto.generateKeyPairSync("ed25519");
  const material: TestAuthKeyMaterial = {
    pubkeyStdB64: rawPublicKeyStdBase64(publicKey),
    privateKeyPem: privateKey.export({ format: "pem", type: "pkcs8" }).toString(),
  };
  // Atomic write so a worker reading concurrently never sees a half-written
  // file: write to a temp sibling, then rename.
  const tmp = `${KEY_FILE}.${process.pid}.tmp`;
  fs.writeFileSync(tmp, JSON.stringify(material), { encoding: "utf8" });
  fs.renameSync(tmp, KEY_FILE);
  return material;
}

function readTestAuthKey(): TestAuthKeyMaterial | null {
  try {
    const parsed = JSON.parse(fs.readFileSync(KEY_FILE, "utf8")) as TestAuthKeyMaterial;
    if (parsed.pubkeyStdB64 && parsed.privateKeyPem) return parsed;
  } catch {
    // missing or unreadable.
  }
  return null;
}

// Playwright sets TEST_WORKER_INDEX only in spec-worker processes; it is absent
// in the MAIN process that evaluates this config to build webServer.env. We use
// that to make the main process the single generation point each run (fresh
// keypair), while workers + the spawned Next server read the SAME file.
const isWorkerProcess = process.env.TEST_WORKER_INDEX !== undefined;

// getTestAuthKey returns the run's key material. The main config process
// regenerates it fresh once per run; every other process (the Next server
// started with AUTH_SIGNING_PUBKEY, and each spec worker that signs cookies in
// e2e/mocked/_session.ts) reads the SAME persisted file, so signer + verifier
// always agree. Falls back to generating if the file is somehow missing.
export function getTestAuthKey(): TestAuthKeyMaterial {
  if (!isWorkerProcess) return generateTestAuthKey();
  return readTestAuthKey() ?? generateTestAuthKey();
}
