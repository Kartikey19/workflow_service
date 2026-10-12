type SavedDraft = { source: string; baseVersion: number };
type Drafts = Record<string, SavedDraft>;
type DraftStorage = Pick<Storage, "getItem" | "setItem" | "removeItem">;

export type DraftSnapshot = {
  source: string;
  baseVersion: number;
  slot: string;
  open: boolean;
  session: number;
  revision: number;
  canUndo: boolean;
  canRedo: boolean;
  dirty: boolean;
  restored: boolean;
  conflict: boolean;
  saveError: "storage" | "changed" | null;
  drafts: Drafts;
};

export function draftStorageKey(organization: string, user: string) {
  return `weft:drafts:1:${JSON.stringify([organization, user])}`;
}

// One definition history serves the canvas and YAML editor. Only the latest
// unpublished definition is persisted. Account setup supplies references only.
export class WorkflowDraftEditor {
  private drafts: Drafts = {};
  private history: string[];
  private index = 0;
  private baseline: string;
  private baseVersion = 0;
  private loaded = true;
  private lastEdit = { group: "", time: 0 };
  private listeners = new Set<() => void>();
  private state: DraftSnapshot;
  private stored: string | null = null;

  constructor(
    private storage: DraftStorage | undefined,
    private key: string,
    private initial: string,
    newWorkflow = false,
  ) {
    let active: string | null = null;
    let saveError: DraftSnapshot["saveError"] = null;
    try {
      const raw = storage?.getItem(key);
      this.stored = raw ?? null;
      if (raw) {
        const saved = JSON.parse(raw);
        if (
          saved.version !== 1 ||
          !saved.drafts ||
          typeof saved.drafts !== "object"
        )
          throw new Error("Invalid draft storage");
        for (const [slot, value] of Object.entries(saved.drafts)) {
          const draft = value as SavedDraft;
          if (
            draft &&
            typeof draft.source === "string" &&
            Number.isSafeInteger(draft.baseVersion) &&
            draft.baseVersion >= 0
          )
            this.drafts = { ...this.drafts, [slot]: draft };
        }
        if (
          typeof saved.active === "string" &&
          Object.hasOwn(this.drafts, saved.active)
        )
          active = saved.active;
      }
    } catch {
      saveError = "storage";
    }
    if (newWorkflow) active = "";
    const draft = active !== null ? this.drafts[active] : undefined;
    this.baseline = initial;
    this.history = draft ? [initial, draft.source] : [initial];
    this.index = this.history.length - 1;
    this.baseVersion = draft?.baseVersion || 0;
    this.loaded = !active;
    this.state = {
      source: this.history[this.index],
      baseVersion: this.baseVersion,
      slot: active || "",
      open: active !== null,
      session: 0,
      revision: 0,
      canUndo: this.index > 0,
      canRedo: false,
      dirty: !!draft,
      restored: !!draft,
      conflict: false,
      saveError,
      drafts: this.drafts,
    };
  }

  getSnapshot = () => this.state;
  subscribe = (listener: () => void) => {
    this.listeners.add(listener);
    return () => {
      this.listeners.delete(listener);
    };
  };

  private emit(patch: Partial<DraftSnapshot> = {}) {
    this.state = {
      ...this.state,
      source: this.history[this.index],
      baseVersion: this.baseVersion,
      canUndo: this.index > 0,
      canRedo: this.index < this.history.length - 1,
      dirty: this.history[this.index] !== this.baseline,
      drafts: this.drafts,
      ...patch,
    };
    this.listeners.forEach((listener) => listener());
  }

  private save() {
    let saveError: DraftSnapshot["saveError"] = null;
    try {
      if (!this.storage) throw new Error("Storage unavailable");
      if (this.storage.getItem(this.key) !== this.stored) {
        this.emit({ saveError: "changed" });
        return;
      }
      const saved = Object.keys(this.drafts).length
        ? JSON.stringify({
            version: 1,
            active:
              this.state.open && Object.hasOwn(this.drafts, this.state.slot)
                ? this.state.slot
                : null,
            drafts: this.drafts,
          })
        : null;
      if (saved === null) this.storage.removeItem(this.key);
      else this.storage.setItem(this.key, saved);
      this.stored = saved;
    } catch {
      saveError = "storage";
    }
    this.emit({ saveError });
  }

  private saveSource() {
    const slot = this.state.slot;
    this.drafts = { ...this.drafts };
    if (this.history[this.index] === this.baseline) delete this.drafts[slot];
    else
      this.drafts = {
        ...this.drafts,
        [slot]: {
          source: this.history[this.index],
          baseVersion: this.drafts[slot]?.baseVersion ?? this.baseVersion,
        },
      };
    this.save();
  }

  begin(slot: string) {
    const draft = Object.hasOwn(this.drafts, slot)
      ? this.drafts[slot]
      : undefined;
    this.baseline = this.initial;
    this.baseVersion = draft?.baseVersion || 0;
    this.loaded = !slot;
    this.history = [this.initial];
    if (draft && draft.source !== this.initial) this.history.push(draft.source);
    this.index = this.history.length - 1;
    this.lastEdit.group = "";
    this.emit({
      slot,
      open: true,
      session: this.state.session + 1,
      revision: this.state.revision + 1,
      restored: !!draft,
      conflict: false,
    });
    this.saveSource();
  }

  // A late response from a previously selected workflow cannot replace this draft.
  load(slot: string, source: string, version: number) {
    if (!this.state.open || this.state.slot !== slot) return;
    const draft = Object.hasOwn(this.drafts, slot)
      ? this.drafts[slot]
      : undefined;
    if (this.loaded && this.baseVersion >= version) return;
    const current = this.history[this.index];
    const keep = !!draft || (this.loaded && current !== this.baseline);
    const conflict =
      keep && (draft?.baseVersion ?? this.baseVersion) !== version;
    this.baseline = source;
    this.baseVersion = version;
    this.loaded = true;
    this.history = keep && current !== source ? [source, current] : [source];
    this.index = this.history.length - 1;
    this.lastEdit.group = "";
    this.emit({ conflict, revision: this.state.revision + 1 });
    this.saveSource();
  }

  change(source: string, group = "", time = Date.now()) {
    if (source === this.history[this.index]) return;
    const coalesce =
      group &&
      group === this.lastEdit.group &&
      time - this.lastEdit.time < 750 &&
      this.index > 0 &&
      this.index === this.history.length - 1;
    this.history = this.history.slice(0, this.index + 1);
    if (coalesce) this.history[this.index] = source;
    else {
      this.history.push(source);
      this.index++;
    }
    if (this.history.length > 80) {
      this.history.shift();
      this.index--;
    }
    this.lastEdit = { group, time };
    this.emit({ restored: false });
    this.saveSource();
  }

  move(direction: -1 | 1) {
    const next = this.index + direction;
    if (next < 0 || next >= this.history.length) return;
    this.index = next;
    this.lastEdit.group = "";
    this.emit({ revision: this.state.revision + 1, restored: false });
    this.saveSource();
  }

  replace(source: string) {
    this.change(source);
    this.emit({ revision: this.state.revision + 1 });
  }

  revert() {
    this.change(this.baseline);
    this.emit({ conflict: false, revision: this.state.revision + 1 });
  }
  close() {
    this.emit({ open: false });
    this.save();
  }

  // Publishing a captured definition must not discard edits made during the request.
  published(
    session: number,
    slot: string,
    name: string,
    source: string,
    version: number,
    canonical = source,
  ) {
    this.drafts = { ...this.drafts };
    if (this.drafts[slot]?.source === source) delete this.drafts[slot];
    if (session === this.state.session && this.state.open) {
      const current = this.history[this.index];
      delete this.drafts[slot];
      this.baseline = canonical;
      this.baseVersion = version;
      this.loaded = true;
      this.history = current === source ? [canonical] : [canonical, current];
      this.index = this.history.length - 1;
      this.lastEdit.group = "";
      this.emit({ slot: name, conflict: false, restored: false });
      this.saveSource();
    } else this.save();
  }
}
