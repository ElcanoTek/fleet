import { describe, expect, it } from "vitest";
import {
  afterDeleteAllUnpinned,
  countDeletedByDeleteAllUnpinned,
  deletedByDeleteAllUnpinned,
  survivesDeleteAllUnpinned,
} from "./deleteAllUnpinnedScope";

// Finding #20: "Delete all unpinned" removed every PROJECT chat from the rail
// on confirm. The server had kept them (store.DeleteAllUnpinned exempts
// project_id IS NOT NULL) and a reload brought them back, but for as long as
// the page stayed open the deletion looked like it had taken the projects with
// it — the one moment a user would panic.
//
// The client predicate here is the server's WHERE clause:
//   pinned = FALSE AND archived_at IS NULL AND project_id IS NULL
// (archived rows live in a separate list this action never touches).

const pinnedUnfiled = { id: "pinned", pinned: true };
const pinnedFiled = { id: "pinned-in-project", pinned: true, project_id: "p2" };
const unpinnedFiled = { id: "filed", pinned: false, project_id: "p2" };
const unpinnedUnfiled = { id: "temporary", pinned: false };

describe("deleteAllUnpinnedScope", () => {
  it("deletes only the unpinned, unfiled chat", () => {
    expect(deletedByDeleteAllUnpinned(unpinnedUnfiled)).toBe(true);
    expect(deletedByDeleteAllUnpinned(unpinnedFiled)).toBe(false);
    expect(deletedByDeleteAllUnpinned(pinnedUnfiled)).toBe(false);
    expect(deletedByDeleteAllUnpinned(pinnedFiled)).toBe(false);
  });

  it("survives is the exact complement of deleted", () => {
    for (const c of [pinnedUnfiled, pinnedFiled, unpinnedFiled, unpinnedUnfiled]) {
      expect(survivesDeleteAllUnpinned(c)).toBe(!deletedByDeleteAllUnpinned(c));
    }
  });

  it("keeps the pinned chat AND the unpinned project chat in the rail, and drops only the unfiled one", () => {
    const remaining = afterDeleteAllUnpinned([
      pinnedUnfiled,
      unpinnedFiled,
      unpinnedUnfiled,
      pinnedFiled,
    ]);
    expect(remaining.map((c) => c.id)).toEqual([
      "pinned",
      "filed",
      "pinned-in-project",
    ]);
  });

  it("treats an empty-string project id as unfiled, the way the server treats NULL", () => {
    // The rail writes `project_id: projectID || undefined` on an unfile, but a
    // server payload can carry "" — both mean "not in a project".
    expect(deletedByDeleteAllUnpinned({ pinned: false, project_id: "" })).toBe(
      true,
    );
  });

  it("counts for the confirm dialog exactly what it deletes", () => {
    const list = [pinnedUnfiled, unpinnedFiled, unpinnedUnfiled, pinnedFiled];
    expect(countDeletedByDeleteAllUnpinned(list)).toBe(1);
    // The count the dialog quotes and the rows the rail drops come from the
    // same predicate, so the promise and the effect cannot drift apart.
    expect(countDeletedByDeleteAllUnpinned(list)).toBe(
      list.length - afterDeleteAllUnpinned(list).length,
    );
  });

  it("is a no-op when everything is pinned or filed", () => {
    const list = [pinnedUnfiled, unpinnedFiled];
    expect(afterDeleteAllUnpinned(list)).toEqual(list);
    expect(countDeletedByDeleteAllUnpinned(list)).toBe(0);
  });
});
