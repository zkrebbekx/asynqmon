// SettingsView › Fleet Console Health (§3.12, phase 12): per-role lease
// holders, tier/budget/coverage readout, standby honesty, unavailable and
// endpoint-error states.

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import { Provider } from "react-redux";
import store from "../store";
import * as apiFleet from "../api-fleet";
import type { HealthRolesResponse } from "../api-fleet";
import SettingsView from "./SettingsView";

vi.mock("../api-fleet");

const healthFixture = (over?: Partial<HealthRolesResponse>): HealthRolesResponse => ({
  roles: [
    {
      role: "stats",
      title: "Stats sweeper",
      holder: "web-1:100:ab12",
      held_by_this_replica: true,
      ttl_ms: 12000,
      fence: 4,
    },
    {
      role: "errsig",
      title: "Error-signature indexer",
      holder: "web-2:200:cd34",
      held_by_this_replica: false,
      ttl_ms: 9000,
      fence: 2,
    },
    {
      role: "hygiene",
      title: "Hygiene scheduler",
      holder: "",
      held_by_this_replica: false,
      ttl_ms: 0,
      fence: 7,
    },
  ],
  stats: {
    available: true,
    tier: 2,
    command_budget: 2000,
    hot_set_size: 512,
    full_rotation_seconds_est: 150,
    queues_total: 5000,
    coverage_pct_5m: 96.2,
    updated_at: new Date().toISOString(),
    source: "local",
    last_sweep_local: {
      at: new Date().toISOString(),
      queues: 5000,
      refreshed: 700,
      read_cmds: 9800,
      write_cmds: 1500,
      duration_ms: 210,
    },
    series: { mode: "hot+rotation", queue_ceiling: 50000 },
  },
  updated_at: new Date().toISOString(),
  ...over,
});

function renderView() {
  return render(
    <Provider store={store}>
      <SettingsView />
    </Provider>
  );
}

beforeEach(() => {
  vi.clearAllMocks();
  window.ROOT_PATH = "";
});

describe("SettingsView health section", () => {
  it("renders per-role lease holders with TTL, fence and this-replica badge", async () => {
    vi.mocked(apiFleet.getHealthRoles).mockResolvedValue(healthFixture());
    renderView();

    expect(await screen.findByText("Console Health")).toBeInTheDocument();
    expect(screen.getByText("Stats sweeper")).toBeInTheDocument();
    expect(screen.getByText("Error-signature indexer")).toBeInTheDocument();
    expect(screen.getByText("Hygiene scheduler")).toBeInTheDocument();

    expect(screen.getByText(/web-1:100:ab12/)).toBeInTheDocument();
    expect(screen.getByText("this replica")).toBeInTheDocument();
    // An unheld role says so instead of showing a blank.
    expect(screen.getByText("unheld")).toBeInTheDocument();
    // Fencing counters render.
    expect(screen.getByText("#4")).toBeInTheDocument();
    expect(screen.getByText("#7")).toBeInTheDocument();
  });

  it("renders the tier, budget, coverage and rotation readout", async () => {
    vi.mocked(apiFleet.getHealthRoles).mockResolvedValue(healthFixture());
    renderView();

    expect(await screen.findByTestId("tier-badge")).toHaveTextContent("TIER 2");
    expect(screen.getByText("2000 cmd/s")).toBeInTheDocument();
    expect(screen.getByText("96.2%")).toBeInTheDocument();
    expect(screen.getByText("5000")).toBeInTheDocument(); // queues
    expect(screen.getByText("512")).toBeInTheDocument(); // hot set
    expect(screen.getByText("2m 30s")).toBeInTheDocument(); // rotation est
    expect(screen.getByText("hot+rotation")).toBeInTheDocument();
    expect(screen.getByText("50,000")).toBeInTheDocument(); // series ceiling
    // Last local sweep cost line.
    expect(screen.getByText(/9800 reads · 1500 writes · 210ms · 700\/5000 queues refreshed/)).toBeInTheDocument();
  });

  it("says every-tick when the budget covers the whole fleet", async () => {
    const fx = healthFixture();
    fx.stats.full_rotation_seconds_est = 0;
    vi.mocked(apiFleet.getHealthRoles).mockResolvedValue(fx);
    renderView();

    expect(await screen.findByText("every tick")).toBeInTheDocument();
  });

  it("is honest on a standby replica that never swept", async () => {
    const fx = healthFixture();
    delete fx.stats.last_sweep_local;
    vi.mocked(apiFleet.getHealthRoles).mockResolvedValue(fx);
    renderView();

    expect(
      await screen.findByText(/This replica has not swept \(standby — costs are measured on the lease holder\)\./)
    ).toBeInTheDocument();
  });

  it("shows the reason when the stats engine is unavailable", async () => {
    vi.mocked(apiFleet.getHealthRoles).mockResolvedValue(
      healthFixture({
        stats: { available: false, reason: "stats engine is disabled on this replica" },
      })
    );
    renderView();

    expect(await screen.findByText("stats engine is disabled on this replica")).toBeInTheDocument();
  });

  it("degrades to a labeled warning when the endpoint is unreachable", async () => {
    vi.mocked(apiFleet.getHealthRoles).mockRejectedValue(new Error("network"));
    renderView();

    expect(
      await screen.findByText(/Health endpoint unreachable/)
    ).toBeInTheDocument();
  });
});
