// Typed fetchers for the saved-view endpoints (build contract §4.2, phase
// 11). Views are server-side team assets in Redis; `state` is the target
// surface's URL-state params exactly as the urlstate module owns them
// (tasks: q/mode/size; queues: f/sort/dir/limit), stored and served
// verbatim. System views are seeded server-side and are undeletable.

import axios from "axios";

const getBaseUrl = () =>
  import.meta.env.PROD
    ? `${window.ROOT_PATH}/api`
    : `${window.location.origin}${window.ROOT_PATH}/api`;

export type ViewTarget = "tasks" | "queues";

export interface SavedView {
  id: string;
  name: string;
  target: ViewTarget;
  state: Record<string, string>;
  created_by: string;
  created_at: string; // RFC3339
  system: boolean;
}

export interface ListViewsResponse {
  views: SavedView[];
}

export async function listViews(): Promise<ListViewsResponse> {
  const resp = await axios({ method: "get", url: `${getBaseUrl()}/views` });
  return resp.data;
}

export interface UpsertViewBody {
  name: string;
  target: ViewTarget;
  state: Record<string, string>;
}

export async function createView(body: UpsertViewBody): Promise<SavedView> {
  const resp = await axios({ method: "post", url: `${getBaseUrl()}/views`, data: body });
  return resp.data;
}

export async function updateView(
  id: string,
  body: Partial<UpsertViewBody>
): Promise<SavedView> {
  const resp = await axios({ method: "put", url: `${getBaseUrl()}/views/${id}`, data: body });
  return resp.data;
}

export async function deleteView(id: string): Promise<void> {
  await axios({ method: "delete", url: `${getBaseUrl()}/views/${id}` });
}
