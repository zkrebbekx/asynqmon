import { useCallback, useEffect, useState } from "react";
import { useSelector, useDispatch } from "react-redux";
import { AppState } from "../store";
import { pollIntervalChange, selectTheme } from "../actions/settingsActions";
import { ThemePreference } from "../reducers/settingsReducer";
import { Label } from "../components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "../components/ui/select";
import PageShell, { panelClass } from "../components/PageShell";
import { getHealthRoles, HealthRolesResponse } from "../api-fleet";

// Settings › Health refresh cadence (independent of the global poll slider —
// this is a diagnostics readout, not a data surface).
const HEALTH_REFRESH_MS = 15000;

function fmtDuration(totalSeconds: number): string {
  if (totalSeconds <= 0) return "—";
  if (totalSeconds < 60) return `${totalSeconds}s`;
  const m = Math.floor(totalSeconds / 60);
  const s = totalSeconds % 60;
  if (m < 60) return s > 0 ? `${m}m ${s}s` : `${m}m`;
  const h = Math.floor(m / 60);
  return `${h}h ${m % 60}m`;
}

function fmtTtl(ms: number): string {
  if (ms <= 0) return "—";
  return `${(ms / 1000).toFixed(0)}s`;
}

// TierBadge renders the §5.1 tier as a compact chip with the fc status hues:
// tier 1 quiet, tiers 2-3 accent (they are normal operation at scale, not a
// warning).
function TierBadge({ tier }: { tier: number }) {
  return (
    <span
      data-testid="tier-badge"
      className="inline-flex items-center rounded-[4px] border border-[var(--fc-line)] bg-[var(--fc-acc-bg)] px-1.5 py-px font-mono text-[11px] font-semibold text-[var(--fc-acc)]"
    >
      TIER {tier}
    </span>
  );
}

function coverageTone(pct: number): string {
  if (pct >= 95) return "text-[var(--fc-good)]";
  if (pct >= 75) return "text-[var(--fc-warn)]";
  return "text-[var(--fc-crit)]";
}

// Stat is one labeled readout cell (fc tokens, monospace numerics).
function Stat({ label, value, tone }: { label: string; value: React.ReactNode; tone?: string }) {
  return (
    <div className="rounded-md border border-[var(--fc-line2)] bg-[var(--fc-raise)] px-2.5 py-1.5">
      <div className="text-[10.5px] uppercase tracking-[0.04em] text-[var(--fc-ink3)]">{label}</div>
      <div className={`mt-px font-mono text-[13px] tabular-nums ${tone ?? "text-[var(--fc-ink)]"}`}>{value}</div>
    </div>
  );
}

// FleetHealthSection is the §3.12 "stats-engine tier status readout": per-role
// lease holders, tier/budget/coverage, last local sweep cost, series sampler.
function FleetHealthSection() {
  const [health, setHealth] = useState<HealthRolesResponse | null>(null);
  const [error, setError] = useState<string | null>(null);

  const load = useCallback(() => {
    getHealthRoles()
      .then((resp) => {
        setHealth(resp);
        setError(null);
      })
      .catch(() => {
        setError("Health endpoint unreachable — backend may predate phase 12 or be down.");
      });
  }, []);

  useEffect(() => {
    load();
    const id = setInterval(load, HEALTH_REFRESH_MS);
    return () => clearInterval(id);
  }, [load]);

  const stats = health?.stats;

  return (
    <section className={panelClass}>
      <div className="border-b border-[var(--fc-line2)] px-3 py-2 text-xs font-semibold text-[var(--fc-ink)]">
        Console Health
      </div>
      <div className="px-3 py-3">
        {error && (
          <p className="rounded-md border border-[var(--fc-line)] bg-[var(--fc-warn-bg)] px-3 py-2 text-sm text-[var(--fc-warn)]">
            {error}
          </p>
        )}
        {!error && !health && (
          <p className="text-sm text-[var(--fc-ink3)]">Loading health…</p>
        )}
        {!error && health && (
          <div className="space-y-4">
            {/* Singleton role leases (§5.13) */}
            <div>
              <div className="mb-1.5 text-[11px] uppercase tracking-[0.04em] text-[var(--fc-ink3)]">
                Singleton roles — lease holders
              </div>
              <div className="overflow-x-auto rounded-md border border-[var(--fc-line)]">
                <table className="w-full text-sm">
                  <thead>
                    <tr className="border-b border-[var(--fc-line2)] bg-[var(--fc-raise)] text-left text-[11px] uppercase tracking-[0.04em] text-[var(--fc-ink3)]">
                      <th className="px-3 py-1.5 font-medium">Role</th>
                      <th className="px-3 py-1.5 font-medium">Holder</th>
                      <th className="px-3 py-1.5 font-medium">Lease TTL</th>
                      <th className="px-3 py-1.5 font-medium">Fence</th>
                    </tr>
                  </thead>
                  <tbody>
                    {health.roles.map((r) => (
                      <tr key={r.role} className="border-b border-[var(--fc-line2)] last:border-b-0">
                        <td className="px-3 py-1.5 text-[var(--fc-ink)]">{r.title}</td>
                        <td className="px-3 py-1.5">
                          {r.holder ? (
                            <span className="font-mono text-[12px] text-[var(--fc-ink2)]">
                              {r.holder}
                              {r.held_by_this_replica && (
                                <span className="ml-1.5 rounded-[4px] bg-[var(--fc-good-bg)] px-1 py-px font-sans text-[10.5px] font-semibold text-[var(--fc-good)]">
                                  this replica
                                </span>
                              )}
                            </span>
                          ) : (
                            <span className="text-[12px] text-[var(--fc-crit)]">unheld</span>
                          )}
                        </td>
                        <td className="px-3 py-1.5 font-mono text-[12px] tabular-nums text-[var(--fc-ink2)]">
                          {fmtTtl(r.ttl_ms)}
                        </td>
                        <td className="px-3 py-1.5 font-mono text-[12px] tabular-nums text-[var(--fc-ink2)]">
                          #{r.fence}
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            </div>

            {/* Stats engine (§5.1 governor) */}
            <div>
              <div className="mb-1.5 flex items-center gap-2 text-[11px] uppercase tracking-[0.04em] text-[var(--fc-ink3)]">
                <span>Stats engine</span>
                {stats?.available && <TierBadge tier={stats.tier ?? 1} />}
              </div>
              {!stats?.available ? (
                <p className="text-sm text-[var(--fc-ink2)]">
                  {stats?.reason ?? "Stats engine unavailable."}
                </p>
              ) : (
                <>
                  <div className="grid grid-cols-2 gap-2 sm:grid-cols-4">
                    <Stat label="Command budget" value={`${stats.command_budget ?? 0} cmd/s`} />
                    <Stat
                      label="Coverage <5m"
                      value={`${(stats.coverage_pct_5m ?? 0).toFixed(1)}%`}
                      tone={coverageTone(stats.coverage_pct_5m ?? 0)}
                    />
                    <Stat label="Queues" value={stats.queues_total ?? 0} />
                    <Stat label="Hot set" value={stats.hot_set_size ?? 0} />
                    <Stat
                      label="Full rotation est."
                      value={
                        (stats.full_rotation_seconds_est ?? 0) === 0
                          ? "every tick"
                          : fmtDuration(stats.full_rotation_seconds_est ?? 0)
                      }
                    />
                    <Stat
                      label="Series sampler"
                      value={stats.series?.mode ?? "—"}
                    />
                    <Stat
                      label="Series ceiling"
                      value={stats.series ? stats.series.queue_ceiling.toLocaleString() : "—"}
                    />
                    <Stat label="Data source" value={stats.source ?? "—"} />
                  </div>
                  <div className="mt-2">
                    {stats.last_sweep_local ? (
                      <p className="text-[12px] text-[var(--fc-ink2)]">
                        Last sweep (this replica):{" "}
                        <span className="font-mono tabular-nums text-[var(--fc-ink)]">
                          {stats.last_sweep_local.read_cmds} reads · {stats.last_sweep_local.write_cmds} writes ·{" "}
                          {stats.last_sweep_local.duration_ms}ms · {stats.last_sweep_local.refreshed}/
                          {stats.last_sweep_local.queues} queues refreshed
                        </span>
                      </p>
                    ) : (
                      <p className="text-[12px] text-[var(--fc-ink3)]">
                        This replica has not swept (standby — costs are measured on the lease holder).
                      </p>
                    )}
                  </div>
                </>
              )}
            </div>
          </div>
        )}
      </div>
    </section>
  );
}

export default function SettingsView() {
  const dispatch = useDispatch();
  const { pollInterval, themePreference } = useSelector((s: AppState) => s.settings);
  const [sliderValue, setSliderValue] = useState(pollInterval);

  // Commit on change (debounced) rather than only on mouseup, so keyboard
  // arrow-key adjustments persist too.
  useEffect(() => {
    if (sliderValue === pollInterval) return;
    const id = setTimeout(() => dispatch(pollIntervalChange(sliderValue)), 300);
    return () => clearTimeout(id);
  }, [sliderValue, pollInterval, dispatch]);

  return (
    <PageShell className="max-w-2xl" title="Settings">
      <div className="space-y-3">
        <section className={panelClass}>
          <div className="border-b border-[var(--fc-line2)] px-3 py-2 text-xs font-semibold text-[var(--fc-ink)]">
            Polling Interval
          </div>
          <div className="px-3 py-3">
            <p className="mb-4 text-sm text-[var(--fc-ink2)]">
              Web UI fetches live data every{" "}
              <strong>{sliderValue === 1 ? "1 second" : `${sliderValue} seconds`}</strong>
            </p>
            <input
              type="range"
              min={2}
              max={20}
              step={1}
              value={sliderValue}
              onChange={(e) => setSliderValue(Number(e.target.value))}
              aria-label="Polling interval in seconds"
              className="w-full accent-[var(--fc-acc)]"
            />
            <div className="mt-1 flex justify-between text-xs text-[var(--fc-ink3)]">
              <span>2s</span>
              <span>20s</span>
            </div>
          </div>
        </section>

        <section className={panelClass}>
          <div className="border-b border-[var(--fc-line2)] px-3 py-2 text-xs font-semibold text-[var(--fc-ink)]">
            Dark Theme
          </div>
          <div className="px-3 py-3">
            <div className="flex items-center justify-between">
              <Label className="text-sm text-[var(--fc-ink2)]">Theme preference</Label>
              <Select
                value={String(themePreference)}
                onValueChange={(v) => dispatch(selectTheme(Number(v) as ThemePreference))}
              >
                <SelectTrigger className="w-48">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value={String(ThemePreference.SystemDefault)}>System Default</SelectItem>
                  <SelectItem value={String(ThemePreference.Always)}>Always Dark</SelectItem>
                  <SelectItem value={String(ThemePreference.Never)}>Always Light</SelectItem>
                </SelectContent>
              </Select>
            </div>
          </div>
        </section>

        <FleetHealthSection />
      </div>
    </PageShell>
  );
}
