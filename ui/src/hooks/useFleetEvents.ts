// useFleetEvents — one live-aggregate feed per tab (build contract §4.4).
//
// Primary transport is the SSE stream GET /api/fleet/events (named events
// `overview` and `attention`, payloads identical to the GET endpoints). The
// stream may 404/503 until the phase-4 backend lands, or drop mid-incident —
// every error path falls back to polling the plain GET endpoints on the
// user's poll cadence, and the stream is retried with exponential backoff.
// The UI must render fine on polling alone.

import { useEffect, useState } from "react";
import { useDispatch, useSelector } from "react-redux";
import axios from "axios";
import { AppState } from "../store";
import { pollTick } from "../actions/settingsActions";
import {
  FleetOverviewResponse,
  FleetAttentionResponse,
  getFleetOverview,
  getFleetAttention,
  fleetEventsUrl,
} from "../api-fleet";

export interface FleetEventsSnapshot {
  overview: FleetOverviewResponse | null;
  attention: FleetAttentionResponse | null;
  source: "sse" | "poll";
  // Epoch ms of the last successful data receipt (either transport); 0 until
  // the first one. Drives the stale-data banner.
  updatedAt: number;
  // Last transport error while no data is flowing ("" once data arrives).
  error: string;
}

const INITIAL_BACKOFF_MS = 5_000;
const MAX_BACKOFF_MS = 60_000;

function errMessage(reason: unknown): string {
  if (axios.isAxiosError(reason)) {
    return reason.response
      ? `${reason.response.status} ${reason.response.statusText}`
      : reason.message;
  }
  return reason instanceof Error ? reason.message : String(reason);
}

export function useFleetEvents(): FleetEventsSnapshot {
  const dispatch = useDispatch();
  const pollingActive = useSelector((s: AppState) => s.settings.pollingActive);
  const pollInterval = useSelector((s: AppState) => s.settings.pollInterval);

  const [overview, setOverview] = useState<FleetOverviewResponse | null>(null);
  const [attention, setAttention] = useState<FleetAttentionResponse | null>(null);
  const [source, setSource] = useState<"sse" | "poll">("poll");
  const [updatedAt, setUpdatedAt] = useState(0);
  const [error, setError] = useState("");

  useEffect(() => {
    let disposed = false;
    let es: EventSource | null = null;
    let reconnectTimer: ReturnType<typeof setTimeout> | null = null;
    let pollTimer: ReturnType<typeof setInterval> | null = null;
    let backoff = INITIAL_BACKOFF_MS;
    let sseHealthy = false;

    // Successful data receipt: stamp freshness locally and in settings so the
    // chrome's "updated Ns ago" pill stays truthful on either transport.
    const touch = () => {
      setUpdatedAt(Date.now());
      setError("");
      dispatch(pollTick());
    };

    const fetchNow = async () => {
      const [ov, att] = await Promise.allSettled([
        getFleetOverview(),
        getFleetAttention(),
      ]);
      if (disposed) return;
      let ok = false;
      if (ov.status === "fulfilled") {
        setOverview(ov.value);
        ok = true;
      }
      if (att.status === "fulfilled") {
        setAttention(att.value);
        ok = true;
      }
      if (ok) {
        touch();
      } else if (ov.status === "rejected") {
        setError(errMessage(ov.reason));
      }
    };

    const onEvent = (apply: (raw: string) => void) => (e: MessageEvent) => {
      if (disposed) return;
      sseHealthy = true;
      backoff = INITIAL_BACKOFF_MS;
      setSource("sse");
      try {
        apply(e.data);
      } catch {
        return; // malformed frame — keep the last good snapshot
      }
      touch();
    };

    const openStream = () => {
      if (disposed || typeof EventSource === "undefined") return;
      es = new EventSource(fleetEventsUrl());
      es.addEventListener(
        "overview",
        onEvent((raw) => setOverview(JSON.parse(raw)))
      );
      es.addEventListener(
        "attention",
        onEvent((raw) => setAttention(JSON.parse(raw)))
      );
      es.onerror = () => {
        // 404/503 (backend not landed), proxy timeout, or dropped connection.
        // Close, fall back to the poll loop, and retry with backoff. Errors
        // can only recur once per (re)connect attempt, so backoff paces them.
        es?.close();
        es = null;
        sseHealthy = false;
        if (disposed) return;
        setSource("poll");
        reconnectTimer = setTimeout(openStream, backoff);
        backoff = Math.min(backoff * 2, MAX_BACKOFF_MS);
      };
    };

    // Always fetch once immediately: fast first paint, and coverage for the
    // window before the stream connects (or forever, if it never does).
    fetchNow();

    if (pollingActive) {
      openStream();
      pollTimer = setInterval(() => {
        // Poll only while SSE isn't delivering; skip hidden tabs.
        if (document.hidden || sseHealthy) return;
        fetchNow();
      }, Math.max(1, pollInterval) * 1000);
    }
    // pollingActive === false: the live pill is paused — one snapshot, no
    // stream, no interval (same contract as usePolling).

    return () => {
      disposed = true;
      if (es !== null) es.close();
      if (reconnectTimer !== null) clearTimeout(reconnectTimer);
      if (pollTimer !== null) clearInterval(pollTimer);
    };
  }, [dispatch, pollingActive, pollInterval]);

  return { overview, attention, source, updatedAt, error };
}
