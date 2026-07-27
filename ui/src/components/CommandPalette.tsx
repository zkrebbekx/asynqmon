// Command palette (build contract §4.1): ⌘K/Ctrl+K opens; one input, five
// result kinds —
//   queue  fuzzy jump over the fleet queues cache (subsequence match, mono)
//   view   saved-view launcher (system + user views, server-side §4.2)
//   verb   bulk verb on the CURRENT console scope (tasks console only) →
//          hands off to the §4.3 BulkJobModal
//   task   paste-a-task-ID → looked up across all seven states via the
//          search API's id= predicate, opens the peek drawer
//   go to  every nav entry
// Overlay is focus-trapped (the input is the only tab stop), aria-listbox
// keyboard model (↑↓ navigate, Enter selects, Esc closes), styled with the
// --fc-* tokens to match the approved mockup in both themes.

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useLocation, useNavigate } from "react-router-dom";
import { Search, CornerDownLeft } from "lucide-react";
import * as api from "../api";
import { JobVerb } from "../api";
import { listFleetQueues, FleetQueueRow } from "../api-fleet";
import { listViews, SavedView } from "../api-views";
import { fuzzyFilter, FuzzyMatch } from "../lib/fuzzy";
import { parseTaskIdInput, STATE_BULK_VERBS, TASK_LOOKUP_STATES } from "../lib/palette";
import { parseConsoleState, serializePeek } from "../lib/urlstate";
import { isAqlQuery } from "../lib/aql";
import { paths, queueDetailsPath } from "../paths";
import { cn } from "../lib/utils";
import { FcChip, Kbd } from "./FleetBits";
import BulkJobModal, { BulkJobScope } from "./BulkJobModal";

const MAX_QUEUE_ROWS = 6;
// High enough that the five system views never crowd out user views.
const MAX_VIEW_ROWS = 10;

interface PaletteEntry {
  key: string;
  kind: "queue" | "view" | "verb" | "task" | "go to";
  label: string;
  match?: FuzzyMatch;
  hint?: string; // right-aligned muted text
  chip?: { tone: "crit" | "warn" | "good" | "mut"; text: string };
  run: () => void;
}

// Highlight matched characters (mockup: <mark> = accent bold, no bg).
function Highlighted({ text, match }: { text: string; match?: FuzzyMatch }) {
  if (!match || match.indices.length === 0) return <>{text}</>;
  const marked = new Set(match.indices);
  return (
    <>
      {text.split("").map((c, i) =>
        marked.has(i) ? (
          <mark key={i} className="bg-transparent font-bold text-[var(--fc-acc)]">
            {c}
          </mark>
        ) : (
          <span key={i}>{c}</span>
        )
      )}
    </>
  );
}

interface Props {
  open: boolean;
  onClose: () => void;
}

export default function CommandPalette({ open, onClose }: Props) {
  const navigate = useNavigate();
  const location = useLocation();
  const p = paths();

  const [input, setInput] = useState("");
  const [highlight, setHighlight] = useState(0);
  const [queues, setQueues] = useState<FleetQueueRow[]>([]);
  const [views, setViews] = useState<SavedView[]>([]);
  const [lookup, setLookup] = useState<"idle" | "busy" | "notfound">("idle");
  // Verb handoff: the modal outlives the (closed) overlay.
  const [pendingVerb, setPendingVerb] = useState<{ verb: JobVerb; scope: BulkJobScope } | null>(null);
  const inputRef = useRef<HTMLInputElement>(null);
  const listRef = useRef<HTMLDivElement>(null);

  // Fresh data each open — both reads are cheap (stats cache / small hash
  // set) and staleness in a jump list is worse than the fetch.
  useEffect(() => {
    if (!open) return;
    setInput("");
    setHighlight(0);
    setLookup("idle");
    let disposed = false;
    (async () => {
      try {
        const r = await listFleetQueues({ sort: "name", dir: "asc", limit: 500 });
        if (!disposed) setQueues(r.queues);
      } catch {
        // Fleet cache absent: fall back to the plain queue name list.
        try {
          const r = await api.listQueues();
          if (!disposed) {
            setQueues(
              r.queues.map((q) => ({ queue: q.queue, pending: q.pending, paused: q.paused, consumers: -1 }) as unknown as FleetQueueRow)
            );
          }
        } catch {
          if (!disposed) setQueues([]);
        }
      }
    })();
    (async () => {
      try {
        const r = await listViews();
        if (!disposed) setViews(r.views);
      } catch {
        if (!disposed) setViews([]);
      }
    })();
    // Focus after mount.
    setTimeout(() => inputRef.current?.focus(), 0);
    return () => {
      disposed = true;
    };
  }, [open]);

  const openView = useCallback(
    (v: SavedView) => {
      const params = new URLSearchParams();
      for (const [k, val] of Object.entries(v.state ?? {})) {
        if (val !== "") params.set(k, String(val));
      }
      const base = v.target === "queues" ? p.QUEUES : p.TASKS;
      const qs = params.toString();
      navigate(qs ? `${base}?${qs}` : base);
      onClose();
    },
    [navigate, onClose, p.QUEUES, p.TASKS]
  );

  // Paste-a-task-ID: search across states via the existing search API's
  // id= predicate; the first hit opens the console with the drawer peeked.
  const lookupTask = useCallback(
    async (id: string, queue?: string) => {
      setLookup("busy");
      try {
        for (const st of TASK_LOOKUP_STATES) {
          const resp = await api.searchTasks({
            state: st,
            q: `id=${id}`,
            queue: queue || undefined,
            size: 1,
          });
          const hit = (resp.tasks ?? []).find((t) => t.id === id);
          if (hit) {
            const params = new URLSearchParams();
            params.set("q", `id=${id} state=${st}`);
            params.set("peek", serializePeek({ queue: hit.queue, id: hit.id }));
            navigate(`${p.TASKS}?${params.toString()}`);
            onClose();
            return;
          }
        }
        setLookup("notfound");
      } catch {
        setLookup("notfound");
      }
    },
    [navigate, onClose, p.TASKS]
  );

  // Current console scope for verb entries — only meaningful on /tasks.
  const onTasksConsole = location.pathname === p.TASKS;
  const consoleState = useMemo(
    () => (onTasksConsole ? parseConsoleState(new URLSearchParams(location.search)) : null),
    [onTasksConsole, location.search]
  );

  const taskId = useMemo(() => parseTaskIdInput(input), [input]);

  const entries = useMemo<PaletteEntry[]>(() => {
    if (taskId) {
      return [
        {
          key: "task-lookup",
          kind: "task",
          label:
            lookup === "busy"
              ? `Looking up ${taskId.id} across states…`
              : lookup === "notfound"
                ? `No task ${taskId.id} found in any state (scan window may be partial)`
                : `Open task ${taskId.id}`,
          run: () => {
            if (lookup === "idle") lookupTask(taskId.id, taskId.queue);
          },
        },
      ];
    }

    const out: PaletteEntry[] = [];

    // Queues — fuzzy jump.
    for (const { item, match } of fuzzyFilter(input, queues, (q) => q.queue, MAX_QUEUE_ROWS)) {
      const uncovered = item.consumers === 0 && item.pending > 0;
      out.push({
        key: `queue-${item.queue}`,
        kind: "queue",
        label: item.queue,
        match,
        hint:
          item.pending >= 0 && Number.isFinite(item.pending)
            ? `${item.pending.toLocaleString("en-US")} pending`
            : undefined,
        chip: uncovered
          ? { tone: "crit", text: "no consumers" }
          : item.paused
            ? { tone: "warn", text: "paused" }
            : undefined,
        run: () => {
          navigate(queueDetailsPath(item.queue));
          onClose();
        },
      });
    }

    // Saved views — system + user.
    for (const { item, match } of fuzzyFilter(input, views, (v) => v.name, MAX_VIEW_ROWS)) {
      out.push({
        key: `view-${item.id}`,
        kind: "view",
        label: item.name,
        match,
        hint: item.system ? undefined : `· ${item.created_by}`,
        chip: item.system ? { tone: "mut", text: "system" } : undefined,
        run: () => openView(item),
      });
    }

    // Verbs on the current scope (tasks console only; queue-directory bulk
    // verbs have no job-runner backend yet).
    if (consoleState && !window.READ_ONLY) {
      const verbs = STATE_BULK_VERBS[consoleState.state] ?? [];
      const scope: BulkJobScope = {
        queue: consoleState.queue,
        state: consoleState.state,
        q: isAqlQuery(consoleState.q) ? "" : consoleState.q,
        meta: [],
        aql: isAqlQuery(consoleState.q) ? consoleState.q : "",
      };
      const verbEntries = verbs.map((verb) => ({
        verb,
        label: `${verb.charAt(0).toUpperCase()}${verb.slice(1)} all matching current filter…`,
      }));
      for (const { item, match } of fuzzyFilter(input, verbEntries, (v) => v.label, 4)) {
        out.push({
          key: `verb-${item.verb}`,
          kind: "verb",
          label: item.label,
          match,
          hint: consoleState.q ? consoleState.q : `state=${consoleState.state}`,
          run: () => {
            setPendingVerb({ verb: item.verb, scope });
            onClose();
          },
        });
      }
    }

    // Go to — every nav item.
    const nav: Array<[string, string]> = [
      ["Overview", p.HOME],
      ["Queues", p.QUEUES],
      ["Tasks", p.TASKS],
      ["Errors", p.ERRORS],
      ["Workers", p.SERVERS],
      ["Schedulers", p.SCHEDULERS],
      ["Operations", p.OPS],
      ["Hygiene", p.HYGIENE],
      ["Redis", p.REDIS],
      ["Settings", p.SETTINGS],
      ...(window.PROMETHEUS_SERVER_ADDRESS ? ([["Metrics", p.QUEUE_METRICS]] as Array<[string, string]>) : []),
    ];
    for (const { item, match } of fuzzyFilter(input, nav, ([label]) => label, input ? 4 : nav.length)) {
      out.push({
        key: `goto-${item[0]}`,
        kind: "go to",
        label: item[0],
        match,
        run: () => {
          navigate(item[1]);
          onClose();
        },
      });
    }

    if (out.length === 0 && input !== "") {
      out.push({
        key: "empty",
        kind: "task",
        label: "No matches — paste a task ID to open it in any state",
        run: () => {},
      });
    }
    return out;
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [taskId, lookup, input, queues, views, consoleState, navigate, onClose, openView, lookupTask]);

  // Clamp the highlight when the list shrinks.
  useEffect(() => {
    setHighlight((h) => Math.min(h, Math.max(0, entries.length - 1)));
  }, [entries.length]);

  // Keep the highlighted row scrolled into view.
  useEffect(() => {
    listRef.current
      ?.querySelector(`[data-pal-idx="${highlight}"]`)
      ?.scrollIntoView({ block: "nearest" });
  }, [highlight]);

  const onKeyDown = (e: React.KeyboardEvent) => {
    if (e.key === "Escape") {
      e.preventDefault();
      e.stopPropagation();
      onClose();
    } else if (e.key === "ArrowDown") {
      e.preventDefault();
      setHighlight((h) => Math.min(h + 1, entries.length - 1));
    } else if (e.key === "ArrowUp") {
      e.preventDefault();
      setHighlight((h) => Math.max(h - 1, 0));
    } else if (e.key === "Enter") {
      e.preventDefault();
      entries[highlight]?.run();
    } else if (e.key === "Tab") {
      // Focus trap: the input is the palette's only tab stop.
      e.preventDefault();
    }
  };

  return (
    <>
      {open && (
        <div
          className="fixed inset-0 z-[60] flex justify-center bg-[rgba(4,8,14,0.6)] pt-[12vh]"
          onMouseDown={(e) => {
            if (e.target === e.currentTarget) onClose();
          }}
        >
          <div
            role="dialog"
            aria-modal="true"
            aria-label="Command palette"
            className="h-fit w-[560px] max-w-[92vw] overflow-hidden rounded-[10px] border border-[var(--fc-line)] bg-[var(--fc-panel)] shadow-xl"
          >
            <div className="flex items-center gap-2.5 border-b border-[var(--fc-line)] px-3.5 py-3 text-sm">
              <Search size={14} aria-hidden className="text-[var(--fc-ink3)]" />
              <input
                ref={inputRef}
                value={input}
                onChange={(e) => {
                  setInput(e.target.value);
                  setHighlight(0);
                  setLookup("idle");
                }}
                onKeyDown={onKeyDown}
                placeholder="Jump to queue, run a verb, paste a task ID…"
                aria-label="Command palette search"
                role="combobox"
                aria-expanded="true"
                aria-controls="fc-palette-list"
                aria-activedescendant={entries[highlight] ? `fc-pal-opt-${highlight}` : undefined}
                spellCheck={false}
                autoComplete="off"
                className="flex-1 border-0 bg-transparent font-mono text-sm text-[var(--fc-ink)] outline-none placeholder:text-[var(--fc-ink3)]"
              />
              <Kbd>esc</Kbd>
            </div>

            <div
              ref={listRef}
              id="fc-palette-list"
              role="listbox"
              aria-label="Results"
              className="max-h-[50vh] overflow-y-auto"
            >
              {entries.map((entry, i) => (
                <div
                  key={entry.key}
                  id={`fc-pal-opt-${i}`}
                  data-pal-idx={i}
                  role="option"
                  aria-selected={i === highlight}
                  onMouseEnter={() => setHighlight(i)}
                  onMouseDown={(e) => {
                    e.preventDefault(); // keep focus in the input
                    entry.run();
                  }}
                  className={cn(
                    "flex cursor-pointer items-center gap-2.5 border-b border-[var(--fc-line2)] px-3.5 py-[9px] text-[13px] text-[var(--fc-ink)] last:border-b-0",
                    i === highlight && "bg-[var(--fc-acc-bg)]"
                  )}
                >
                  <span className="w-[52px] shrink-0 text-[10.5px] uppercase tracking-[0.06em] text-[var(--fc-ink3)]">
                    {entry.kind}
                  </span>
                  <span className={cn("truncate", entry.kind === "queue" && "font-mono text-xs")}>
                    <Highlighted text={entry.label} match={entry.match} />
                  </span>
                  <span className="flex-1" />
                  {entry.hint && (
                    <span className="max-w-[180px] truncate font-mono text-[11px] tabular-nums text-[var(--fc-ink3)]">
                      {entry.hint}
                    </span>
                  )}
                  {entry.chip && <FcChip tone={entry.chip.tone}>{entry.chip.text}</FcChip>}
                  {i === highlight && (
                    <CornerDownLeft size={12} aria-hidden className="shrink-0 text-[var(--fc-ink3)]" />
                  )}
                </div>
              ))}
              {entries.length === 0 && (
                <div className="px-3.5 py-6 text-center text-xs text-[var(--fc-ink3)]">
                  Type to jump to a queue, launch a saved view, or paste a task ID.
                </div>
              )}
            </div>
          </div>
        </div>
      )}

      {/* §4.3 bulk-verb handoff — survives the palette closing. */}
      {pendingVerb && (
        <BulkJobModal
          open
          verb={pendingVerb.verb}
          scope={pendingVerb.scope}
          onClose={() => setPendingVerb(null)}
          onStarted={() => setPendingVerb(null)}
        />
      )}
    </>
  );
}
