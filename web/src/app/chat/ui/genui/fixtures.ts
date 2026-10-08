// Loads the Go-side shared fixtures (internal/genui/testdata) so the browser
// halves of the pinned pairs run the same cases as the validator. Test-only.
import { readFileSync } from "node:fs";
import path from "node:path";

const TESTDATA = path.resolve(__dirname, "../../../../../../internal/genui/testdata");

export function loadFixture<T>(name: string): T {
  return JSON.parse(readFileSync(path.join(TESTDATA, name), "utf8")) as T;
}
