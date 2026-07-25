// Typed fetchers for the §5.8 ring-buffer series read API and the §5.14
// deploy markers (phase 10). Points are period-aligned; v === null means
// NO-SAMPLE — a slot that was never flushed. Renderers must gap, never zero.

import axios from "axios";

const getBaseUrl = () =>
  import.meta.env.PROD
    ? `${window.ROOT_PATH}/api`
    : `${window.location.origin}${window.ROOT_PATH}/api`;

/**************************************************************
                      GET /api/series
 **************************************************************/

export interface SeriesPoint {
  t: number; // period start, unix seconds
  v: number | null; // null = no-sample (never zero)
}

export type SeriesWindow = "3h" | "24h" | "30d";

export interface SeriesResponse {
  scope: string; // "fleet" | "queue:<name>" | "server:<id>"
  metric: string;
  window: SeriesWindow;
  period: number; // seconds per point
  points: SeriesPoint[];
  // RFC3339; present while series sampling has existed <24h — ring-derived
  // baselines are still learning.
  learning_until?: string;
}

export interface SeriesSpec {
  scope: string;
  metric: string;
  window: SeriesWindow;
  points?: number; // last N points only
}

export async function getSeries(spec: SeriesSpec): Promise<SeriesResponse> {
  const usp = new URLSearchParams({
    scope: spec.scope,
    metric: spec.metric,
    window: spec.window,
  });
  if (spec.points) usp.set("points", String(spec.points));
  const resp = await axios({ method: "get", url: `${getBaseUrl()}/series?${usp}` });
  return resp.data;
}

/**************************************************************
                  GET /api/series/batch  (cap 60)
 **************************************************************/

export interface SeriesBatchResponse {
  series: SeriesResponse[]; // mirrors the specs order
}

export async function getSeriesBatch(specs: SeriesSpec[]): Promise<SeriesBatchResponse> {
  const usp = new URLSearchParams({ specs: JSON.stringify(specs) });
  const resp = await axios({ method: "get", url: `${getBaseUrl()}/series/batch?${usp}` });
  return resp.data;
}

/**************************************************************
                    Deploy markers (§5.14)
 **************************************************************/

export interface DeployMarker {
  id: string;
  label: string;
  actor: string;
  at: string; // RFC3339
}

export interface ListMarkersResponse {
  markers: DeployMarker[]; // ascending by time
}

export async function listMarkers(window: SeriesWindow = "24h"): Promise<ListMarkersResponse> {
  const resp = await axios({
    method: "get",
    url: `${getBaseUrl()}/markers?window=${window}`,
  });
  return resp.data;
}

export async function createMarker(label: string): Promise<DeployMarker> {
  const resp = await axios({
    method: "post",
    url: `${getBaseUrl()}/markers`,
    data: { label },
  });
  return resp.data;
}
