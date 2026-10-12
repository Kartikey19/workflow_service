import { useEffect, useMemo, useSyncExternalStore } from "react";
import { WorkflowDraftEditor, draftStorageKey } from "./workflow-draft";

export function useWorkflowDraft(
  organization: string,
  user: string,
  initial: string,
  editable: boolean,
  newWorkflow: boolean,
) {
  const editor = useMemo(() => {
    let storage: Storage | undefined;
    try {
      if (editable) storage = window.localStorage;
    } catch {
      /* Saving reports unavailable storage. */
    }
    return new WorkflowDraftEditor(
      storage,
      draftStorageKey(organization, user),
      initial,
      newWorkflow,
    );
  }, [organization, user, initial, editable, newWorkflow]);
  const state = useSyncExternalStore(editor.subscribe, editor.getSnapshot);
  useEffect(() => {
    if (!editable || !state.open) return;
    const keys = (event: KeyboardEvent) => {
      if (!(event.metaKey || event.ctrlKey) || event.altKey) return;
      const target = event.target as HTMLElement;
      // Keep native text-field undo and never handle shortcuts inside setup dialogs.
      if (
        target.closest(
          "input, textarea, select, [contenteditable], dialog, [role=dialog]",
        )
      )
        return;
      const key = event.key.toLowerCase();
      if (key === "z" || (key === "y" && event.ctrlKey)) {
        event.preventDefault();
        editor.move(event.shiftKey || key === "y" ? 1 : -1);
      }
    };
    const leave = (event: BeforeUnloadEvent) => {
      const current = editor.getSnapshot();
      if (current.dirty && current.saveError) {
        event.preventDefault();
        event.returnValue = "";
      }
    };
    window.addEventListener("keydown", keys);
    window.addEventListener("beforeunload", leave);
    return () => {
      window.removeEventListener("keydown", keys);
      window.removeEventListener("beforeunload", leave);
    };
  }, [editor, editable, state.open]);
  return { editor, state };
}
