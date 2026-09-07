// api-views: the PUT body carries the version the caller read, and the two
// 409 answers are told apart (review #55.2).

import { describe, it, expect, vi, beforeEach } from "vitest";

// api.ts builds one shared client with axios.create at module load, and every
// api module sends through it, so the mock must provide create.
const { httpSpy } = vi.hoisted(() => ({ httpSpy: vi.fn() }));
vi.mock("axios", () => ({
  default: {
    create: vi.fn(() => httpSpy),
    isAxiosError: () => false,
  },
}));

import { isDuplicateNameError, isViewChangedError, updateView } from "./api-views";

beforeEach(() => {
  vi.clearAllMocks();
  window.ROOT_PATH = "";
});

describe("updateView", () => {
  it("sends the version in the PUT body", async () => {
    httpSpy.mockResolvedValue({ data: { id: "vw_1", version: 3 } } as never);
    await updateView("vw_1", 2, { name: "renamed" });
    expect(httpSpy).toHaveBeenCalledWith(
      expect.objectContaining({
        method: "put",
        data: { name: "renamed", version: 2 },
      })
    );
  });
});

describe("409 classification", () => {
  const err = (status: number, msg: string) => ({ response: { status, data: { error: msg } } });

  it("recognises a stale-version conflict", () => {
    expect(isViewChangedError(err(409, "view changed"))).toBe(true);
    expect(isViewChangedError(err(409, "a view named x already exists"))).toBe(false);
    expect(isViewChangedError(err(404, "view not found"))).toBe(false);
    expect(isViewChangedError(new Error("network"))).toBe(false);
  });

  it("recognises a duplicate-name conflict", () => {
    expect(isDuplicateNameError(err(409, "a view named x already exists — pick another name"))).toBe(true);
    expect(isDuplicateNameError(err(409, "view changed"))).toBe(false);
    expect(isDuplicateNameError(undefined)).toBe(false);
  });
});
