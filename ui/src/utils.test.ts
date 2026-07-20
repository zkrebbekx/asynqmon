import { describe, it, expect, vi, afterEach } from "vitest";
import { timeAgo, durationBefore, durationSince, timeAgoUnix } from "./utils";

const NOW = Date.parse("2026-07-20T12:00:00Z");

function freezeNow() {
  vi.useFakeTimers();
  vi.setSystemTime(NOW);
}

afterEach(() => {
  vi.useRealTimers();
});

describe("time formatting", () => {
  // The backend sends "-" for active tasks whose worker heartbeat is missing,
  // and "" for zero times. Date.parse returns NaN (it does not throw) for both,
  // which used to render as "NaNhNaNmNaNs".
  const unparsable = ["-", "", "not-a-date", "0001-01-01T00:00:00Z"];

  it.each(unparsable)("timeAgo(%o) renders a placeholder, not NaN", (ts) => {
    expect(timeAgo(ts)).toBe("-");
  });

  it.each(unparsable)("durationBefore(%o) renders a placeholder, not NaN", (ts) => {
    expect(durationBefore(ts)).toBe("-");
  });

  it.each(unparsable)("durationSince(%o) renders a placeholder, not NaN", (ts) => {
    expect(durationSince(ts)).toBe("-");
  });

  it("timeAgoUnix ignores a NaN unixtime", () => {
    expect(timeAgoUnix(NaN)).toBe("");
  });

  it("timeAgo formats an elapsed timestamp", () => {
    freezeNow();
    expect(timeAgo("2026-07-20T11:49:18Z")).toBe("10m42s ago");
  });

  it("durationSince formats elapsed time without the ago suffix", () => {
    freezeNow();
    expect(durationSince("2026-07-20T09:46:56Z")).toBe("2h13m4s");
  });

  it("durationSince floors sub-second elapsed time to 0s", () => {
    freezeNow();
    expect(durationSince("2026-07-20T11:59:59.5Z")).toBe("0s");
  });

  it("durationBefore formats a future timestamp", () => {
    freezeNow();
    expect(durationBefore("2026-07-20T12:19:17Z")).toBe("in 19m17s");
  });
});
