// SchedulersView × duplicate registrations (#54.4): the stable key merges
// identical entries into one row, so the row must say how many live entries
// it stands for. One process that registered the same task twice enqueues it
// twice every tick, and the chip is the only place that shows it.

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import { Provider } from "react-redux";
import { MemoryRouter } from "react-router-dom";
import store from "../store";
import * as api from "../api";
import * as apiFleet from "../api-fleet";
import type { SchedulerRow } from "../api-fleet";
import { resetFeaturesCache } from "../hooks/useFeatures";
import SchedulersView from "./SchedulersView";

vi.mock("../api");
vi.mock("../api-fleet");

function row(taskType: string, liveCount: number | undefined): SchedulerRow {
  return {
    stable_key: taskType.padEnd(64, "x"),
    live: true,
    first_seen: "2026-07-20T00:00:00Z",
    last_seen: "2026-07-26T00:00:00Z",
    live_count: liveCount,
    entry: {
      id: `${taskType}-entry-id`,
      spec: "@every 1h",
      task_type: taskType,
      task_payload: "{}",
      options: ['Queue("dq")'],
      next_enqueue_at: "2026-07-26T02:00:00Z",
      prev_enqueue_at: "2026-07-26T01:00:00Z",
    },
  };
}

function renderView() {
  return render(
    <Provider store={store}>
      <MemoryRouter>
        <SchedulersView />
      </MemoryRouter>
    </Provider>
  );
}

beforeEach(() => {
  vi.clearAllMocks();
  resetFeaturesCache();
  window.ROOT_PATH = "";
  window.READ_ONLY = false;
  vi.mocked(api.getFeatures).mockResolvedValue({ features: { enqueue: false } });
});

describe("SchedulersView duplicate registrations (#54.4)", () => {
  it("chips the row with the live count when more than one entry shares the key", async () => {
    vi.mocked(apiFleet.listSchedulers).mockResolvedValue({
      entries: [row("dup:task", 2), row("solo:task", 1)],
    });
    renderView();

    expect(await screen.findByText("dup:task")).toBeInTheDocument();
    expect(screen.getByText("x2 registered")).toBeInTheDocument();
    // The single registration keeps a plain LIVE chip.
    expect(screen.queryByText("x1 registered")).not.toBeInTheDocument();
    expect(screen.getAllByText("LIVE")).toHaveLength(2);
  });

  it("renders no chip when an older backend omits live_count", async () => {
    vi.mocked(apiFleet.listSchedulers).mockResolvedValue({
      entries: [row("dup:task", undefined)],
    });
    renderView();

    expect(await screen.findByText("dup:task")).toBeInTheDocument();
    expect(screen.queryByText(/registered$/)).not.toBeInTheDocument();
  });
});
