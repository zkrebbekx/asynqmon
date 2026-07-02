import { useEffect, useState } from "react";
import { useDispatch, useSelector } from "react-redux";
import { Sun, Moon, Pause, Play, RefreshCw } from "lucide-react";
import { AppState } from "../store";
import { ThemePreference } from "../reducers/settingsReducer";
import { selectTheme, togglePolling } from "../actions/settingsActions";
import { useIsDark } from "../hooks";
import { timeAgoUnix } from "../utils";
import { cn } from "../lib/utils";
import { Tooltip, TooltipContent, TooltipProvider, TooltipTrigger } from "./ui/tooltip";

export default function HeaderBar() {
  const dispatch = useDispatch();
  const { pollingActive, lastUpdatedAt, pollInterval } = useSelector(
    (s: AppState) => s.settings
  );
  const dark = useIsDark();

  // Re-render once a second so the "updated Ns ago" label stays fresh.
  const [, setNow] = useState(Date.now());
  useEffect(() => {
    const id = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(id);
  }, []);

  const updatedLabel =
    lastUpdatedAt > 0 ? `Updated ${timeAgoUnix(lastUpdatedAt / 1000)}` : "Waiting for data…";

  return (
    <header className="flex items-center justify-end gap-2 h-12 px-4 border-b border-[hsl(var(--border))] bg-[hsl(var(--background))]/80 backdrop-blur-sm sticky top-0 z-20">
      {/* Last updated indicator */}
      <div className="flex items-center gap-1.5 text-xs text-[hsl(var(--muted-foreground))] mr-2">
        <RefreshCw
          size={12}
          className={cn(pollingActive && "animate-[spin_3s_linear_infinite]")}
        />
        <span className="tabular-nums">{updatedLabel}</span>
      </div>

      <TooltipProvider>
        {/* Auto-refresh toggle */}
        <Tooltip>
          <TooltipTrigger asChild>
            <button
              onClick={() => dispatch(togglePolling())}
              className={cn(
                "inline-flex items-center justify-center h-8 w-8 rounded-md border transition-colors",
                pollingActive
                  ? "border-[hsl(var(--primary))]/40 text-[hsl(var(--primary))] hover:bg-[hsl(var(--accent))]"
                  : "border-[hsl(var(--border))] text-[hsl(var(--muted-foreground))] hover:bg-[hsl(var(--accent))]"
              )}
            >
              {pollingActive ? <Pause size={15} /> : <Play size={15} />}
            </button>
          </TooltipTrigger>
          <TooltipContent>
            {pollingActive ? `Auto-refresh on (every ${pollInterval}s) — click to pause` : "Auto-refresh paused — click to resume"}
          </TooltipContent>
        </Tooltip>

        {/* Theme toggle */}
        <Tooltip>
          <TooltipTrigger asChild>
            <button
              onClick={() =>
                dispatch(selectTheme(dark ? ThemePreference.Never : ThemePreference.Always))
              }
              className="inline-flex items-center justify-center h-8 w-8 rounded-md border border-[hsl(var(--border))] text-[hsl(var(--muted-foreground))] hover:bg-[hsl(var(--accent))] hover:text-[hsl(var(--foreground))] transition-colors"
            >
              {dark ? <Sun size={15} /> : <Moon size={15} />}
            </button>
          </TooltipTrigger>
          <TooltipContent>{dark ? "Switch to light theme" : "Switch to dark theme"}</TooltipContent>
        </Tooltip>
      </TooltipProvider>
    </header>
  );
}
