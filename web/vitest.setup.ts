// Vitest setup — loaded before every test file.
// Pulls in the jest-dom matchers (toBeInTheDocument, toHaveTextContent, etc).
import "@testing-library/jest-dom/vitest";

// Node >= 25 defines `localStorage` / `sessionStorage` accessors on globalThis
// (its Web Storage implementation) whose getters return undefined unless node
// runs with --localstorage-file. vitest's jsdom environment leaves globals
// that already exist alone — and `window` IS globalThis inside it — so on such
// a Node every test that touches storage sees `undefined.setItem`. CI pins
// Node 24 (web/.nvmrc) and never hits this; a developer on a newer Node hit 22
// spurious failures across five files. Install a plain in-memory Storage with
// the DOM semantics the tests rely on (string keys and values, null for a
// missing key). A no-op where storage already works.
class MemoryStorage implements Storage {
  [name: string]: unknown;
  private readonly entries = new Map<string, string>();
  get length(): number {
    return this.entries.size;
  }
  clear(): void {
    this.entries.clear();
  }
  getItem(key: string): string | null {
    return this.entries.get(String(key)) ?? null;
  }
  key(index: number): string | null {
    return [...this.entries.keys()][index] ?? null;
  }
  removeItem(key: string): void {
    this.entries.delete(String(key));
  }
  setItem(key: string, value: string): void {
    this.entries.set(String(key), String(value));
  }
}

for (const key of ["localStorage", "sessionStorage"] as const) {
  const current = (globalThis as unknown as Record<string, Storage | undefined>)[key];
  if (typeof current?.setItem !== "function") {
    Object.defineProperty(globalThis, key, {
      value: new MemoryStorage(),
      configurable: true,
      writable: true,
    });
  }
}
