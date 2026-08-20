import { describe, expect, it } from "vitest";
import { renderHook } from "@testing-library/react";
import { useLatestOnly } from "./index";

describe("useLatestOnly", () => {
  it("marks the most recent call current and stales older ones", () => {
    const { result } = renderHook(() => useLatestOnly());
    const first = result.current();
    expect(first()).toBe(true);
    const second = result.current();
    expect(first()).toBe(false);
    expect(second()).toBe(true);
  });

  it("keeps a stable function identity across renders", () => {
    const { result, rerender } = renderHook(() => useLatestOnly());
    const before = result.current;
    rerender();
    expect(result.current).toBe(before);
  });
});
