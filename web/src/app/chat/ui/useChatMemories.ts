// useChatMemories — chat's memory manager state and actions, moved out of
// chat-experience.tsx unchanged (#2008). The requests live in
// shared/lib/memoriesApi.ts (shared with Settings → Memories); the dialog is
// MemoryManagerDialog.tsx.
//
// loadMemories is also a turn-stream dependency (turnStreamDeps): a turn that
// saved a memory refreshes the list, so the hook returns it as before — a
// fresh function each render, reading the current loading flag.

import { useState } from "react";
import { errorMessage } from "@/app/shared/lib/errorMessage";
import {
  deleteMemory as deleteMemoryRequest,
  listMemories,
  patchMemory as patchMemoryRequest,
  promoteMemory,
  saveMemory as saveMemoryRequest,
  type UserMemory,
} from "@/app/shared/lib/memoriesApi";

export type MemoryView = "list" | "team";

export function useChatMemories() {
  const [memories, setMemories] = useState<UserMemory[]>([]);
  const [memoryManagerOpen, setMemoryManagerOpen] = useState(false);
  // Memory-manager tab: the flat record list (default) or the project's
  // Team learnings, shown only while the open chat is in a project (Item D4) —
  // members manage them without leaving the chat.
  const [memoryView, setMemoryView] = useState<MemoryView>("list");
  // Promotion of a personal memory into a project's team learnings (Item D5).
  // Non-null = the picker is open for this memory id (only when more than one
  // team-shared project is a candidate; with exactly one we just move it).
  const [promoteMemoryId, setPromoteMemoryId] = useState<string | null>(null);
  const [memoryDraft, setMemoryDraft] = useState("");
  const [memoryKindDraft, setMemoryKindDraft] = useState<string>("fact");
  const [editingMemoryId, setEditingMemoryId] = useState<string | null>(null);
  const [memoryError, setMemoryError] = useState<string | null>(null);
  const [isLoadingMemories, setIsLoadingMemories] = useState(false);
  const [isSavingMemory, setIsSavingMemory] = useState(false);

  const loadMemories = async () => {
    if (isLoadingMemories) return;
    setIsLoadingMemories(true);
    setMemoryError(null);
    try {
      setMemories(await listMemories("Unable to load memories"));
    } catch (err) {
      setMemoryError(errorMessage(err, "Unable to load memories."));
    } finally {
      setIsLoadingMemories(false);
    }
  };

  const openMemoryManager = () => {
    setMemoryManagerOpen(true);
    setMemoryView("list");
    setMemoryDraft("");
    setMemoryKindDraft("fact");
    setEditingMemoryId(null);
    void loadMemories();
  };

  const saveMemory = async () => {
    const content = memoryDraft.trim();
    if (!content || isSavingMemory) return;
    setIsSavingMemory(true);
    setMemoryError(null);
    try {
      await saveMemoryRequest(
        { id: editingMemoryId, content, kind: memoryKindDraft },
        "Unable to save memory",
      );
      setMemoryDraft("");
      setMemoryKindDraft("fact");
      setEditingMemoryId(null);
      await loadMemories();
    } catch (err) {
      setMemoryError(errorMessage(err, "Unable to save memory."));
    } finally {
      setIsSavingMemory(false);
    }
  };

  // promoteMemoryToProject MOVES one personal memory into a project's team
  // learnings (Item D5; see memoriesApi.promoteMemory).
  const promoteMemoryToProject = async (id: string, projectID: string) => {
    setMemoryError(null);
    try {
      await promoteMemory(id, projectID, "Unable to move that memory to team learnings");
      setPromoteMemoryId(null);
      await loadMemories();
    } catch (err) {
      setMemoryError(errorMessage(err, "Unable to move that memory to team learnings."));
    }
  };

  const deleteMemory = async (id: string) => {
    setMemoryError(null);
    try {
      await deleteMemoryRequest(id, "Unable to delete memory");
      setMemories((prev) => prev.filter((m) => m.id !== id));
      if (editingMemoryId === id) {
        setEditingMemoryId(null);
        setMemoryDraft("");
      }
    } catch (err) {
      setMemoryError(errorMessage(err, "Unable to delete memory."));
    }
  };

  // Partial update for the #515 controls (pin/unpin, retire/restore).
  const patchMemory = async (id: string, patch: { pinned?: boolean; retired?: boolean }) => {
    setMemoryError(null);
    try {
      await patchMemoryRequest(id, patch, "Unable to update memory");
      await loadMemories();
    } catch (err) {
      setMemoryError(errorMessage(err, "Unable to update memory."));
    }
  };

  // Puts a memory in the draft for editing.
  const editMemory = (memory: UserMemory) => {
    setEditingMemoryId(memory.id);
    setMemoryDraft(memory.content);
    setMemoryKindDraft(memory.kind ?? "fact");
  };

  const cancelEdit = () => {
    setEditingMemoryId(null);
    setMemoryDraft("");
  };

  return {
    memories,
    memoryManagerOpen,
    setMemoryManagerOpen,
    memoryView,
    setMemoryView,
    promoteMemoryId,
    setPromoteMemoryId,
    memoryDraft,
    setMemoryDraft,
    memoryKindDraft,
    setMemoryKindDraft,
    editingMemoryId,
    memoryError,
    isLoadingMemories,
    isSavingMemory,
    loadMemories,
    openMemoryManager,
    saveMemory,
    promoteMemoryToProject,
    deleteMemory,
    patchMemory,
    editMemory,
    cancelEdit,
  };
}

export type ChatMemories = ReturnType<typeof useChatMemories>;
