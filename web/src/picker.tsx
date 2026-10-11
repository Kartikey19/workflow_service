import {
  useEffect,
  useId,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from "react";
import { Plus, Search } from "lucide-react";

export type PickerItem = {
  id: string;
  group: string;
  title: string;
  hint?: string;
  tag?: string;
};

function matches(item: PickerItem, words: string[]) {
  const text =
    `${item.title} ${item.group} ${item.hint || ""} ${item.tag || ""}`.toLowerCase();
  return words.every((w) => text.includes(w));
}

// A searchable, grouped catalog opened from one button. It replaces long native
// selects: type to filter across names, groups and descriptions; arrow keys
// move, Enter picks, Escape closes. With no query the list is grouped; while
// searching it stays flat-ordered but keeps each result's group visible.
export function StepPicker({
  items,
  onPick,
  label = "Add step",
  placeholder = "Search steps, connectors and agents",
  empty = "Nothing to add yet.",
  icon = <Plus size={15} aria-hidden="true" />,
  plain = false,
}: {
  items: PickerItem[];
  onPick: (id: string) => void;
  label?: string;
  placeholder?: string;
  empty?: string;
  icon?: ReactNode;
  plain?: boolean;
}) {
  const [open, setOpen] = useState(false),
    [query, setQuery] = useState(""),
    [active, setActive] = useState(0);
  const root = useRef<HTMLDivElement>(null),
    input = useRef<HTMLInputElement>(null),
    trigger = useRef<HTMLButtonElement>(null);
  const id = useId();
  const words = query.toLowerCase().split(/\s+/).filter(Boolean);
  const shown = useMemo(
    () => items.filter((item) => matches(item, words)),
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [items, query],
  );
  const groups = useMemo(() => {
    const out: { name: string; items: PickerItem[] }[] = [];
    for (const item of shown) {
      let g = out.find((g) => g.name === item.group);
      if (!g) out.push((g = { name: item.group, items: [] }));
      g.items.push(item);
    }
    return out;
  }, [shown]);
  // Keyboard order follows what is drawn: group by group.
  const ordered = useMemo(() => groups.flatMap((g) => g.items), [groups]);
  const activeItem = ordered[Math.min(active, ordered.length - 1)];
  const domID = (item: PickerItem) =>
    `${id}-${ordered.findIndex((o) => o.id === item.id)}`;

  function close(refocus: boolean) {
    setOpen(false);
    setQuery("");
    setActive(0);
    if (refocus) trigger.current?.focus();
  }
  function pick(item: PickerItem | undefined) {
    if (!item) return;
    onPick(item.id);
    close(true);
  }
  useEffect(() => {
    if (!open) return;
    input.current?.focus();
    const away = (e: PointerEvent) => {
      if (!root.current?.contains(e.target as Node)) close(false);
    };
    document.addEventListener("pointerdown", away);
    return () => document.removeEventListener("pointerdown", away);
  }, [open]);
  useEffect(() => {
    if (!open || !activeItem) return;
    document
      .getElementById(`${id}-${ordered.indexOf(activeItem)}`)
      ?.scrollIntoView({ block: "nearest" });
  }, [open, activeItem, ordered, id]);

  return (
    <div className="picker" ref={root}>
      <button
        ref={trigger}
        type="button"
        className={plain ? "" : "primary"}
        aria-haspopup="listbox"
        aria-expanded={open}
        onClick={() => (open ? close(false) : setOpen(true))}
      >
        {icon}
        {label}
      </button>
      {open && (
        <div className="picker-pop">
          <div className="picker-search">
            <Search size={15} aria-hidden="true" />
            <input
              ref={input}
              role="combobox"
              aria-expanded="true"
              aria-controls={`${id}-list`}
              aria-activedescendant={activeItem ? domID(activeItem) : undefined}
              aria-label={placeholder}
              placeholder={placeholder}
              value={query}
              autoComplete="off"
              onChange={(e) => {
                setQuery(e.target.value);
                setActive(0);
              }}
              onKeyDown={(e) => {
                if (e.key === "ArrowDown") {
                  e.preventDefault();
                  setActive((a) => Math.min(a + 1, ordered.length - 1));
                } else if (e.key === "ArrowUp") {
                  e.preventDefault();
                  setActive((a) => Math.max(a - 1, 0));
                } else if (e.key === "Enter") {
                  e.preventDefault();
                  pick(activeItem);
                } else if (e.key === "Escape") {
                  e.preventDefault();
                  close(true);
                } else if (e.key === "Tab") {
                  close(false);
                }
              }}
            />
          </div>
          <div className="picker-list" role="listbox" id={`${id}-list`}>
            {groups.map((g) => (
              <div key={g.name} role="group" aria-label={g.name}>
                <div className="picker-group" aria-hidden="true">
                  {g.name}
                </div>
                {g.items.map((item) => (
                  <div
                    key={item.id}
                    id={domID(item)}
                    role="option"
                    aria-selected={item.id === activeItem?.id}
                    className="picker-option"
                    onPointerMove={() =>
                      setActive(ordered.findIndex((o) => o.id === item.id))
                    }
                    onClick={() => pick(item)}
                  >
                    <span>
                      <strong>{item.title}</strong>
                      {item.hint && <small>{item.hint}</small>}
                    </span>
                    {item.tag && <span className="picker-tag">{item.tag}</span>}
                  </div>
                ))}
              </div>
            ))}
            {!ordered.length && (
              <p className="picker-empty">
                {items.length ? `No steps match “${query}”.` : empty}
              </p>
            )}
          </div>
        </div>
      )}
    </div>
  );
}
