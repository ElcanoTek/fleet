import { describe, expect, it } from "vitest";
import { buildIdFor } from "../../../vite.config";

// The client's baked build id must equal what the Go gateway stamps as
// X-App-Version: the real id for a production build (build-id.txt), and
// "dev" under the dev server, where the gateway stamps "dev" — otherwise the
// update banner would never clear during `make dev`.
describe("buildIdFor", () => {
  it("is dev under the dev server", () => {
    expect(buildIdFor("serve")).toBe("dev");
  });
  it("is the real id for a build", () => {
    expect(buildIdFor("build")).not.toBe("dev");
  });
});
