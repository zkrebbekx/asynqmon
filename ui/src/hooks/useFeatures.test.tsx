// useCorrelationKeys (§3.5): the Flow view's recognized-key list comes from
// GET /api/features, with the built-in defaults as fallback while loading,
// on probe failure (older backends without the endpoint), and when the
// response omits the field.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { renderHook, waitFor } from "@testing-library/react";
import * as api from "../api";
import { DEFAULT_CORRELATION_KEYS } from "../lib/correlation";
import { resetFeaturesCache, useCorrelationKeys } from "./useFeatures";

vi.mock("../api");

beforeEach(() => {
  vi.mocked(api.getFeatures).mockReset();
  resetFeaturesCache();
});

describe("useCorrelationKeys", () => {
  it("returns the configured list once /api/features answers", async () => {
    vi.mocked(api.getFeatures).mockResolvedValue({
      features: { enqueue: false },
      correlation_keys: ["order_ref", "trace_id"],
    });
    const { result } = renderHook(() => useCorrelationKeys());
    // Defaults apply while loading.
    expect(result.current).toEqual(DEFAULT_CORRELATION_KEYS);
    await waitFor(() => expect(result.current).toEqual(["order_ref", "trace_id"]));
  });

  it("falls back to the defaults when the endpoint is absent (older backend)", async () => {
    vi.mocked(api.getFeatures).mockRejectedValue(new Error("404"));
    const { result } = renderHook(() => useCorrelationKeys());
    await waitFor(() => expect(api.getFeatures).toHaveBeenCalled());
    expect(result.current).toEqual(DEFAULT_CORRELATION_KEYS);
  });

  it("keeps the defaults when the response omits or empties correlation_keys", async () => {
    vi.mocked(api.getFeatures).mockResolvedValue({ features: { enqueue: true } });
    const { result } = renderHook(() => useCorrelationKeys());
    await waitFor(() => expect(api.getFeatures).toHaveBeenCalled());
    expect(result.current).toEqual(DEFAULT_CORRELATION_KEYS);

    resetFeaturesCache();
    vi.mocked(api.getFeatures).mockResolvedValue({
      features: { enqueue: true },
      correlation_keys: [],
    });
    const second = renderHook(() => useCorrelationKeys());
    await waitFor(() => expect(api.getFeatures).toHaveBeenCalledTimes(2));
    expect(second.result.current).toEqual(DEFAULT_CORRELATION_KEYS);
  });
});
