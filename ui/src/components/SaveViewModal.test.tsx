// Saved views are a shared team asset, so the modal checks the name against
// the existing views before it posts, and it reports the server 409 in
// place instead of showing a raw error (review #55.2).

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import SaveViewModal from "./SaveViewModal";
import * as apiViews from "../api-views";
import type { SavedView } from "../api-views";

vi.mock("../api-views", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../api-views")>();
  return { ...actual, listViews: vi.fn(), createView: vi.fn() };
});
vi.mock("sonner", () => ({ toast: Object.assign(vi.fn(), { success: vi.fn(), error: vi.fn() }) }));

const view = (name: string): SavedView => ({
  id: "vw_1",
  name,
  target: "tasks",
  state: { q: "state=retry" },
  created_by: "mira@ops",
  created_at: "2026-08-20T00:00:00Z",
  updated_at: "2026-08-20T00:00:00Z",
  version: 1,
  system: false,
});

beforeEach(() => vi.clearAllMocks());

describe("SaveViewModal", () => {
  it("refuses a name another view already uses, ignoring case, and does not POST", async () => {
    vi.mocked(apiViews.listViews).mockResolvedValue({ views: [view("gw-timeout blast")] });
    const user = userEvent.setup();
    render(<SaveViewModal open target="tasks" state={{ q: "state=retry" }} onClose={() => {}} />);

    await user.type(screen.getByPlaceholderText(/Name —/), "  GW-Timeout Blast  ");
    await user.click(screen.getByRole("button", { name: /Save view/ }));

    expect(await screen.findByText(/already exists/)).toBeInTheDocument();
    expect(apiViews.createView).not.toHaveBeenCalled();
  });

  it("posts a free name and closes", async () => {
    vi.mocked(apiViews.listViews).mockResolvedValue({ views: [view("other")] });
    vi.mocked(apiViews.createView).mockResolvedValue(view("fresh name"));
    const onClose = vi.fn();
    const user = userEvent.setup();
    render(<SaveViewModal open target="tasks" state={{ q: "state=retry" }} onClose={onClose} />);

    await user.type(screen.getByPlaceholderText(/Name —/), "fresh name");
    await user.click(screen.getByRole("button", { name: /Save view/ }));

    await waitFor(() => expect(onClose).toHaveBeenCalled());
    expect(apiViews.createView).toHaveBeenCalledWith({
      name: "fresh name",
      target: "tasks",
      state: { q: "state=retry" },
    });
  });

  it("reports the server duplicate-name 409 in the modal", async () => {
    vi.mocked(apiViews.listViews).mockResolvedValue({ views: [] }); // raced
    vi.mocked(apiViews.createView).mockRejectedValue({
      response: { status: 409, data: { error: "a view named x already exists — pick another name" } },
    });
    const user = userEvent.setup();
    render(<SaveViewModal open target="tasks" state={{}} onClose={() => {}} />);

    await user.type(screen.getByPlaceholderText(/Name —/), "x");
    await user.click(screen.getByRole("button", { name: /Save view/ }));

    expect(await screen.findByText(/already exists/)).toBeInTheDocument();
  });
});
