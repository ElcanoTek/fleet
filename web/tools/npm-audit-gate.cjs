#!/usr/bin/env node
// npm-audit-gate — the web tier's dependency-CVE gate (ADR-0113,
// docs/SCANNING.md). It runs `npm audit` against package-lock.json and fails on
// ANY advisory of any severity, exactly like `npm audit --audit-level=low`,
// except one listed in the reviewed register npm-audit-accepted.json. An entry
// there is accepted only while ALL of these hold, each checked here:
//
//   - it names the advisory (GHSA id) AND the package it is filed against;
//   - it carries a written reason;
//   - it has not expired (an expired entry fails, so it gets re-reviewed);
//   - every copy of that package in the lockfile is a dev dependency, so the
//     code never ships in the browser bundle or the binary;
//   - the advisory is still reported (a stale entry fails, so the register
//     never quietly outlives its need).
//
// Fixing the dependency is always preferred: the register is for an advisory
// with no patched release on a build-time-only package we cannot drop.
//
// Usage (from web/): node tools/npm-audit-gate.cjs

"use strict";

const { execFileSync } = require("node:child_process");
const fs = require("node:fs");
const path = require("node:path");

const WEB = path.resolve(__dirname, "..");
const REGISTER = path.join(WEB, "npm-audit-accepted.json");

function audit() {
  let out;
  try {
    out = execFileSync("npm", ["audit", "--json", "--package-lock-only"], {
      cwd: WEB,
      encoding: "utf8",
      stdio: ["ignore", "pipe", "inherit"],
      maxBuffer: 64 << 20,
    });
  } catch (err) {
    // npm audit exits 1 when it finds anything; its JSON is still on stdout.
    out = err.stdout;
  }
  if (!out) throw new Error("npm audit produced no output");
  const data = JSON.parse(out);
  if (data.error) throw new Error(`npm audit failed: ${JSON.stringify(data.error)}`);
  return data;
}

/** The advisories npm reports, one per (GHSA, package). */
function advisories(report) {
  const found = new Map();
  for (const vuln of Object.values(report.vulnerabilities ?? {})) {
    for (const via of vuln.via ?? []) {
      // A string `via` is a package that is only vulnerable because it depends
      // on another; the advisory itself is reported on that other package.
      if (typeof via !== "object" || !via) continue;
      const ghsa = /GHSA-[a-z0-9-]+/i.exec(via.url ?? "")?.[0] ?? `npm-${via.source}`;
      const key = `${ghsa}|${via.name}`;
      if (!found.has(key)) {
        found.set(key, { ghsa, pkg: via.name, severity: via.severity, title: via.title, url: via.url, range: via.range });
      }
    }
  }
  return [...found.values()];
}

/** Lockfile entries for a package name, and whether each is dev-only. */
function lockCopies(name) {
  const lock = JSON.parse(fs.readFileSync(path.join(WEB, "package-lock.json"), "utf8"));
  return Object.entries(lock.packages ?? {})
    .filter(([p]) => p === `node_modules/${name}` || p.endsWith(`/node_modules/${name}`))
    .map(([p, meta]) => ({ path: p, dev: meta.dev === true }));
}

function main() {
  const register = JSON.parse(fs.readFileSync(REGISTER, "utf8"));
  const accepted = register.accepted ?? [];
  const today = new Date().toISOString().slice(0, 10);
  const problems = [];

  for (const e of accepted) {
    if (!/^GHSA-[a-z0-9-]+$/i.test(e.advisory ?? "")) problems.push(`register entry has no GHSA id: ${JSON.stringify(e)}`);
    if (!e.package) problems.push(`register entry ${e.advisory} names no package`);
    if (!e.reason || e.reason.trim().length < 40) problems.push(`register entry ${e.advisory} has no written reason`);
    if (!/^\d{4}-\d{2}-\d{2}$/.test(e.expires ?? "")) problems.push(`register entry ${e.advisory} has no expiry (YYYY-MM-DD)`);
    else if (e.expires < today) problems.push(`register entry ${e.advisory} (${e.package}) expired on ${e.expires}: fix the dependency, or re-review and extend it`);
  }

  const found = advisories(audit());
  const used = new Set();
  const blocking = [];
  const waived = [];
  for (const a of found) {
    const entry = accepted.find((e) => e.advisory?.toLowerCase() === a.ghsa.toLowerCase() && e.package === a.pkg);
    if (!entry) {
      blocking.push(a);
      continue;
    }
    used.add(entry);
    const copies = lockCopies(a.pkg);
    const shipped = copies.filter((c) => !c.dev);
    if (copies.length === 0 || shipped.length > 0) {
      problems.push(
        `${a.ghsa} (${a.pkg}) is in the register but ${a.pkg} is not dev-only: ${shipped.map((c) => c.path).join(", ") || "not in the lockfile"}`,
      );
      continue;
    }
    waived.push({ ...a, expires: entry.expires });
  }
  for (const e of accepted) {
    if (!used.has(e)) problems.push(`register entry ${e.advisory} (${e.package}) no longer matches any advisory: remove it`);
  }

  for (const a of waived) {
    console.log(`accepted (dev-only, until ${a.expires}): ${a.ghsa} ${a.pkg} ${a.range ?? ""} [${a.severity}] ${a.title}`);
  }
  for (const a of blocking) {
    console.error(`BLOCKING: ${a.ghsa} ${a.pkg} ${a.range ?? ""} [${a.severity}] ${a.title} ${a.url ?? ""}`);
  }
  for (const p of problems) console.error(`BLOCKING: ${p}`);

  if (blocking.length || problems.length) {
    console.error(
      `\nnpm audit gate: ${blocking.length} advisory(ies) not accepted, ${problems.length} register problem(s). ` +
        "Fix the dependency (npm update / overrides); see docs/SCANNING.md before touching npm-audit-accepted.json.",
    );
    process.exit(1);
  }
  console.log(`npm audit gate: ok (${found.length} advisory(ies) reported, ${waived.length} accepted by the register).`);
}

main();
