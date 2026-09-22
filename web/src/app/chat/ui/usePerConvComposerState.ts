import { useRef, useState } from "react";
import type { PendingAttachment } from "./ChatChips";

// usePerConvComposerState groups the four per-conversation composer state
// pieces that used to live inline in ChatExperience — promptByConv,
// pendingAttachmentsByConv, attachmentErrorByConv, and the uploadingConvs
// set. They were extracted together (issue #401) because they share one
// shape — a per-conv slot keyed by `currentConvKey` (a real conversation
// id or the PENDING sentinel for the empty new-chat view) — and one set of
// invariants: each chat keeps its own draft + queued uploads + error, an
// async submit clears the slot it was launched from even after the user
// navigates away, and a brand-new chat's per-submission key is promoted to
// its real conversation id in one atomic step when the server confirms it.
//
// The derived reads and the closure-captured setters take `currentConvKey`
// as a hook argument, which means a setter created during one render writes
// to the key that was current *at that render*. Keeping the capture
// render-scoped (not a ref) is load-bearing: it's what lets an in-flight
// submit in conv A keep clearing A's slot after the user moves to conv B.
//
// The three per-conv slots are Maps, not plain objects, and that is a
// security property rather than a style choice. The key is a conversation
// id that arrives from the server, so writing it as a property name on an
// object literal makes the property name remote-controlled — CodeQL's
// js/remote-property-injection (security-severity 7.5) flagged all six
// writes, and a key of `__proto__` or `constructor` is the reason the rule
// exists. A Map has no prototype chain to walk into: `set("__proto__", v)`
// stores an ordinary entry, so the sink is gone rather than argued about.
export type PerConvComposerState = {
  // Raw per-conv records. Exposed for callers that read a specific
  // (submit-time) key rather than the current one; prefer the derived reads
  // and getPendingAttachmentsForKey below for everything else.
  promptByConv: ReadonlyMap<string, string>;
  pendingAttachmentsByConv: ReadonlyMap<string, PendingAttachment[]>;
  attachmentErrorByConv: ReadonlyMap<string, string>;
  // Derived for the current conversation key.
  prompt: string;
  pendingAttachments: PendingAttachment[];
  attachmentError: string | null;
  isUploadingAttachments: boolean;
  // Dispatch-compatible setters bound to the current key (captured at render).
  setPrompt: React.Dispatch<React.SetStateAction<string>>;
  setPendingAttachments: React.Dispatch<React.SetStateAction<PendingAttachment[]>>;
  setAttachmentError: React.Dispatch<React.SetStateAction<string | null>>;
  // Explicit-key setters for callers that target a specific slot.
  setPromptForKey: (key: string, value: string) => void;
  setPendingAttachmentsForKey: (key: string, value: PendingAttachment[]) => void;
  setAttachmentErrorForKey: (key: string, value: string | null) => void;
  // Upload-in-flight marks for a specific conv slot.
  markConvUploading: (key: string) => void;
  markConvUploadDone: (key: string) => void;
  // Read a specific slot's queued attachments (used by submit, which knows
  // its own composer key rather than the currently-displayed one).
  getPendingAttachmentsForKey: (key: string) => PendingAttachment[];
  // Promote every composer slot from a pending key onto the real conv id in
  // one synchronous step. Mirrors promoteStreamKey in useTurnStreamState so
  // the conversation-event handler can re-key both families back-to-back
  // without an SSE event observing a half-renamed state.
  promoteComposerKey: (oldKey: string, newKey: string) => void;
};

// setSlot / clearSlot are the two Map writes every setter below funnels
// through. Both return `prev` unchanged when the write is a no-op so React
// skips the re-render, which is the same identity-stability contract the
// object-spread version had.
function setSlot<V>(prev: Map<string, V>, key: string, value: V): Map<string, V> {
  const next = new Map(prev);
  next.set(key, value);
  return next;
}

function clearSlot<V>(prev: Map<string, V>, key: string): Map<string, V> {
  if (!prev.has(key)) return prev;
  const next = new Map(prev);
  next.delete(key);
  return next;
}

export function usePerConvComposerState(currentConvKey: string): PerConvComposerState {
  // Per-conv composer state — promptByConv / pendingAttachmentsByConv /
  // attachmentErrorByConv / uploadingConvs. These used to be global,
  // which meant typing in chat A then switching to chat B leaked A's
  // draft + queued files into B's composer. They're keyed by
  // currentConvKey (real conv id or the PENDING sentinel for the empty
  // new-chat view) so each chat keeps its own draft + uploads + errors.
  // Setters are derived below and use closure-captured currentConvKey
  // so an async submit in conv A clears A's slot even if the user
  // navigated to B in the meantime.
  const [promptByConv, setPromptByConv] = useState<Map<string, string>>(
    () => new Map<string, string>(),
  );
  const [pendingAttachmentsByConv, setPendingAttachmentsByConv] = useState<
    Map<string, PendingAttachment[]>
  >(() => new Map<string, PendingAttachment[]>());
  const [attachmentErrorByConv, setAttachmentErrorByConv] = useState<
    Map<string, string>
  >(() => new Map<string, string>());
  // uploadingConvs is the set of conv keys with an in-flight attachment
  // upload. Used so the send button + attachment-removal chips disable
  // only for the conv whose upload is running, not for an unrelated
  // chat the user navigates to.
  const [uploadingConvs, setUploadingConvs] = useState<Set<string>>(
    () => new Set<string>(),
  );
  const uploadingConvsRef = useRef<Set<string>>(new Set<string>());
  const markConvUploading = (key: string) => {
    if (uploadingConvsRef.current.has(key)) return;
    uploadingConvsRef.current.add(key);
    setUploadingConvs(new Set(uploadingConvsRef.current));
  };
  const markConvUploadDone = (key: string) => {
    if (!uploadingConvsRef.current.has(key)) return;
    uploadingConvsRef.current.delete(key);
    setUploadingConvs(new Set(uploadingConvsRef.current));
  };

  // Composer derivations. Each setter mutates the per-conv Map under
  // the currentConvKey captured at *render* time, which means an async
  // submit closure keeps writing to the slot it was launched from even
  // if the user has since navigated to another chat.
  const EMPTY_PENDING_ATTACHMENTS: readonly PendingAttachment[] = [];
  const prompt = promptByConv.get(currentConvKey) ?? "";
  const pendingAttachments =
    pendingAttachmentsByConv.get(currentConvKey) ??
    (EMPTY_PENDING_ATTACHMENTS as PendingAttachment[]);
  const attachmentError = attachmentErrorByConv.get(currentConvKey) ?? null;
  const isUploadingAttachments = uploadingConvs.has(currentConvKey);
  const setPrompt: React.Dispatch<React.SetStateAction<string>> = (next) => {
    setPromptByConv((prev) => {
      const old = prev.get(currentConvKey) ?? "";
      const value =
        typeof next === "function"
          ? (next as (s: string) => string)(old)
          : next;
      if (value === old) return prev;
      return value === ""
        ? clearSlot(prev, currentConvKey)
        : setSlot(prev, currentConvKey, value);
    });
  };
  const setPromptForKey = (key: string, value: string) => {
    setPromptByConv((prev) => {
      const old = prev.get(key) ?? "";
      if (value === old) return prev;
      return value === "" ? clearSlot(prev, key) : setSlot(prev, key, value);
    });
  };
  const setPendingAttachments: React.Dispatch<
    React.SetStateAction<PendingAttachment[]>
  > = (next) => {
    setPendingAttachmentsByConv((prev) => {
      const old = prev.get(currentConvKey) ?? [];
      const value =
        typeof next === "function"
          ? (next as (a: PendingAttachment[]) => PendingAttachment[])(old)
          : next;
      if (value === old) return prev;
      return value.length === 0
        ? clearSlot(prev, currentConvKey)
        : setSlot(prev, currentConvKey, value);
    });
  };
  const setPendingAttachmentsForKey = (key: string, value: PendingAttachment[]) => {
    setPendingAttachmentsByConv((prev) =>
      value.length === 0 ? clearSlot(prev, key) : setSlot(prev, key, value),
    );
  };
  const setAttachmentError: React.Dispatch<
    React.SetStateAction<string | null>
  > = (next) => {
    setAttachmentErrorByConv((prev) => {
      const old = prev.get(currentConvKey) ?? null;
      const value =
        typeof next === "function"
          ? (next as (s: string | null) => string | null)(old)
          : next;
      if (value === old) return prev;
      return value === null
        ? clearSlot(prev, currentConvKey)
        : setSlot(prev, currentConvKey, value);
    });
  };
  const setAttachmentErrorForKey = (key: string, value: string | null) => {
    setAttachmentErrorByConv((prev) =>
      value === null ? clearSlot(prev, key) : setSlot(prev, key, value),
    );
  };

  const getPendingAttachmentsForKey = (key: string): PendingAttachment[] =>
    pendingAttachmentsByConv.get(key) ?? [];

  // promoteComposerKey migrates the per-submission pending key's composer
  // state onto the real conversation id once the "conversation" SSE event
  // lands. Composer state for the per-submission key (rare but possible if
  // the user typed something in the pending view) follows the slot to the
  // real id so a future submit on this conv finds its draft. Functional
  // setters are used so the read sees the latest committed value, not a
  // (potentially stale) closure capture.
  const promoteComposerKey = (oldKey: string, newKey: string) => {
    setPromptByConv((prev) => {
      const v = prev.get(oldKey);
      if (typeof v !== "string") return prev;
      const next = clearSlot(prev, oldKey);
      return v === "" ? next : setSlot(next, newKey, v);
    });
    setPendingAttachmentsByConv((prev) => {
      const v = prev.get(oldKey);
      if (!v || v.length === 0) return prev;
      return setSlot(clearSlot(prev, oldKey), newKey, v);
    });
    setAttachmentErrorByConv((prev) => {
      const v = prev.get(oldKey);
      if (typeof v !== "string") return prev;
      return setSlot(clearSlot(prev, oldKey), newKey, v);
    });
    if (uploadingConvsRef.current.has(oldKey)) {
      uploadingConvsRef.current.delete(oldKey);
      uploadingConvsRef.current.add(newKey);
      setUploadingConvs(new Set(uploadingConvsRef.current));
    }
  };

  return {
    promptByConv,
    pendingAttachmentsByConv,
    attachmentErrorByConv,
    prompt,
    pendingAttachments,
    attachmentError,
    isUploadingAttachments,
    setPrompt,
    setPendingAttachments,
    setAttachmentError,
    setPromptForKey,
    setPendingAttachmentsForKey,
    setAttachmentErrorForKey,
    markConvUploading,
    markConvUploadDone,
    getPendingAttachmentsForKey,
    promoteComposerKey,
  };
}
