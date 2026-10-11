import {
  useEffect,
  useId,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from "react";
import { ChevronLeft, ChevronRight, Search, X } from "lucide-react";

// One entry of the step catalog. A branch opens a list of its own (an app opens
// its actions); a leaf adds a step. `section` groups leaves inside a branch,
// such as an app's reads and writes.
export type CreatorEntry = {
  id: string;
  title: string;
  hint?: string;
  icon: ReactNode;
  section?: string;
  children?: CreatorEntry[];
  // Why a leaf cannot be added yet, with a way to fix it.
  blocked?: { reason: string; href: string; action: string };
};

function leaves(
  entries: CreatorEntry[],
  path: string[] = [],
): { entry: CreatorEntry; path: string[] }[] {
  return entries.flatMap((e) =>
    e.children ? leaves(e.children, [...path, e.title]) : [{ entry: e, path }],
  );
}

// The step catalog as n8n's node creator works: categories first, then an app,
// then its actions, each list with its own search. Typing at any level searches
// everything below it; Escape goes back a level, then closes.
export function NodeCreator({
  entries,
  onPick,
  onClose,
  title = "Add a step",
}: {
  entries: CreatorEntry[];
  onPick: (id: string) => void;
  onClose: () => void;
  title?: string;
}) {
  const [trail, setTrail] = useState<CreatorEntry[]>([]),
    [query, setQuery] = useState(""),
    [active, setActive] = useState(0);
  const input = useRef<HTMLInputElement>(null);
  const id = useId();
  const here = trail.length ? trail[trail.length - 1] : null;
  // Re-resolve the trail against fresh entries, so a reload keeps the place.
  const level = useMemo(() => {
    let list = entries;
    for (const step of trail)
      list = list.find((e) => e.id === step.id)?.children || [];
    return list;
  }, [entries, trail]);
  const words = query.toLowerCase().split(/\s+/).filter(Boolean);
  const shown = useMemo(() => {
    if (!words.length)
      return level.map((entry) => ({ entry, path: [] as string[] }));
    return leaves(level).filter(({ entry, path }) => {
      const text =
        `${entry.title} ${entry.hint || ""} ${path.join(" ")}`.toLowerCase();
      return words.every((w) => text.includes(w));
    });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [level, query]);
  const sections = useMemo(() => {
    const out: { name: string; rows: typeof shown }[] = [];
    for (const row of shown) {
      const name = words.length
        ? row.path.join(" › ")
        : row.entry.section || "";
      let s = out.find((s) => s.name === name);
      if (!s) out.push((s = { name, rows: [] }));
      s.rows.push(row);
    }
    return out;
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [shown]);
  const ordered = sections.flatMap((s) => s.rows);
  const current = ordered[Math.min(active, ordered.length - 1)];

  useEffect(() => {
    input.current?.focus();
  }, [here]);
  useEffect(() => {
    if (current)
      document
        .getElementById(`${id}-${ordered.indexOf(current)}`)
        ?.scrollIntoView({ block: "nearest" });
  }, [current, ordered, id]);

  function open(entry: CreatorEntry) {
    if (entry.children) {
      setTrail([...trail, entry]);
      setQuery("");
      setActive(0);
    } else if (!entry.blocked) {
      onPick(entry.id);
    }
  }
  function back() {
    if (query) setQuery("");
    else if (trail.length) setTrail(trail.slice(0, -1));
    else onClose();
    setActive(0);
  }

  return (
    <section
      className="creator"
      role="dialog"
      aria-label={here ? here.title : title}
      onKeyDown={(e) => {
        if (e.key === "Escape") {
          e.preventDefault();
          back();
        }
      }}
    >
      <header className="creator-head">
        {here ? (
          <button className="icon-button" aria-label="Back" onClick={back}>
            <ChevronLeft size={18} aria-hidden="true" />
          </button>
        ) : null}
        <span className="creator-title">
          {here?.icon}
          <h2>{here ? here.title : title}</h2>
        </span>
        <button className="icon-button" aria-label="Close" onClick={onClose}>
          <X size={18} aria-hidden="true" />
        </button>
      </header>
      <label className="creator-search">
        <Search size={15} aria-hidden="true" />
        <input
          ref={input}
          role="combobox"
          aria-expanded="true"
          aria-controls={`${id}-list`}
          aria-activedescendant={
            current ? `${id}-${ordered.indexOf(current)}` : undefined
          }
          aria-label={here ? `Search ${here.title}` : "Search steps and apps"}
          placeholder={here ? `Search ${here.title}` : "Search steps and apps"}
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
            } else if (e.key === "Enter" && current) {
              e.preventDefault();
              open(current.entry);
            } else if (
              e.key === "ArrowRight" &&
              current?.entry.children &&
              !query
            ) {
              e.preventDefault();
              open(current.entry);
            } else if (e.key === "ArrowLeft" && !query && trail.length) {
              e.preventDefault();
              back();
            }
          }}
        />
      </label>
      <div className="creator-list" role="listbox" id={`${id}-list`}>
        {sections.map((s) => (
          <div key={s.name} role="group" aria-label={s.name || undefined}>
            {s.name && (
              <div className="creator-section" aria-hidden="true">
                {s.name}
              </div>
            )}
            {s.rows.map((row) => {
              const e = row.entry,
                index = ordered.indexOf(row);
              return (
                <div
                  key={`${row.path.join("/")}/${e.id}`}
                  id={`${id}-${index}`}
                  role="option"
                  aria-selected={row === current}
                  aria-disabled={!!e.blocked || undefined}
                  className="creator-row"
                  onPointerMove={() => setActive(index)}
                  onClick={() => open(e)}
                >
                  <span className="creator-icon">{e.icon}</span>
                  <span className="creator-text">
                    <strong>{e.title}</strong>
                    {e.blocked ? (
                      <small>
                        {e.blocked.reason}{" "}
                        <a
                          href={e.blocked.href}
                          target="_blank"
                          rel="noreferrer"
                          onClick={(event) => event.stopPropagation()}
                        >
                          {e.blocked.action}
                        </a>
                      </small>
                    ) : (
                      e.hint && <small>{e.hint}</small>
                    )}
                  </span>
                  {e.children && (
                    <ChevronRight
                      size={16}
                      className="creator-more"
                      aria-hidden="true"
                    />
                  )}
                </div>
              );
            })}
          </div>
        ))}
        {!ordered.length && (
          <p className="creator-empty">
            {query ? `Nothing matches “${query}”.` : "Nothing here yet."}
          </p>
        )}
      </div>
    </section>
  );
}
