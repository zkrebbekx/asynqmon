import { useEffect, useState, lazy, Suspense } from "react";
import { useSelector, useDispatch } from "react-redux";
import { BrowserRouter, Routes, Route, NavLink } from "react-router-dom";
import { Toaster, toast } from "sonner";
import {
  Activity,
  AlertTriangle,
  Layers,
  ListChecks,
  Play,
  Server,
  Clock,
  Database,
  Sparkles,
  TrendingUp,
  Settings,
  ChevronLeft,
  ChevronRight,
} from "lucide-react";
import { AppState } from "./store";
import { paths } from "./paths";
import { toggleDrawer } from "./actions/settingsActions";
import { closeSnackbar } from "./actions/snackbarActions";
import { useIsDark } from "./hooks";
import { useKeymap } from "./hooks/useKeymap";
import { OVERLAY_ATTR } from "./lib/keymap";
import { cn } from "./lib/utils";
import HeaderBar from "./components/HeaderBar";
import CommandPalette from "./components/CommandPalette";
import KeyboardCheatsheet from "./components/KeyboardCheatsheet";

// Route-level code splitting: each view (and its heavy deps like recharts)
// loads on demand instead of in the initial bundle.
const FleetView = lazy(() => import("./views/FleetView"));
const QueuesDirectoryView = lazy(() => import("./views/QueuesDirectoryView"));
const TasksView = lazy(() => import("./views/TasksView"));
const TasksGlobalView = lazy(() => import("./views/TasksGlobalView"));
const ErrorsView = lazy(() => import("./views/ErrorsView"));
// Legacy task-details deep links redirect into the Tasks console drawer.
const TaskDetailsRedirect = lazy(() => import("./views/TaskDetailsRedirect"));
const SchedulersView = lazy(() => import("./views/SchedulersView"));
const ServersView = lazy(() => import("./views/ServersView"));
const OpsView = lazy(() => import("./views/OpsView"));
const HygieneView = lazy(() => import("./views/HygieneView"));
const RedisInfoView = lazy(() => import("./views/RedisInfoView"));
const MetricsView = lazy(() => import("./views/MetricsView"));
const SettingsView = lazy(() => import("./views/SettingsView"));
const PageNotFoundView = lazy(() => import("./views/PageNotFoundView"));

interface NavItemProps {
  to: string;
  icon: React.ReactNode;
  label: string;
  collapsed: boolean;
  end?: boolean;
}

function NavItem({ to, icon, label, collapsed, end }: NavItemProps) {
  return (
    <NavLink
      to={to}
      end={end}
      title={collapsed ? label : undefined}
      className={({ isActive }) =>
        cn(
          "flex items-center gap-2.5 rounded-md px-2.5 py-[7px] text-[13px] transition-colors",
          isActive
            ? "bg-[var(--fc-acc-bg)] font-semibold text-[var(--fc-acc)]"
            : "text-[var(--fc-ink2)] hover:bg-[var(--fc-raise)] hover:text-[var(--fc-ink)]"
        )
      }
    >
      <span className="w-[18px] shrink-0 text-center">{icon}</span>
      {!collapsed && <span className="truncate">{label}</span>}
    </NavLink>
  );
}

// Micro-label above each nav group: letterspaced caps, per the mockup.
function NavGroupLabel({ label, collapsed }: { label: string; collapsed: boolean }) {
  if (collapsed) {
    return <hr className="mx-1 my-2 border-0 border-t border-[var(--fc-line2)]" />;
  }
  return (
    <div className="px-2.5 pb-1 pt-2.5 text-[10px] uppercase tracking-[0.1em] text-[var(--fc-ink3)]">
      {label}
    </div>
  );
}

function AppContent() {
  const dispatch = useDispatch();
  const { isDrawerOpen } = useSelector((s: AppState) => s.settings);
  const snackbar = useSelector((s: AppState) => s.snackbar);
  const isDark = useIsDark();
  const appPaths = paths();

  // Global keyboard model (§4.5): ⌘K palette, "?" cheat-sheet, "/" focuses
  // the active page's query/filter bar (inputs opt in via data-fc-querybar).
  const [paletteOpen, setPaletteOpen] = useState(false);
  const [cheatOpen, setCheatOpen] = useState(false);
  useKeymap({
    "mod+k": {
      onTrigger: () => setPaletteOpen((o) => !o),
      allowInInput: true,
      allowInOverlay: true,
    },
    "?": { onTrigger: () => setCheatOpen((o) => !o), allowInOverlay: true },
    "/": () => {
      document.querySelector<HTMLElement>("[data-fc-querybar]")?.focus();
    },
  });

  // While an overlay is open, page-level bindings (j/k/x…) go quiet — the
  // body attribute is the guard lib/keymap's dispatcher checks.
  useEffect(() => {
    const open = paletteOpen || cheatOpen;
    document.body.toggleAttribute(OVERLAY_ATTR, open);
    return () => document.body.removeAttribute(OVERLAY_ATTR);
  }, [paletteOpen, cheatOpen]);

  // Apply dark class to root
  useEffect(() => {
    document.documentElement.classList.toggle("dark", isDark);
  }, [isDark]);

  // Show toast when snackbar opens
  useEffect(() => {
    if (snackbar.isOpen && snackbar.message) {
      toast(snackbar.message);
      dispatch(closeSnackbar());
    }
  }, [snackbar.isOpen, snackbar.message, dispatch]);

  const collapsed = !isDrawerOpen;

  return (
    <div
      className={cn(
        "grid h-screen grid-rows-[44px_1fr] overflow-hidden bg-[var(--fc-bg)]",
        collapsed ? "grid-cols-[56px_1fr]" : "grid-cols-[200px_1fr]"
      )}
    >
      <HeaderBar onOpenPalette={() => setPaletteOpen(true)} />

      {/* Grouped nav (build contract §2 IA). */}
      <aside className="flex flex-col overflow-y-auto border-r border-[var(--fc-line)] bg-[var(--fc-panel)] px-2 py-2">
        <nav className="flex flex-1 flex-col gap-px">
          <NavGroupLabel label="Monitor" collapsed={collapsed} />
          <NavItem to={appPaths.HOME} end icon={<Activity size={15} />} label="Overview" collapsed={collapsed} />
          <NavItem to={appPaths.QUEUES} icon={<Layers size={15} />} label="Queues" collapsed={collapsed} />
          <NavItem to={appPaths.TASKS} icon={<ListChecks size={15} />} label="Tasks" collapsed={collapsed} />
          <NavItem to={appPaths.ERRORS} icon={<AlertTriangle size={15} />} label="Errors" collapsed={collapsed} />
          {window.PROMETHEUS_SERVER_ADDRESS && (
            <NavItem to={appPaths.QUEUE_METRICS} icon={<TrendingUp size={15} />} label="Metrics" collapsed={collapsed} />
          )}
          <NavGroupLabel label="Operate" collapsed={collapsed} />
          <NavItem to={appPaths.SERVERS} icon={<Server size={15} />} label="Workers" collapsed={collapsed} />
          <NavItem to={appPaths.SCHEDULERS} icon={<Clock size={15} />} label="Schedulers" collapsed={collapsed} />
          <NavItem to={appPaths.OPS} icon={<Play size={15} />} label="Operations" collapsed={collapsed} />
          <NavItem to={appPaths.HYGIENE} icon={<Sparkles size={15} />} label="Hygiene" collapsed={collapsed} />
          <NavGroupLabel label="System" collapsed={collapsed} />
          <NavItem to={appPaths.REDIS} icon={<Database size={15} />} label="Redis" collapsed={collapsed} />
          <NavItem to={appPaths.SETTINGS} icon={<Settings size={15} />} label="Settings" collapsed={collapsed} />
        </nav>

        <button
          onClick={() => dispatch(toggleDrawer())}
          className="flex items-center gap-2.5 rounded-md px-2.5 py-[7px] text-[13px] text-[var(--fc-ink3)] transition-colors hover:bg-[var(--fc-raise)] hover:text-[var(--fc-ink)]"
        >
          <span className="w-[18px] shrink-0 text-center">
            {collapsed ? <ChevronRight size={15} /> : <ChevronLeft size={15} />}
          </span>
          {!collapsed && <span>Collapse</span>}
        </button>
      </aside>

      {/* Main content */}
      <main className="overflow-y-auto">
        <Suspense
          fallback={
            <div className="flex h-64 items-center justify-center text-sm text-[var(--fc-ink3)]">
              Loading…
            </div>
          }
        >
          <Routes>
            <Route path={appPaths.TASK_DETAILS} element={<TaskDetailsRedirect />} />
            <Route path={appPaths.TASKS} element={<TasksGlobalView />} />
            <Route path={appPaths.ERRORS} element={<ErrorsView />} />
            <Route path={appPaths.QUEUE_DETAILS} element={<TasksView />} />
            <Route path={appPaths.QUEUES} element={<QueuesDirectoryView />} />
            <Route path={appPaths.SCHEDULERS} element={<SchedulersView />} />
            <Route path={appPaths.SERVERS} element={<ServersView />} />
            <Route path={appPaths.OPS} element={<OpsView />} />
            <Route path={appPaths.HYGIENE} element={<HygieneView />} />
            <Route path={appPaths.REDIS} element={<RedisInfoView />} />
            <Route path={appPaths.SETTINGS} element={<SettingsView />} />
            <Route path={appPaths.QUEUE_METRICS} element={<MetricsView />} />
            <Route path={appPaths.HOME} element={<FleetView />} />
            <Route path="*" element={<PageNotFoundView />} />
          </Routes>
        </Suspense>
      </main>

      {/* §4.1 command palette + §4.5 cheat-sheet overlays */}
      <CommandPalette open={paletteOpen} onClose={() => setPaletteOpen(false)} />
      <KeyboardCheatsheet open={cheatOpen} onClose={() => setCheatOpen(false)} />

      {/* Toasts wear the fc surface tokens in both themes. */}
      <Toaster
        position="bottom-left"
        theme={isDark ? "dark" : "light"}
        toastOptions={{
          style: {
            background: "var(--fc-panel)",
            borderColor: "var(--fc-line)",
            color: "var(--fc-ink)",
          },
        }}
      />
    </div>
  );
}

export default function App() {
  return (
    <BrowserRouter>
      <AppContent />
    </BrowserRouter>
  );
}
