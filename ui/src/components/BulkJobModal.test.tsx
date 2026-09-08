// BulkJobModal — the §4.3 bulk-mutation confirm screen. The pure gate rules
// live in lib/bulkjob (unit-tested there); this suite proves the WIRING an
// operator meets: the streaming preview meter, the delete gate (delete stays
// unavailable until the preview is complete, and a partial acknowledgment
// cannot open it), the mandatory reason, the typed confirmation for a large
// delete, the LIST-removal cost disclosure, the execute handoff and the live
// execute meter, cancel/close (which cancels an orphan preview), and the
// failure paths.
//
// The live transport is mocked at the useJobProgress boundary, the same way
// TasksGlobalView.jobresults.test does: the modal is a presentational shell
// over that feed, and this suite drives the feed frame by frame.

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, fireEvent, act } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import * as api from "../api";
import { JobCounts, JobDetail, JobMutationVerb } from "../api";
import BulkJobModal, { BulkJobScope } from "./BulkJobModal";

vi.mock("../api");
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));
vi.mock("../hooks/useJobProgress", () => ({ useJobProgress: vi.fn() }));

import { toast } from "sonner";
import { useJobProgress } from "../hooks/useJobProgress";

const mockUseJobProgress = vi.mocked(useJobProgress);

function c(over: Partial<JobCounts> = {}): JobCounts {
  return { candidates: 0, acted: 0, skipped: 0, failed: 0, scanned: 0, ...over };
}

function makeJob(over: Partial<JobDetail> = {}): JobDetail {
  return {
    id: "jb_1",
    verb: "delete",
    scope: { queue: "default", state: "retry", q: "boom", meta: [] },
    phase: "preview",
    state: "previewing",
    throttle: 200,
    reason: "(preview only — not yet confirmed)",
    actor: "tester",
    counts: c(),
    cost_class: "cheap",
    cost_list_len: 0,
    preview_complete: false,
    proceed_on_partial: false,
    created_at: "2026-07-27T00:00:00Z",
    started_at: "",
    finished_at: "",
    preview_completed_at: "",
    fence: 1,
    error: "",
    failures_overflow: 0,
    ctl_pending: "",
    sample: [],
    failures: [],
    failures_total: 0,
    ...over,
  };
}

// The frame the mocked live feed currently serves, and the transport label.
let live: JobDetail | null = null;
let liveSource: "sse" | "poll" = "poll";

const scope: BulkJobScope = { queue: "default", state: "retry", q: "boom", meta: [] };

function renderModal(verb: JobMutationVerb = "delete") {
  const onClose = vi.fn();
  const onStarted = vi.fn();
  const ui = () => (
    <MemoryRouter>
      <BulkJobModal
        open
        verb={verb}
        scope={scope}
        onClose={onClose}
        onStarted={onStarted}
      />
    </MemoryRouter>
  );
  const utils = render(ui());
  // Publish the next live frame: set it, then re-render so the mocked hook
  // hands the modal the new job.
  const feed = (next: JobDetail) => {
    live = next;
    utils.rerender(ui());
  };
  return { ...utils, onClose, onStarted, feed };
}

// A preview that is still counting, and the same preview once complete.
const counting = makeJob({ counts: c({ candidates: 42, scanned: 900 }) });
const complete = makeJob({
  state: "preview_ready",
  preview_complete: true,
  preview_completed_at: "2026-07-27T00:00:05Z",
  counts: c({ candidates: 57, scanned: 4500 }),
});

// Fill the mandatory reason the gate always demands.
async function typeReason(user: ReturnType<typeof userEvent.setup>, text = "flatten gw-timeout storm") {
  await user.type(screen.getByRole("textbox", { name: /Reason/ }), text);
}

function executeButton(verb = "Delete") {
  return screen.getByRole("button", { name: `${verb} as background job` });
}

beforeEach(() => {
  vi.clearAllMocks();
  window.ROOT_PATH = "";
  window.READ_ONLY = false;
  live = counting;
  liveSource = "poll";
  vi.mocked(api.createJob).mockResolvedValue(makeJob());
  vi.mocked(api.getJob).mockImplementation(async () => live ?? makeJob());
  vi.mocked(api.executeJob).mockResolvedValue(makeJob({ phase: "execute", state: "running" }));
  vi.mocked(api.cancelJob).mockResolvedValue(makeJob({ state: "canceled" }));
  mockUseJobProgress.mockImplementation((jobId) => ({
    job: jobId ? live : null,
    source: liveSource,
    error: "",
  }));
});

describe("BulkJobModal preview states", () => {
  it("names the verb in the dialog and echoes the scope it will act on", async () => {
    renderModal("delete");

    expect(
      screen.getByRole("dialog", { name: /Bulk delete — gone forever, not recoverable/ })
    ).toBeInTheDocument();
    expect(
      await screen.findByText('queue=default state=retry q~"boom"')
    ).toBeInTheDocument();
  });

  it("says the preview is starting until the job exists", async () => {
    // The create call never settles: the modal has no job to report yet.
    vi.mocked(api.createJob).mockImplementation(() => new Promise(() => {}));
    renderModal("delete");

    expect(await screen.findByText("starting preview…")).toBeInTheDocument();
  });

  it("reports a counting preview as a lower bound, then the exact count", async () => {
    const { feed } = renderModal("delete");

    expect(await screen.findByText(/≥42 and counting…/)).toBeInTheDocument();
    expect(screen.getByText(/as of .* — the live set changes/)).toBeInTheDocument();

    feed(complete);
    expect(await screen.findByText("57 tasks — exact")).toBeInTheDocument();
  });

  it("creates the preview job with the scope and a placeholder reason", async () => {
    renderModal("archive");

    await screen.findByText(/≥42 and counting…/);
    expect(api.createJob).toHaveBeenCalledWith({
      verb: "archive",
      scope: { queue: "default", state: "retry", q: "boom", meta: undefined, aql: undefined },
      reason: "(preview only — not yet confirmed)",
      throttle: 200,
    });
  });

  it("shows the preview failure instead of a silent empty meter", async () => {
    vi.mocked(api.createJob).mockRejectedValue({
      response: { data: { error: "compiling scope: bad predicate" } },
    });
    renderModal("delete");

    expect(await screen.findByText("compiling scope: bad predicate")).toBeInTheDocument();
  });
});

describe("BulkJobModal delete gate", () => {
  it("keeps delete unavailable while the preview is still counting", async () => {
    const user = userEvent.setup();
    renderModal("delete");

    await screen.findByText(/≥42 and counting…/);
    await typeReason(user);

    expect(executeButton()).toBeDisabled();
    expect(screen.getByTestId("gate-hint")).toHaveTextContent(
      "Delete requires a completed preview. The button unlocks when the count is exact."
    );
    // The verb-semantics block repeats the rule next to the delete warning.
    const semantics = screen
      .getByText("Delete is gone forever — not recoverable.")
      .closest("div")!;
    expect(semantics).toHaveTextContent(
      "Delete requires a completed preview. The button unlocks when the count is exact."
    );

    // Even a forced click cannot execute: the handler re-checks the gate.
    fireEvent.click(executeButton());
    expect(api.executeJob).not.toHaveBeenCalled();
  });

  it("offers no partial-count acknowledgment for delete — the gate has no override", async () => {
    renderModal("delete");

    await screen.findByText(/≥42 and counting…/);
    expect(screen.queryByRole("checkbox")).toBeNull();
  });

  it("unlocks delete once the preview completes", async () => {
    const user = userEvent.setup();
    const { feed, onStarted } = renderModal("delete");

    await screen.findByText(/≥42 and counting…/);
    await typeReason(user);
    expect(executeButton()).toBeDisabled();

    feed(complete);
    await screen.findByText("57 tasks — exact");

    expect(executeButton()).toBeEnabled();
    expect(screen.queryByTestId("gate-hint")).toBeNull();

    await user.click(executeButton());
    expect(api.executeJob).toHaveBeenCalledWith("jb_1", {
      proceed_on_partial: false,
      throttle: 200,
      reason: "flatten gw-timeout storm",
    });
    expect(onStarted).toHaveBeenCalledWith("jb_1");
  });

  it("demands a reason even when the preview is complete", async () => {
    live = complete;
    renderModal("delete");

    await screen.findByText("57 tasks — exact");
    expect(executeButton()).toBeDisabled();
    expect(screen.getByTestId("gate-hint")).toHaveTextContent(
      "A reason is required — it goes to the audit log."
    );
  });

  it("sends the operator's chosen throttle with the execute request", async () => {
    live = complete;
    const user = userEvent.setup();
    renderModal("delete");

    await screen.findByText("57 tasks — exact");
    await typeReason(user);
    await user.selectOptions(screen.getByRole("combobox", { name: "Throttle" }), "500");
    await user.click(executeButton());

    expect(api.executeJob).toHaveBeenCalledWith("jb_1", {
      proceed_on_partial: false,
      throttle: 500,
      reason: "flatten gw-timeout storm",
    });
  });
});

describe("BulkJobModal typed confirmation for a large delete", () => {
  const large = makeJob({
    state: "preview_ready",
    preview_complete: true,
    counts: c({ candidates: 1500, scanned: 9000 }),
  });

  it("blocks until the exact phrase is typed, then executes", async () => {
    live = large;
    const user = userEvent.setup();
    renderModal("delete");

    await screen.findByText("1,500 tasks — exact");
    await typeReason(user);

    expect(executeButton()).toBeDisabled();
    expect(screen.getByTestId("gate-hint")).toHaveTextContent('Type "DELETE 1500" to confirm.');

    const confirm = screen.getByRole("textbox", { name: /Type DELETE 1500 to confirm/ });
    await user.type(confirm, "DELETE 1499");
    expect(executeButton()).toBeDisabled();

    await user.clear(confirm);
    await user.type(confirm, "DELETE 1500");
    expect(executeButton()).toBeEnabled();

    await user.click(executeButton());
    expect(api.executeJob).toHaveBeenCalledTimes(1);
  });

  it("asks for no typed phrase below the threshold", async () => {
    live = complete;
    renderModal("delete");

    await screen.findByText("57 tasks — exact");
    expect(screen.queryByText(/to confirm \(count > 1,000\)/)).toBeNull();
  });
});

describe("BulkJobModal partial-count acknowledgment (non-delete verbs)", () => {
  it("requires the acknowledgment, then passes it to the execute request", async () => {
    const user = userEvent.setup();
    renderModal("archive");

    await screen.findByText(/≥42 and counting…/);
    await typeReason(user);

    expect(executeButton("Archive")).toBeDisabled();
    expect(screen.getByTestId("gate-hint")).toHaveTextContent(
      "Preview is still counting — acknowledge proceeding on a partial count, or wait."
    );

    await user.click(screen.getByRole("checkbox", { name: /Proceed on the partial count/ }));
    expect(executeButton("Archive")).toBeEnabled();

    await user.click(executeButton("Archive"));
    expect(api.executeJob).toHaveBeenCalledWith("jb_1", {
      proceed_on_partial: true,
      throttle: 200,
      reason: "flatten gw-timeout storm",
    });
  });

  it("warns that an oversized archive discards the oldest tasks", async () => {
    live = makeJob({
      verb: "archive",
      state: "preview_ready",
      preview_complete: true,
      counts: c({ candidates: 30000, scanned: 30000 }),
    });
    renderModal("archive");

    expect(
      await screen.findByText(
        /Archiving 30,000 tasks into a 10k-capped set will discard the oldest 20,000\./
      )
    ).toBeInTheDocument();
  });
});

describe("BulkJobModal cost disclosure", () => {
  it("discloses the LIST-removal cost with an estimate and the alternatives", async () => {
    live = makeJob({
      cost_class: "list_removal",
      cost_list_len: 2_000_000,
      counts: c({ candidates: 500, scanned: 500 }),
    });
    renderModal("delete");

    const disclosure = (await screen.findByText(/Cost class: LIST removal/)).closest("div")!;
    expect(disclosure).toHaveTextContent(
      /Selective delete from a 2,000,000-entry retry list is LREM/
    );
    expect(disclosure).toHaveTextContent(/~2 min of single-threaded Redis time/);
    expect(disclosure).toHaveTextContent(/Pause the queue first/);
    expect(disclosure).toHaveTextContent(/Whole-state operation/);
    expect(disclosure).toHaveTextContent(/Archive-by-signature later/);
  });

  it("shows no cost disclosure for a cheap scope", async () => {
    renderModal("delete");

    await screen.findByText(/≥42 and counting…/);
    expect(screen.queryByText(/Cost class: LIST removal/)).toBeNull();
  });
});

describe("BulkJobModal execute handoff and live progress", () => {
  async function handOff(user: ReturnType<typeof userEvent.setup>) {
    live = complete;
    const r = renderModal("delete");
    await screen.findByText("57 tasks — exact");
    await typeReason(user);
    await user.click(executeButton());
    return r;
  }

  it("reports the in-flight execute request and locks the controls", async () => {
    let release: (v: unknown) => void = () => {};
    vi.mocked(api.executeJob).mockImplementation(
      () => new Promise((res) => (release = res)) as Promise<never>
    );
    live = complete;
    const user = userEvent.setup();
    renderModal("delete");

    await screen.findByText("57 tasks — exact");
    await typeReason(user);
    await user.click(executeButton());

    expect(screen.getByRole("button", { name: "Starting…" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Cancel" })).toBeDisabled();

    await act(async () => {
      release(makeJob({ phase: "execute", state: "running" }));
    });
  });

  it("swaps the preview meter for the live execute meter and keeps the modal open", async () => {
    const user = userEvent.setup();
    const { feed } = await handOff(user);

    expect(await screen.findByTestId("execute-meter")).toBeInTheDocument();
    expect(screen.queryByTestId("preview-meter")).toBeNull();
    // The execute button is gone; the remaining action closes the modal.
    expect(screen.queryByRole("button", { name: /as background job/ })).toBeNull();
    expect(screen.getAllByRole("button", { name: "Close" }).length).toBeGreaterThan(0);
    // The pre-flight controls are gone once the job belongs to the runner.
    expect(screen.queryByRole("textbox", { name: /Reason/ })).toBeNull();
    expect(screen.queryByRole("combobox", { name: "Throttle" })).toBeNull();
    expect(toast.success).toHaveBeenCalledWith(
      "Bulk delete handed to the job runner",
      expect.anything()
    );

    feed(
      makeJob({
        phase: "execute",
        state: "running",
        preview_complete: true,
        throttle: 200,
        counts: c({ candidates: 10000, acted: 1000 }),
      })
    );
    expect(await screen.findByText("1,000 of 10,000 · ETA ~45s")).toBeInTheDocument();
    expect(screen.getByText("polling")).toBeInTheDocument();
  });

  it("labels the live stream when the feed is an SSE source", async () => {
    const user = userEvent.setup();
    const { feed } = await handOff(user);
    liveSource = "sse";

    feed(
      makeJob({
        phase: "execute",
        state: "running",
        preview_complete: true,
        counts: c({ candidates: 100, acted: 10 }),
      })
    );
    expect(await screen.findByText("live stream")).toBeInTheDocument();
  });

  it("flags a paused execute run", async () => {
    const user = userEvent.setup();
    const { feed } = await handOff(user);

    feed(
      makeJob({
        phase: "execute",
        state: "paused",
        preview_complete: true,
        counts: c({ candidates: 100, acted: 10 }),
      })
    );
    expect(await screen.findByText(/10 of 100 · paused/)).toBeInTheDocument();
  });

  it("states the final tally when the job finishes with failures", async () => {
    const user = userEvent.setup();
    const { feed } = await handOff(user);

    feed(
      makeJob({
        phase: "execute",
        state: "done",
        preview_complete: true,
        finished_at: "2026-07-27T00:10:00Z",
        counts: c({ candidates: 100, acted: 90, skipped: 5, failed: 5 }),
      })
    );
    expect(
      await screen.findByText("done — 90 acted, 5 skipped, 5 failed")
    ).toBeInTheDocument();
    // A terminal job has no transport label — nothing more is coming.
    expect(screen.queryByText("polling")).toBeNull();
    expect(screen.queryByText("live stream")).toBeNull();
  });

  it("states a failed job with what it managed to do", async () => {
    const user = userEvent.setup();
    const { feed } = await handOff(user);

    feed(
      makeJob({
        phase: "execute",
        state: "failed",
        preview_complete: true,
        error: "redis: connection refused",
        counts: c({ candidates: 100, acted: 12, skipped: 0, failed: 3 }),
      })
    );
    expect(
      await screen.findByText("failed — 12 acted, 0 skipped, 3 failed")
    ).toBeInTheDocument();
  });

  it("keeps the modal on the confirm screen when the execute request is refused", async () => {
    vi.mocked(api.executeJob).mockRejectedValue({
      response: { data: { error: "preview not complete" } },
    });
    const user = userEvent.setup();
    live = complete;
    renderModal("delete");

    await screen.findByText("57 tasks — exact");
    await typeReason(user);
    await user.click(executeButton());

    expect(await screen.findByText("preview not complete")).toBeInTheDocument();
    expect(executeButton()).toBeInTheDocument();
    expect(screen.getByTestId("preview-meter")).toBeInTheDocument();
  });

  it("announces the execute outcome once the runner settles the job", async () => {
    const user = userEvent.setup();
    await handOff(user);
    vi.mocked(toast.success).mockClear();

    const onSettled = mockUseJobProgress.mock.calls.at(-1)?.[1]?.onSettled;
    expect(onSettled).toBeTypeOf("function");

    // A canceled orphan preview is not news.
    act(() => onSettled!(makeJob({ phase: "preview", state: "canceled" })));
    expect(toast.success).not.toHaveBeenCalled();
    expect(toast.error).not.toHaveBeenCalled();

    act(() =>
      onSettled!(
        makeJob({
          phase: "execute",
          state: "done",
          counts: c({ candidates: 100, acted: 90, skipped: 5, failed: 5 }),
        })
      )
    );
    expect(toast.success).toHaveBeenCalledWith(
      "Bulk delete complete — 90 acted, 5 skipped, 5 failed"
    );

    act(() => onSettled!(makeJob({ phase: "execute", state: "failed", error: "boom" })));
    expect(toast.error).toHaveBeenCalledWith("Bulk delete failed — boom");
  });
});

describe("BulkJobModal closing", () => {
  it("cancels the orphan preview job when the operator backs out", async () => {
    const user = userEvent.setup();
    const { onClose } = renderModal("delete");

    await screen.findByText(/≥42 and counting…/);
    await user.click(screen.getByRole("button", { name: "Cancel" }));

    expect(api.cancelJob).toHaveBeenCalledWith("jb_1");
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it("cancels the orphan preview from the dialog's own close control", async () => {
    const user = userEvent.setup();
    const { onClose } = renderModal("delete");

    await screen.findByText(/≥42 and counting…/);
    await user.click(screen.getByRole("button", { name: "Close" }));

    expect(api.cancelJob).toHaveBeenCalledWith("jb_1");
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it("leaves a started job running when the operator closes the modal", async () => {
    const user = userEvent.setup();
    live = complete;
    const { onClose } = renderModal("delete");

    await screen.findByText("57 tasks — exact");
    await typeReason(user);
    await user.click(executeButton());
    await screen.findByTestId("execute-meter");

    // Two "Close" controls now exist: the footer button and the dialog's X.
    await user.click(screen.getAllByRole("button", { name: "Close" })[0]);
    expect(api.cancelJob).not.toHaveBeenCalled();
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it("closes the modal when the operator leaves for Operations", async () => {
    const user = userEvent.setup();
    const { onClose } = renderModal("delete");

    await screen.findByText(/≥42 and counting…/);
    await user.click(screen.getByRole("link", { name: /View job history in Operations/ }));

    expect(api.cancelJob).toHaveBeenCalledWith("jb_1");
    expect(onClose).toHaveBeenCalledTimes(1);
  });
});
