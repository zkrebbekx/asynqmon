// Fleet Console topbar (build contract §2 chrome): breadcrumb, live/paused
// pill with "updated Ns ago", READ-ONLY badge, theme toggle. Styled with the
// --fc-* tokens from the approved mockup. Rendered as the 44px top grid row
// spanning nav + main.

import { useEffect, useState } from "react";
import { useDispatch, useSelector } from "react-redux";
import { Link, matchPath, useLocation } from "react-router-dom";
import { Sun, Moon, Pause } from "lucide-react";
import { AppState } from "../store";
import { ThemePreference } from "../reducers/settingsReducer";
import { selectTheme, togglePolling } from "../actions/settingsActions";
import { useIsDark } from "../hooks";
import { timeAgoUnix } from "../utils";
import { cn } from "../lib/utils";
import { paths, queueDetailsPath } from "../paths";

interface Crumb {
  label: string;
  to?: string;
}

// matchPath returns the RAW path segment, unlike useParams, which decodes it.
// Queue names routinely carry a colon ("email:send"), which the path builders
// percent-encode, so an undecoded label read "email%3Asend" and a link built
// from it encoded a second time ("email%253Asend"). A malformed sequence makes
// decodeURIComponent throw, so fall back to the raw segment.
function decodeSegment(segment: string): string {
  try {
    return decodeURIComponent(segment);
  } catch {
    return segment;
  }
}

function useBreadcrumbs(): Crumb[] {
  const { pathname } = useLocation();
  const p = paths();

  const task = matchPath(p.TASK_DETAILS, pathname);
  if (task?.params.qname) {
    const qname = decodeSegment(task.params.qname);
    return [
      { label: "Queues", to: p.QUEUES },
      { label: qname, to: queueDetailsPath(qname) },
      { label: "task" },
    ];
  }
  const queue = matchPath(p.QUEUE_DETAILS, pathname);
  if (queue?.params.qname) {
    return [
      { label: "Queues", to: p.QUEUES },
      { label: decodeSegment(queue.params.qname) },
    ];
  }

  const sections: Array<[string, string]> = [
    [p.QUEUES, "Queues"],
    [p.TASKS, "Tasks"],
    [p.ERRORS, "Errors"],
    [p.SERVERS, "Workers"],
    [p.SCHEDULERS, "Schedulers"],
    [p.OPS, "Operations"],
    [p.HYGIENE, "Hygiene"],
    [p.REDIS, "Redis"],
    [p.SETTINGS, "Settings"],
    [p.QUEUE_METRICS, "Metrics"],
  ];
  for (const [path, label] of sections) {
    if (matchPath(path, pathname)) return [{ label }];
  }
  return [{ label: "Overview" }];
}

// True on macOS/iOS — decides whether the palette hint shows ⌘K or Ctrl K.
const IS_MAC = /Mac|iP(hone|ad|od)/.test(navigator.platform);

interface Props {
  onOpenPalette?: () => void;
  onOpenCheatsheet?: () => void;
}

export default function HeaderBar({ onOpenPalette, onOpenCheatsheet }: Props) {
  const dispatch = useDispatch();
  const { pollingActive, lastUpdatedAt, pollInterval } = useSelector(
    (s: AppState) => s.settings
  );
  const dark = useIsDark();
  const crumbs = useBreadcrumbs();

  // Re-render once a second so the "updated Ns ago" label stays fresh.
  const [, setNow] = useState(Date.now());
  useEffect(() => {
    const id = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(id);
  }, []);

  const ago = lastUpdatedAt > 0 ? timeAgoUnix(lastUpdatedAt / 1000) : "waiting for data";

  return (
    <header className="col-span-2 flex h-11 items-center gap-2.5 border-b border-[var(--fc-line)] bg-[var(--fc-panel)] px-3.5 text-[var(--fc-ink)]">
      {/* Brand — sits over the nav column, like the mockup */}
      <div className="flex w-[180px] shrink-0 items-center gap-2 text-[13px] font-semibold tracking-[0.01em]">
        <span aria-hidden className="h-[9px] w-[9px] rounded-[2px] bg-[var(--fc-acc)]" />
        Asynqmon
      </div>

      {/* Breadcrumb */}
      <nav aria-label="Breadcrumb" className="flex min-w-0 items-center gap-1.5 text-xs text-[var(--fc-ink3)]">
        {crumbs.map((c, i) => (
          <span key={`${c.label}-${i}`} className="flex min-w-0 items-center gap-1.5">
            {i > 0 && <span aria-hidden>›</span>}
            {c.to ? (
              <Link to={c.to} className="text-[var(--fc-acc)] hover:underline">
                {c.label}
              </Link>
            ) : (
              <b className="truncate font-semibold text-[var(--fc-ink)]">{c.label}</b>
            )}
          </span>
        ))}
      </nav>

      {/* ⌘K palette hint chip (build contract §2 chrome / §4.1) */}
      {onOpenPalette && (
        <button
          onClick={onOpenPalette}
          title="Open the command palette"
          className="ml-2 flex items-center gap-2 rounded-md border border-[var(--fc-line)] bg-[var(--fc-raise)] px-2.5 py-1 text-xs text-[var(--fc-ink3)] hover:border-[var(--fc-ink3)] hover:text-[var(--fc-ink2)]"
        >
          Search or jump…
          <kbd className="rounded border border-b-2 border-[var(--fc-line)] bg-[var(--fc-panel)] px-[5px] font-sans text-[10.5px] text-[var(--fc-ink2)]">
            {IS_MAC ? "⌘K" : "Ctrl K"}
          </kbd>
        </button>
      )}
      {/* "?" cheatsheet hint: the rich keymap was effectively secret —
          nothing on screen revealed that "?" exists. */}
      {onOpenCheatsheet && (
        <button
          onClick={onOpenCheatsheet}
          aria-label="Keyboard shortcuts"
          title="Keyboard shortcuts"
          className="ml-1.5 flex items-center rounded-md border border-[var(--fc-line)] bg-[var(--fc-raise)] px-2 py-1 text-xs text-[var(--fc-ink3)] hover:border-[var(--fc-ink3)] hover:text-[var(--fc-ink2)]"
        >
          <kbd className="rounded border border-b-2 border-[var(--fc-line)] bg-[var(--fc-panel)] px-[5px] font-sans text-[10.5px] text-[var(--fc-ink2)]">
            ?
          </kbd>
        </button>
      )}

      <div className="flex-1" />

      {/* Live/paused pill with freshness */}
      <button
        onClick={() => dispatch(togglePolling())}
        title={
          pollingActive
            ? `Auto-refresh on (every ${pollInterval}s) — click to pause`
            : "Auto-refresh paused — click to resume"
        }
        className="flex items-center gap-1.5 rounded-full border border-[var(--fc-line)] bg-[var(--fc-panel)] px-2.5 py-1 text-xs text-[var(--fc-ink2)] hover:border-[var(--fc-ink3)]"
      >
        {pollingActive ? (
          <span aria-hidden className="h-[7px] w-[7px] rounded-full bg-[var(--fc-good)]" />
        ) : (
          <Pause size={10} aria-hidden className="text-[var(--fc-warn)]" />
        )}
        <span className="tabular-nums">
          {pollingActive ? "live" : "paused"} · {ago}
        </span>
      </button>

      {/* Read-only badge */}
      {window.READ_ONLY && (
        <span className="rounded border border-[var(--fc-warn)]/40 bg-[var(--fc-warn-bg)] px-1.5 py-[3px] text-[10.5px] font-semibold uppercase tracking-[0.08em] text-[var(--fc-warn)]">
          Read-only
        </span>
      )}

      {/* Theme toggle */}
      <button
        onClick={() =>
          dispatch(selectTheme(dark ? ThemePreference.Never : ThemePreference.Always))
        }
        title={dark ? "Switch to light theme" : "Switch to dark theme"}
        className={cn(
          "inline-flex h-7 w-7 items-center justify-center rounded-md border border-[var(--fc-line)]",
          "text-[var(--fc-ink2)] hover:bg-[var(--fc-raise)] hover:text-[var(--fc-ink)]"
        )}
      >
        {dark ? <Sun size={14} /> : <Moon size={14} />}
      </button>
    </header>
  );
}
