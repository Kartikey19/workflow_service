import { test } from "node:test";
import assert from "node:assert/strict";
import { loadModule } from "./load-module.mjs";

const { WorkflowDraftEditor, draftStorageKey } =
  await loadModule("workflow-draft");
const initial = "empty definition";
const key = draftStorageKey("company-a", "user-a");
const memory = () => {
  const values = new Map();
  return {
    getItem: (key) => values.get(key) ?? null,
    setItem: (key, value) => values.set(key, value),
    removeItem: (key) => values.delete(key),
  };
};

test("typing is one undo action; structural edits, redo branching and revert preserve definitions", () => {
  const editor = new WorkflowDraftEditor(memory(), key, initial);
  editor.begin("");
  editor.change("AI step");
  editor.change("prompt: h", "prompt", 1000);
  editor.change("prompt: hello", "prompt", 1100);
  editor.move(-1);
  assert.equal(editor.getSnapshot().source, "AI step");
  editor.move(1);
  assert.equal(editor.getSnapshot().source, "prompt: hello");
  editor.change("deleted step");
  editor.move(-1);
  assert.equal(editor.getSnapshot().source, "prompt: hello");
  editor.change("different model");
  assert.equal(editor.getSnapshot().canRedo, false);
  editor.revert();
  assert.equal(editor.getSnapshot().dirty, false);
  editor.move(-1);
  assert.equal(editor.getSnapshot().source, "different model");
});

test("reload restores the active draft; workflow, user and organization boundaries stay separate", () => {
  const storage = memory();
  let editor = new WorkflowDraftEditor(storage, key, initial);
  editor.begin("first");
  editor.load("first", "published first", 1);
  editor.change("first draft");
  editor.begin("second");
  editor.load("second", "published second", 2);
  editor.change("second draft");
  editor = new WorkflowDraftEditor(storage, key, initial);
  assert.equal(editor.getSnapshot().slot, "second");
  assert.equal(editor.getSnapshot().source, "second draft");
  editor.load("first", "late response", 3);
  assert.equal(editor.getSnapshot().source, "second draft");
  editor.load("second", "newer second", 3);
  assert.equal(editor.getSnapshot().conflict, true);
  assert.equal(editor.getSnapshot().source, "second draft");
  editor.move(-1);
  assert.equal(editor.getSnapshot().source, "newer second");
  editor.begin("first");
  assert.equal(editor.getSnapshot().source, "first draft");
  editor.close();
  editor = new WorkflowDraftEditor(storage, key, initial);
  assert.equal(editor.getSnapshot().open, false);
  assert.equal(editor.getSnapshot().drafts.first.source, "first draft");
  for (const [org, user] of [
    ["company-b", "user-a"],
    ["company-a", "user-b"],
  ]) {
    const other = new WorkflowDraftEditor(
      storage,
      draftStorageKey(org, user),
      initial,
    );
    assert.equal(other.getSnapshot().open, false);
    assert.deepEqual(other.getSnapshot().drafts, {});
  }
});

test("publishing clears only the submitted draft and preserves in-flight edits or a different selection", () => {
  const storage = memory();
  const editor = new WorkflowDraftEditor(storage, key, initial);
  editor.begin("");
  editor.change("submitted");
  const session = editor.getSnapshot().session;
  editor.change("edited during publish");
  editor.published(session, "", "named", "submitted", 1);
  assert.equal(editor.getSnapshot().slot, "named");
  assert.equal(editor.getSnapshot().source, "edited during publish");
  editor.load("named", "stale server response", 0);
  assert.equal(editor.getSnapshot().source, "edited during publish");
  assert.equal(editor.getSnapshot().baseVersion, 1);
  editor.move(-1);
  assert.equal(editor.getSnapshot().source, "submitted");
  assert.equal(editor.getSnapshot().dirty, false);
  assert.equal(storage.getItem(key), null);
  editor.change("second submission");
  const second = editor.getSnapshot().session;
  editor.begin("other");
  editor.change("other draft");
  editor.published(second, "named", "named", "second submission", 2);
  assert.equal(editor.getSnapshot().slot, "other");
  assert.equal(editor.getSnapshot().source, "other draft");
  assert.equal(editor.getSnapshot().drafts.named, undefined);
});

test("unavailable storage reports failure while undo still works; invalid YAML is recoverable", () => {
  const blocked = {
    getItem() {
      throw new Error("blocked");
    },
    setItem() {
      throw new Error("full");
    },
    removeItem() {
      throw new Error("blocked");
    },
  };
  const editor = new WorkflowDraftEditor(blocked, key, initial);
  editor.begin("");
  editor.change("nodes: [invalid YAML");
  assert.equal(editor.getSnapshot().saveError, "storage");
  editor.move(-1);
  assert.equal(editor.getSnapshot().source, initial);
  const storage = memory();
  const saved = new WorkflowDraftEditor(storage, key, initial);
  saved.begin("");
  saved.change("nodes: [invalid YAML");
  assert.equal(
    new WorkflowDraftEditor(storage, key, initial).getSnapshot().source,
    "nodes: [invalid YAML",
  );
  saved.change("assistant draft");
  assert.equal(saved.getSnapshot().source, "assistant draft");
});

test("another tab's drafts are never silently overwritten", () => {
  const storage = memory();
  const first = new WorkflowDraftEditor(storage, key, initial);
  first.begin("");
  first.change("original draft");
  const second = new WorkflowDraftEditor(storage, key, initial);
  second.change("saved by second tab");
  first.change("first tab's unsaved edit");
  assert.equal(first.getSnapshot().saveError, "changed");
  assert.equal(first.getSnapshot().source, "first tab's unsaved edit");
  assert.equal(
    new WorkflowDraftEditor(storage, key, initial).getSnapshot().source,
    "saved by second tab",
  );
});
