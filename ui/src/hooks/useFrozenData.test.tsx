import { describe, it, expect } from "vitest";
import { act, renderHook } from "@testing-library/react";
import { useFrozenData } from "./useFrozenData";

type Rows = string[];

function setup(initial: Rows = ["a"]) {
  return renderHook(
    ({ live, frozen }: { live: Rows; frozen: boolean }) => useFrozenData(live, frozen),
    { initialProps: { live: initial, frozen: false } }
  );
}

describe("useFrozenData", () => {
  it("tracks live data while unfrozen", () => {
    const h = setup(["a"]);
    h.rerender({ live: ["a", "b"], frozen: false });
    expect(h.result.current.data).toEqual(["a", "b"]);
    expect(h.result.current.pending).toBe(0);
  });

  it("freezes rendering while counting suspended changes", () => {
    const h = setup(["a"]);
    h.rerender({ live: ["a"], frozen: true });
    h.rerender({ live: ["b"], frozen: true });
    h.rerender({ live: ["c"], frozen: true });
    expect(h.result.current.data).toEqual(["a"]); // render frozen
    expect(h.result.current.pending).toBe(2); // two real changes buffered
  });

  it("ignores identical refetches while frozen (fresh array, same rows)", () => {
    const h = setup(["a"]);
    h.rerender({ live: ["a"], frozen: true });
    h.rerender({ live: ["a"].slice(), frozen: true });
    expect(h.result.current.pending).toBe(0);
  });

  it("apply (pill click) promotes the buffer and keeps the freeze", () => {
    const h = setup(["a"]);
    h.rerender({ live: ["b"], frozen: true });
    act(() => h.result.current.apply());
    expect(h.result.current.data).toEqual(["b"]);
    expect(h.result.current.pending).toBe(0);
    // Still frozen: the next change buffers again.
    h.rerender({ live: ["c"], frozen: true });
    expect(h.result.current.data).toEqual(["b"]);
    expect(h.result.current.pending).toBe(1);
  });

  it("unfreezing applies the buffered value", () => {
    const h = setup(["a"]);
    h.rerender({ live: ["b"], frozen: true });
    h.rerender({ live: ["b"], frozen: false });
    expect(h.result.current.data).toEqual(["b"]);
    expect(h.result.current.pending).toBe(0);
  });

  it("applyNextChange flushes the next live arrival through the freeze (mutation invalidation)", () => {
    const h = setup(["a"]);
    h.rerender({ live: ["a"], frozen: true });
    act(() => h.result.current.applyNextChange());
    h.rerender({ live: ["after-mutation"], frozen: true });
    expect(h.result.current.data).toEqual(["after-mutation"]);
    expect(h.result.current.pending).toBe(0);
    // One-shot: the following change buffers normally again.
    h.rerender({ live: ["later"], frozen: true });
    expect(h.result.current.data).toEqual(["after-mutation"]);
    expect(h.result.current.pending).toBe(1);
  });

  it("a resetKey change flushes the next arrival through the freeze (deliberate view change)", () => {
    const h = renderHook(
      ({ live, frozen, key }: { live: Rows; frozen: boolean; key: string }) =>
        useFrozenData(live, frozen, undefined, key),
      { initialProps: { live: ["a"] as Rows, frozen: true, key: "f1" } }
    );
    // Steady freeze, then the operator changes the filter: reset + fetch.
    h.rerender({ live: [], frozen: true, key: "f2" }); // reset clears to the (empty) live
    expect(h.result.current.pending).toBe(0);
    h.rerender({ live: ["fresh"], frozen: true, key: "f2" }); // response applies through
    expect(h.result.current.data).toEqual(["fresh"]);
    expect(h.result.current.pending).toBe(0);
    // Later polls buffer normally again.
    h.rerender({ live: ["poll2"], frozen: true, key: "f2" });
    expect(h.result.current.data).toEqual(["fresh"]);
    expect(h.result.current.pending).toBe(1);
  });

  it("a resetKey change applies the fresh data even when live arrives later (sort under hover)", () => {
    const h = renderHook(
      ({ live, frozen, key }: { live: Rows; frozen: boolean; key: string }) =>
        useFrozenData(live, frozen, undefined, key),
      { initialProps: { live: ["old-sort"] as Rows, frozen: true, key: "sort:a" } }
    );
    h.rerender({ live: ["old-sort"], frozen: true, key: "sort:b" }); // click sort header
    expect(h.result.current.data).toEqual(["old-sort"]); // old page holds while loading
    h.rerender({ live: ["new-sort"], frozen: true, key: "sort:b" });
    expect(h.result.current.data).toEqual(["new-sort"]);
    expect(h.result.current.pending).toBe(0);
  });

  it("supports a custom equality function", () => {
    const byLength = (a: Rows, b: Rows) => a.length === b.length;
    const h = renderHook(
      ({ live, frozen }: { live: Rows; frozen: boolean }) =>
        useFrozenData(live, frozen, byLength),
      { initialProps: { live: ["a"] as Rows, frozen: false } }
    );
    h.rerender({ live: ["a"], frozen: true });
    h.rerender({ live: ["x"], frozen: true }); // same length — not a change
    expect(h.result.current.pending).toBe(0);
    h.rerender({ live: ["x", "y"], frozen: true });
    expect(h.result.current.pending).toBe(1);
  });
});
