import { useEffect, lazy, Suspense } from "react";
import { useSelector, useDispatch } from "react-redux";
import { BrowserRouter, Routes, Route, NavLink } from "react-router-dom";
import { Toaster, toast } from "sonner";
import {
  BarChart2,
  ListChecks,
  Server,
  Clock,
  Database,
  TrendingUp,
  Settings,
  MessageSquare,
  ChevronLeft,
  ChevronRight,
} from "lucide-react";
import { AppState } from "./store";
import { paths } from "./paths";
import { toggleDrawer } from "./actions/settingsActions";
import { closeSnackbar } from "./actions/snackbarActions";
import { useIsDark } from "./hooks";
import { cn } from "./lib/utils";
import HeaderBar from "./components/HeaderBar";

// Route-level code splitting: each view (and its heavy deps like recharts)
// loads on demand instead of in the initial bundle.
const DashboardView = lazy(() => import("./views/DashboardView"));
const TasksView = lazy(() => import("./views/TasksView"));
const TasksGlobalView = lazy(() => import("./views/TasksGlobalView"));
const TaskDetailsView = lazy(() => import("./views/TaskDetailsView"));
const SchedulersView = lazy(() => import("./views/SchedulersView"));
const ServersView = lazy(() => import("./views/ServersView"));
const RedisInfoView = lazy(() => import("./views/RedisInfoView"));
const MetricsView = lazy(() => import("./views/MetricsView"));
const SettingsView = lazy(() => import("./views/SettingsView"));
const PageNotFoundView = lazy(() => import("./views/PageNotFoundView"));

// Import logo SVGs as URLs
import logoColor from "./images/logo-color.svg";
import logoWhite from "./images/logo-white.svg";

interface NavItemProps {
  to: string;
  icon: React.ReactNode;
  label: string;
  collapsed: boolean;
}

function NavItem({ to, icon, label, collapsed }: NavItemProps) {
  return (
    <NavLink
      to={to}
      end={to === paths().HOME}
      className={({ isActive }) =>
        cn(
          "flex items-center gap-3 px-3 py-2 rounded-r-full text-sm font-medium transition-colors mx-0",
          "hover:bg-[hsl(var(--accent))] hover:text-[hsl(var(--accent-foreground))]",
          isActive
            ? "bg-[hsl(var(--primary))]/10 text-[hsl(var(--primary))]"
            : "text-[hsl(var(--muted-foreground))]"
        )
      }
    >
      <span className="shrink-0">{icon}</span>
      {!collapsed && <span className="truncate">{label}</span>}
    </NavLink>
  );
}

function AppContent() {
  const dispatch = useDispatch();
  const { isDrawerOpen } = useSelector((s: AppState) => s.settings);
  const snackbar = useSelector((s: AppState) => s.snackbar);
  const isDark = useIsDark();
  const appPaths = paths();

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

  const navItems = [
    { to: appPaths.HOME, icon: <BarChart2 size={18} />, label: "Queues" },
    { to: appPaths.TASKS, icon: <ListChecks size={18} />, label: "Tasks" },
    { to: appPaths.SERVERS, icon: <Server size={18} />, label: "Servers" },
    { to: appPaths.SCHEDULERS, icon: <Clock size={18} />, label: "Schedulers" },
    { to: appPaths.REDIS, icon: <Database size={18} />, label: "Redis" },
    ...(window.PROMETHEUS_SERVER_ADDRESS
      ? [{ to: appPaths.QUEUE_METRICS, icon: <TrendingUp size={18} />, label: "Metrics" }]
      : []),
  ];

  return (
    <div className="flex h-screen bg-[hsl(var(--background))] overflow-hidden">
      {/* Sidebar */}
      <aside
        className={cn(
          "flex flex-col border-r border-[hsl(var(--border))] bg-[hsl(var(--background))] transition-all duration-200 shrink-0",
          isDrawerOpen ? "w-[220px]" : "w-14"
        )}
      >
        {/* Logo */}
        <div className="flex items-center h-14 px-3 border-b border-[hsl(var(--border))]">
          {isDrawerOpen ? (
            <img
              src={isDark ? logoWhite : logoColor}
              alt="Asynqmon"
              className="h-7 object-contain"
            />
          ) : (
            <span className="text-[hsl(var(--primary))] font-bold text-lg">A</span>
          )}
        </div>

        {/* Nav */}
        <nav className="flex-1 py-4 space-y-1 overflow-y-auto">
          {navItems.map((item) => (
            <NavItem key={item.to} {...item} collapsed={!isDrawerOpen} />
          ))}
        </nav>

        {/* Bottom nav */}
        <div className="border-t border-[hsl(var(--border))] py-2 space-y-1">
          <NavItem
            to={appPaths.SETTINGS}
            icon={<Settings size={18} />}
            label="Settings"
            collapsed={!isDrawerOpen}
          />
          <a
            href="https://github.com/hibiken/asynqmon/issues"
            target="_blank"
            rel="noreferrer"
            className={cn(
              "flex items-center gap-3 px-3 py-2 rounded-r-full text-sm font-medium transition-colors",
              "text-[hsl(var(--muted-foreground))] hover:bg-[hsl(var(--accent))] hover:text-[hsl(var(--accent-foreground))]"
            )}
          >
            <MessageSquare size={18} className="shrink-0" />
            {isDrawerOpen && <span className="truncate">Feedback</span>}
          </a>
          <button
            onClick={() => dispatch(toggleDrawer())}
            className={cn(
              "flex items-center gap-3 px-3 py-2 w-full rounded-r-full text-sm font-medium transition-colors",
              "text-[hsl(var(--muted-foreground))] hover:bg-[hsl(var(--accent))] hover:text-[hsl(var(--accent-foreground))]"
            )}
          >
            {isDrawerOpen ? <ChevronLeft size={18} /> : <ChevronRight size={18} />}
            {isDrawerOpen && <span>Collapse</span>}
          </button>
        </div>
      </aside>

      {/* Main content */}
      <main className="flex-1 overflow-y-auto">
        <HeaderBar />
        <Suspense
          fallback={
            <div className="flex h-64 items-center justify-center text-sm text-[hsl(var(--muted-foreground))]">
              Loading…
            </div>
          }
        >
          <Routes>
            <Route path={appPaths.TASK_DETAILS} element={<TaskDetailsView />} />
            <Route path={appPaths.TASKS} element={<TasksGlobalView />} />
            <Route path={appPaths.QUEUE_DETAILS} element={<TasksView />} />
            <Route path={appPaths.SCHEDULERS} element={<SchedulersView />} />
            <Route path={appPaths.SERVERS} element={<ServersView />} />
            <Route path={appPaths.REDIS} element={<RedisInfoView />} />
            <Route path={appPaths.SETTINGS} element={<SettingsView />} />
            <Route path={appPaths.QUEUE_METRICS} element={<MetricsView />} />
            <Route path={appPaths.HOME} element={<DashboardView />} />
            <Route path="*" element={<PageNotFoundView />} />
          </Routes>
        </Suspense>
      </main>

      <Toaster position="bottom-left" />
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
