// patrolBarPrefs — whether this browser has hidden the Patrol bar for one
// conversation.
//
// Hiding the bar is a per-viewer view choice, never a change to the patrol:
// the schedule keeps running and stays one click away under the composer's
// clock. So the choice lives in this browser's storage, keyed by
// conversation, through the guarded storage helpers (a blocked or full
// storage just means the choice lasts for this page). Showing the bar again
// removes the key, so storage only ever holds the conversations someone hid.

import { readPref, removePref, writePref } from "@/app/shared/lib/storage";

const PREFIX = "fleet.patrolBarHidden.";

export function readPatrolBarHidden(conversationId: string): boolean {
  return readPref(PREFIX + conversationId) === "1";
}

export function writePatrolBarHidden(conversationId: string, hidden: boolean): void {
  if (hidden) writePref(PREFIX + conversationId, "1");
  else removePref(PREFIX + conversationId);
}
