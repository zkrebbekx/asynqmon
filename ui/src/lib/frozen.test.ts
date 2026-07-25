import { describe, it, expect } from "vitest";
import { frozenInit, frozenReduce, jsonEquals, FrozenState } from "./frozen";

type Rows = string[];

const live = (s: FrozenState<Rows>, data: Rows) => frozenReduce(s, { type: "live", data });
const freeze = (s: FrozenState<Rows>, frozen: boolean) => frozenReduce(s, { type: "setFrozen", frozen });
const apply = (s: FrozenState<Rows>) => frozenReduce(s, { type: "apply" });

describe("frozenReduce", () => {
  it("passes live data straight through while unfrozen", () => {
    let s = frozenInit<Rows>(["a"]);
    s = live(s, ["a", "b"]);
    expect(s.data).toEqual(["a", "b"]);
    expect(s.pending).toBe(0);
  });

  it("suspends rendering while frozen — data still fetched, render frozen", () => {
    let s = frozenInit<Rows>(["a"]);
    s = freeze(s, true);
    s = live(s, ["b"]);
    s = live(s, ["c"]);
    expect(s.data).toEqual(["a"]); // render frozen
    expect(s.live).toEqual(["c"]); // fetching continued
    expect(s.pending).toBe(2);
  });

  it("does not count identical refetches as updates", () => {
    let s = frozenInit<Rows>(["a"]);
    s = freeze(s, true);
    s = live(s, ["a"]); // fresh array, same content
    expect(s.pending).toBe(0);
    s = live(s, ["b"]);
    expect(s.pending).toBe(1);
  });

  it("does not re-count a change on subsequent identical polls", () => {
    let s = frozenInit<Rows>(["a"]);
    s = freeze(s, true);
    s = live(s, ["b"]); // one real change…
    s = live(s, ["b"]); // …then steady-state polls
    s = live(s, ["b"]);
    expect(s.pending).toBe(1);
  });

  it("counts each transition, including a revert to the rendered value", () => {
    let s = frozenInit<Rows>(["a"]);
    s = freeze(s, true);
    s = live(s, ["b"]);
    s = live(s, ["a"]); // reverted — still two suspended transitions
    expect(s.pending).toBe(2);
    expect(s.live).toEqual(["a"]); // the buffer is current
  });

  it("apply promotes the buffer and resets the pill, keeping the freeze", () => {
    let s = frozenInit<Rows>(["a"]);
    s = freeze(s, true);
    s = live(s, ["b"]);
    s = apply(s);
    expect(s.data).toEqual(["b"]);
    expect(s.pending).toBe(0);
    expect(s.frozen).toBe(true); // drawer may still be open
    s = live(s, ["c"]); // recounts from zero
    expect(s.pending).toBe(1);
  });

  it("unfreezing applies the buffered value immediately", () => {
    let s = frozenInit<Rows>(["a"]);
    s = freeze(s, true);
    s = live(s, ["b"]);
    s = freeze(s, false);
    expect(s.data).toEqual(["b"]);
    expect(s.pending).toBe(0);
    expect(s.frozen).toBe(false);
  });

  it("re-freezing without changes is a no-op on data and pill", () => {
    let s = frozenInit<Rows>(["a"]);
    s = freeze(s, true);
    s = freeze(s, true);
    expect(s.data).toEqual(["a"]);
    expect(s.pending).toBe(0);
  });
});

describe("jsonEquals", () => {
  it("is structural, not referential", () => {
    expect(jsonEquals([{ id: 1 }], [{ id: 1 }])).toBe(true);
    expect(jsonEquals([{ id: 1 }], [{ id: 2 }])).toBe(false);
  });

  it("treats identical references as equal even when unserializable", () => {
    const cyclic: Record<string, unknown> = {};
    cyclic.self = cyclic;
    expect(jsonEquals(cyclic, cyclic)).toBe(true);
    expect(jsonEquals(cyclic, { self: null })).toBe(false);
  });
});
