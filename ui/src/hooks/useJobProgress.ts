// useJobProgress — live tracker for ONE bulk job (the /tasks scan meter, the
// bulk-job modal). Primary transport is the `jobs` SSE event (useJobsEvents);
// whenever the stream is not delivering this job, the hook falls back to
// polling GET /api/jobs/:id every 2 seconds — the UI must work on polling
// alone (same contract as useFleetEvents).
//
// "Settled" is caller-defined (default: terminal state) because a preview
// job's interesting end is the preview_ready park, which is not terminal.
// onSettled fires EXACTLY ONCE, and only when the settlement is observed
// live: attaching to an already-settled job (page reload after the fact)
// renders the settled state but does not re-fire the notification.

import { useEffect, useRef, useState } from "react";
import * as api from "../api";
import { JobInfo } from "../api";
import { isTerminalJobState } from "../lib/bulkjob";
import { useJobsEvents } from "./useJobsEvents";

export interface UseJobProgressOptions {
  // Polling stops and onSettled fires once this predicate holds. Default:
  // the job reached a terminal state.
  settled?: (job: JobInfo) => boolean;
  onSettled?: (job: JobInfo) => void;
}

export interface JobProgressSnapshot {
  job: JobInfo | null;
  source: "sse" | "poll";
  error: string;
}

const FALLBACK_POLL_MS = 2_000;

const defaultSettled = (job: JobInfo) => isTerminalJobState(job.state);

export function useJobProgress(
  jobId: string | null,
  options?: UseJobProgressOptions
): JobProgressSnapshot {
  const { jobs: sseJobs, source: streamSource, lastEventAt } = useJobsEvents(
    jobId !== null
  );
  const [job, setJob] = useState<JobInfo | null>(null);
  const [error, setError] = useState("");

  // Latest callbacks in refs so effect deps stay minimal (inline closures
  // must not restart transports every render).
  const settledRef = useRef<(j: JobInfo) => boolean>(defaultSettled);
  settledRef.current = options?.settled ?? defaultSettled;
  const onSettledRef = useRef<((j: JobInfo) => void) | undefined>(undefined);
  onSettledRef.current = options?.onSettled;
  // null until the first observation; then "fired" | "pending".
  const notifyState = useRef<"pending" | "fired" | null>(null);

  // Reset per tracked job.
  useEffect(() => {
    setJob(null);
    setError("");
    notifyState.current = null;
  }, [jobId]);

  // Observation sink shared by both transports: keeps the freshest state
  // (never regressing a settled job), and fires onSettled once on a
  // settlement observed live.
  const observe = (incoming: JobInfo) => {
    setJob((prev) => {
      if (
        prev &&
        isTerminalJobState(prev.state) &&
        !isTerminalJobState(incoming.state)
      ) {
        return prev; // stale frame racing the terminal event
      }
      return incoming;
    });
    setError("");
    const isSettled = settledRef.current(incoming);
    if (notifyState.current === null) {
      // First observation. Already settled → attach silently (reload of a
      // finished job must not re-toast / re-run queries).
      notifyState.current = isSettled ? "fired" : "pending";
      return;
    }
    if (notifyState.current === "pending" && isSettled) {
      notifyState.current = "fired";
      onSettledRef.current?.(incoming);
    }
  };
  const observeRef = useRef(observe);
  observeRef.current = observe;

  // SSE delivery for this job.
  const sseJob = jobId ? sseJobs[jobId] : undefined;
  useEffect(() => {
    if (sseJob) observeRef.current(sseJob);
  }, [sseJob]);

  // Poll transport: one immediate fetch on attach (fast first paint, and the
  // job may have finished while no page was watching), then a 2s fallback
  // interval that stands down while the stream is delivering this job and
  // stops for good once the job settles.
  const sseCovering = streamSource === "sse" && sseJob !== undefined;
  const settledNow = job !== null && settledRef.current(job);
  useEffect(() => {
    if (!jobId) return;
    let disposed = false;
    const fetchNow = async () => {
      try {
        const detail = await api.getJob(jobId);
        if (!disposed) observeRef.current(detail);
      } catch (e) {
        if (!disposed) {
          setError(e instanceof Error ? e.message : String(e));
        }
      }
    };
    fetchNow();
    let id: ReturnType<typeof setInterval> | null = null;
    if (!settledNow && !sseCovering) {
      id = setInterval(fetchNow, FALLBACK_POLL_MS);
    }
    return () => {
      disposed = true;
      if (id !== null) clearInterval(id);
    };
  }, [jobId, sseCovering, settledNow]);

  return {
    job,
    source: sseCovering ? "sse" : "poll",
    error,
  };
}
