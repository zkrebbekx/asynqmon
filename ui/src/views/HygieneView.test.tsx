// HygieneView (§3.10): card rendering states, run-now flow, the empty state,
// job-scope construction for the dead-letter one-click action, honest
// "not sampled" storage rendering, and the schedule editor flow.

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Provider } from "react-redux";
import store from "../store";
import * as api from "../api-hygiene";
import type { HygieneCard, HygieneReport } from "../api-hygiene";
import HygieneView from "./HygieneView";

vi.mock("../api-hygiene");

// BulkJobModal is exercised by its own suite; here we only assert the scope
// handed to it by the one-click action.
vi.mock("../components/BulkJobModal", () => ({
  default: (props: { verb: string; scope: unknown }) => (
    <div data-testid="bulk-modal">{JSON.stringify({ verb: props.verb, scope: props.scope })}</div>
  ),
}));

const cards = (over?: Partial<Record<api.HygieneKind, Partial<HygieneCard>>>): HygieneCard[] =>
  (
    [
      ["inventory", "Queue inventory", 604800],
      ["storage", "Storage attribution", 2592000],
      ["dead-letter", "Dead-letter digest", 604800],
      ["scheduler-health", "Scheduler health", 86400],
    ] as const
  ).map(([kind, title, interval]) => ({
    kind,
    title,
    enabled: false,
    interval_seconds: interval,
    last_generated_at: "",
    summary: null,
    ...(over?.[kind] ?? {}),
  }));

function renderView() {
  return render(
    <Provider store={store}>
      <HygieneView />
    </Provider>
  );
}

beforeEach(() => {
  vi.clearAllMocks();
  window.ROOT_PATH = "";
  window.READ_ONLY = false;
});

describe("HygieneView cards", () => {
  it("renders all four report cards with their summary chips", async () => {
    vi.mocked(api.listHygiene).mockResolvedValue({
      reports: cards({
        inventory: {
          enabled: true,
          last_generated_at: new Date().toISOString(),
          summary: {
            dead_queue_candidates: 41,
            zero_consumer_pending: 3,
            paused_over_7d: 2,
            queues_total: 1847,
          },
        },
      }),
    });
    renderView();

    expect(await screen.findByText("Queue inventory")).toBeInTheDocument();
    expect(screen.getByText("Storage attribution")).toBeInTheDocument();
    expect(screen.getByText("Dead-letter digest")).toBeInTheDocument();
    expect(screen.getByText("Scheduler health")).toBeInTheDocument();

    // Summary chips for the generated report; question line for the rest.
    expect(screen.getByText("dead-queue candidates (30d silent)")).toBeInTheDocument();
    expect(screen.getByText("41")).toBeInTheDocument();
    expect(screen.getByTestId("stamp-inventory")).toHaveTextContent(/generated/);
    expect(screen.getByText(/where does the Redis memory actually go/)).toBeInTheDocument();
  });

  it("shows the §3.10 empty state and enables the weekly inventory from it", async () => {
    vi.mocked(api.listHygiene).mockResolvedValue({ reports: cards() });
    vi.mocked(api.putHygieneConfig).mockResolvedValue({
      kind: "inventory",
      enabled: true,
      interval_seconds: 604800,
    });
    renderView();

    expect(
      await screen.findByText("No reports scheduled — enable the weekly inventory to start.")
    ).toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: "Enable weekly inventory" }));
    expect(api.putHygieneConfig).toHaveBeenCalledWith("inventory", {
      enabled: true,
      interval_seconds: 7 * 24 * 3600,
    });
  });

  it("hides the empty state once anything is scheduled or generated", async () => {
    vi.mocked(api.listHygiene).mockResolvedValue({
      reports: cards({ "scheduler-health": { enabled: true } }),
    });
    renderView();
    await screen.findByText("Queue inventory");
    expect(screen.queryByText(/No reports scheduled/)).not.toBeInTheDocument();
  });
});

describe("Run now", () => {
  it("runs the report, shows a spinner while pending, then the fresh report with its stamp", async () => {
    vi.mocked(api.listHygiene).mockResolvedValue({ reports: cards() });
    let resolveRun!: (r: HygieneReport) => void;
    vi.mocked(api.runHygieneReport).mockReturnValue(
      new Promise<HygieneReport>((res) => {
        resolveRun = res;
      })
    );
    renderView();
    await screen.findByText("Scheduler health");

    const runButtons = screen.getAllByRole("button", { name: /Run now/ });
    await userEvent.click(runButtons[3]); // scheduler-health card (display order)
    expect(api.runHygieneReport).toHaveBeenCalledWith("scheduler-health");
    expect(screen.getByTestId("spinner-scheduler-health")).toBeInTheDocument();

    resolveRun({
      kind: "scheduler-health",
      generated_at: new Date().toISOString(),
      summary: { gone: 1, retention_less: 0, zero_consumer_targets: 0, live_entries: 2 },
      scheduler_health: {
        gone: [
          {
            stable_key: "k1",
            task_type: "digest:weekly",
            spec: "0 6 * * 0",
            queue: "email",
            last_seen_at: new Date().toISOString(),
            retention_seconds: 0,
            consumers: 1,
          },
        ],
        retention_less: [],
        zero_consumer_targets: [],
        live_entries: 2,
        snapshots: 3,
      },
    });

    expect(await screen.findByText("digest:weekly")).toBeInTheDocument();
    await waitFor(() =>
      expect(screen.queryByTestId("spinner-scheduler-health")).not.toBeInTheDocument()
    );
    // The card list refetches so the generated-at stamp updates.
    expect(vi.mocked(api.listHygiene).mock.calls.length).toBeGreaterThan(1);
  });
});

describe("dead-letter one-click action", () => {
  it("opens BulkJobModal with the delete-archived-older-than scope from the descriptor", async () => {
    vi.mocked(api.listHygiene).mockResolvedValue({
      reports: cards({
        "dead-letter": {
          last_generated_at: new Date().toISOString(),
          summary: { archived_total: 10015, older_than_30d: 8000, trim_cap_queues: 1 },
        },
      }),
    });
    vi.mocked(api.getHygieneReport).mockResolvedValue({
      kind: "dead-letter",
      generated_at: new Date().toISOString(),
      data_as_of: new Date().toISOString(),
      summary: { archived_total: 10015, older_than_30d: 8000, trim_cap_queues: 1 },
      dead_letter: {
        bucket_labels: ["<1d", "1-7d", "7-30d", ">30d"],
        action_threshold_days: 30,
        queues: [
          {
            queue: "billing:invoice",
            archived: 10000,
            buckets: [100, 400, 1500, 8000],
            older_than_threshold: 8000,
            at_trim_cap: true,
            clusters: [
              { sig: "s1", template: "gw timeout after ~N~s", archived: 7000, lower_bound: true },
            ],
            action: {
              label: "Delete archived older than 30d",
              verb: "delete",
              queue: "billing:invoice",
              state: "archived",
              aql: "died>30d",
              estimate: 8000,
            },
          },
        ],
        queues_with_archived: 1,
        queues_truncated: false,
        clusters_indexed_at: new Date().toISOString(),
      },
    });
    renderView();
    await screen.findByText("Dead-letter digest");

    // Expand the full report, then fire the one-click action.
    const openButtons = screen.getAllByRole("button", { name: "Open report" });
    await userEvent.click(openButtons[2]); // dead-letter card (display order)
    const actionBtn = await screen.findByRole("button", {
      name: /Delete archived older than 30d/,
    });
    await userEvent.click(actionBtn);

    const modal = await screen.findByTestId("bulk-modal");
    expect(JSON.parse(modal.textContent!)).toEqual({
      verb: "delete",
      scope: {
        queue: "billing:invoice",
        state: "archived",
        q: "",
        meta: [],
        aql: "died>30d",
      },
    });
    // The trim-cap flag renders as a form cue, not color alone.
    expect(screen.getByText("10k cap")).toBeInTheDocument();
  });
});

describe("storage honesty", () => {
  it("labels unsampled queues as 'not sampled' and renders the method verbatim", async () => {
    vi.mocked(api.listHygiene).mockResolvedValue({
      reports: cards({
        storage: {
          last_generated_at: new Date().toISOString(),
          summary: {
            estimated_bytes: 1000,
            not_sampled_queues: 7,
            reclaim_24h: 0,
            reclaim_past_due: 0,
          },
        },
      }),
    });
    vi.mocked(api.getHygieneReport).mockResolvedValue({
      kind: "storage",
      generated_at: new Date().toISOString(),
      summary: { estimated_bytes: 1000, not_sampled_queues: 7 },
      storage: {
        memory: [
          {
            queue: "media",
            tasks: 100,
            struct_bytes: 5000,
            sampled_tasks: 16,
            avg_task_bytes: 4200,
            estimated_bytes: 425000,
            sampled_at: new Date().toISOString(),
          },
        ],
        not_sampled_queues: 7,
        sample_per_queue: 16,
        method: "sampled: MEMORY USAGE over the state structures plus head/tail task probes",
        offenders: [{ queue: "media", id: "abcd1234-x", state: "pending", msg_bytes: 4200 }],
        retention: {
          bucket_labels: ["past_due", "<1h", "1-6h", "6-24h", "1-3d", "3-7d", ">7d"],
          fleet: { queue: "", counts: [0, 0, 0, 0, 0, 0, 0], total: 0 },
          queues: [],
          queues_truncated: false,
        },
      },
    });
    renderView();
    await screen.findByText("Storage attribution");

    const openButtons = screen.getAllByRole("button", { name: "Open report" });
    await userEvent.click(openButtons[1]); // storage card (display order)

    expect(await screen.findByText(/7 more queues with tasks were/)).toBeInTheDocument();
    expect(screen.getByText("not sampled")).toBeInTheDocument();
    expect(screen.getByText(/MEMORY USAGE over the state structures/)).toBeInTheDocument();
  });
});

describe("schedule editor", () => {
  it("saves an edited schedule via PUT config", async () => {
    vi.mocked(api.listHygiene).mockResolvedValue({ reports: cards() });
    vi.mocked(api.putHygieneConfig).mockResolvedValue({
      kind: "scheduler-health",
      enabled: true,
      interval_seconds: 86400,
    });
    renderView();
    await screen.findByText("Scheduler health");

    await userEvent.click(screen.getByRole("switch", { name: "Schedule Scheduler health" }));
    await userEvent.click(screen.getByRole("button", { name: "Save schedule" }));

    expect(api.putHygieneConfig).toHaveBeenCalledWith("scheduler-health", {
      enabled: true,
      interval_seconds: 86400,
    });
  });

  it("hides the schedule editor in read-only mode (the PUT is server-blocked)", async () => {
    window.READ_ONLY = true;
    vi.mocked(api.listHygiene).mockResolvedValue({ reports: cards() });
    renderView();
    await screen.findByText("Queue inventory");

    expect(screen.queryAllByRole("switch")).toHaveLength(0);
    // Run-now stays available: reports are reads, audited server-side.
    expect(screen.getAllByRole("button", { name: /Run now/ })).toHaveLength(4);
  });
});
