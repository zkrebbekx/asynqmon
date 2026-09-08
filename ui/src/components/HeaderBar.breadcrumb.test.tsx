// The breadcrumb reads the queue name out of the path with matchPath, which
// returns the RAW segment. Queue names routinely carry a colon
// ("email:send"), which the path builders percent-encode, so the crumb used
// to read "email%3Asend" and its link encoded a second time.

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { Provider } from "react-redux";
import { configureStore } from "@reduxjs/toolkit";
import HeaderBar from "./HeaderBar";
import settingsReducer from "../reducers/settingsReducer";

function renderAt(path: string) {
  const store = configureStore({
    reducer: { settings: settingsReducer },
  });
  return render(
    <Provider store={store}>
      <MemoryRouter initialEntries={[path]}>
        <HeaderBar />
      </MemoryRouter>
    </Provider>
  );
}

describe("breadcrumb path decoding", () => {
  beforeEach(() => {
    window.ROOT_PATH = "";
    vi.clearAllMocks();
  });

  it("shows a colon in a queue name instead of its escape", () => {
    renderAt("/queues/email%3Asend");
    const crumbs = screen.getByLabelText("Breadcrumb");
    expect(crumbs.textContent).toContain("email:send");
    expect(crumbs.textContent).not.toContain("%3A");
  });

  it("shows a slash and a percent sign decoded too", () => {
    renderAt("/queues/a%2Fb%20c%25d");
    const crumbs = screen.getByLabelText("Breadcrumb");
    expect(crumbs.textContent).toContain("a/b c%d");
  });

  it("links the queue crumb of a task path without encoding twice", () => {
    renderAt("/queues/email%3Asend/tasks/abc123");
    const link = screen.getByRole("link", { name: "email:send" });
    // Encoded exactly once: %3A, never %253A.
    expect(link.getAttribute("href")).toBe("/queues/email%3Asend");
  });

  it("falls back to the raw segment when the escape is malformed", () => {
    renderAt("/queues/bad%ZZname");
    const crumbs = screen.getByLabelText("Breadcrumb");
    expect(crumbs.textContent).toContain("bad%ZZname");
  });

  it("leaves a name that needs no escaping alone", () => {
    renderAt("/queues/plain");
    const crumbs = screen.getByLabelText("Breadcrumb");
    expect(crumbs.textContent).toContain("plain");
  });
});
