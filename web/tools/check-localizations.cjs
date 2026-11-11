// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT
// TypeScript 5 is the existing generator workspace's dev-only dependency.
// TypeScript 7's CLI does not expose the compiler API.
const fs = require("node:fs");
const path = require("node:path");
const { createRequire } = require("node:module");
const ts = createRequire(path.join(__dirname, "openapi-typescript/package.json"))("typescript");
const root = path.resolve(__dirname, "..");
const catalogs = ["es", "pt"].map((language) => [language, JSON.parse(fs.readFileSync(path.join(root, `src/app/lib/i18n/${language}.json`), "utf8"))]);
const failures = [];
const placeholders = (s) => [...s.matchAll(/\{[\w]+\}/g)].map((m) => m[0]).sort().join("|");
const keys = new Set(Object.keys(catalogs[0][1]));
for (const [language, catalog] of catalogs) {
  for (const source of new Set([...keys, ...Object.keys(catalog)])) {
    const value = catalog[source];
    if (typeof value !== "string" || !value.trim()) failures.push(`${language}: missing ${JSON.stringify(source)}`);
    else if (placeholders(source) !== placeholders(value)) failures.push(`${language}: changed placeholders in ${JSON.stringify(source)}`);
    if (!keys.has(source)) failures.push(`${language}: unexpected key ${JSON.stringify(source)}`);
  }
}
for (const file of ["backend-messages.json", "server-display-messages.json"]) {
  const errors = JSON.parse(fs.readFileSync(path.join(root, "src/app/lib/i18n", file), "utf8"));
  for (const source of [...errors.exact, ...errors.formatted.map((entry) => entry.message)]) {
    for (const [language, catalog] of catalogs) if (!Object.hasOwn(catalog, source)) failures.push(`${language}: missing server message ${JSON.stringify(source)}`);
  }
}
const files = [];
function walk(dir) {
  for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
    const file = path.join(dir, entry.name);
    if (entry.isDirectory()) {
      if (!["generated", "highlight", "fonts", "test"].includes(entry.name)) walk(file);
    } else if (/\.tsx?$/.test(file) && !/\.(?:test|spec|d)\.tsx?$/.test(file)) files.push(file);
  }
}
walk(path.join(root, "src"));
const program = ts.createProgram(files, { jsx: ts.JsxEmit.ReactJSX, noResolve: true, noLib: true });
const checker = program.getTypeChecker();
const messages = new Set();
function literals(node, seen = new Set()) {
  if (!node || seen.has(node)) return [];
  const visited = new Set(seen).add(node);
  if (ts.isStringLiteralLike(node)) return [node.text];
  if (ts.isParenthesizedExpression(node) || ts.isAsExpression(node) || ts.isSatisfiesExpression(node)) return literals(node.expression, visited);
  if (ts.isConditionalExpression(node)) return [...literals(node.whenTrue, visited), ...literals(node.whenFalse, visited)];
  if (ts.isArrayLiteralExpression(node)) return node.elements.flatMap((el) => literals(el, visited));
  if (ts.isElementAccessExpression(node)) return literals(node.expression, visited);
  if (ts.isIdentifier(node)) {
    const declarations = checker.getSymbolAtLocation(node)?.declarations ?? [];
    return declarations.flatMap((declaration) => ts.isVariableDeclaration(declaration) ? literals(declaration.initializer, visited) : []);
  }
  // API-owned labels use bundle catalogs. Dynamic values are not guessed or
  // extracted from user records; the bundle loader validates those catalogs.
  return [];
}
for (const file of files) {
  const sourceFile = program.getSourceFile(file);
  function visit(node) {
    if (ts.isCallExpression(node) && /^(?:t|tr|rich)$/.test(node.expression.getText(sourceFile))) {
      for (const source of literals(node.arguments[0])) {
        if (!source.trim()) continue;
        messages.add(source);
        for (const [language, catalog] of catalogs) if (!Object.hasOwn(catalog, source)) {
          const line = sourceFile.getLineAndCharacterOfPosition(node.getStart()).line + 1;
          failures.push(`${path.relative(root, file)}:${line}: ${language} missing ${JSON.stringify(source)}`);
        }
      }
    }
    ts.forEachChild(node, visit);
  }
  visit(sourceFile);
}
if (failures.length) {
  process.stderr.write([...new Set(failures)].join("\n") + "\n");
  process.exitCode = 1;
} else {
  process.stdout.write(`Localization: ${messages.size} source messages and ${keys.size} catalog entries checked for es/pt.\n`);
}
