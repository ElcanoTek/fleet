// Vitest setup — loaded before every test file.
// Pulls in the jest-dom matchers (toBeInTheDocument, toHaveTextContent, etc).
import "@testing-library/jest-dom/vitest";

import { configure } from "@testing-library/react";
// The account record (shared/lib/account) is cached module-wide; a record one
// case's mocked fetch returned must not answer the next case's read.
import { afterEach } from "vitest";
import { _resetMeForTests } from "@/app/shared/lib/account";

// Radix UI (@floating-ui/dom, ScrollArea, Select, Switch, Dialog) and cmdk
// rely on browser DOM APIs that jsdom omits by default. Install safe fallbacks
// only when absent so individual tests that spy on or save/restore them keep
// working unchanged.
if (typeof globalThis.ResizeObserver === "undefined") {
  globalThis.ResizeObserver = class ResizeObserver {
    observe() {}
    unobserve() {}
    disconnect() {}
  };
}
if (typeof Element !== "undefined") {
  Element.prototype.scrollIntoView ??= () => {};
}
if (typeof HTMLElement !== "undefined") {
  HTMLElement.prototype.hasPointerCapture ??= () => false;
  HTMLElement.prototype.setPointerCapture ??= () => {};
  HTMLElement.prototype.releasePointerCapture ??= () => {};
}

// findBy*/waitFor give up after 1 s by default. The full suite runs ~200 jsdom
// files in parallel, and on a loaded CI runner a multi-request flow (the patrol
// launchpad's settle-then-roll-back close, for one) can take just over that,
// failing a correct test. A longer ceiling only costs time when a test is
// already failing; a passing one still resolves as soon as its condition holds.
configure({ asyncUtilTimeout: 5000 });

afterEach(() => {
  _resetMeForTests();
});
