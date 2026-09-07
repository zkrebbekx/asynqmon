// Typed fetchers for the saved-view endpoints (build contract §4.2, phase
// 11). Views are server-side team assets in Redis; `state` is the target
// surface's URL-state params exactly as the urlstate module owns them
// (tasks: q/mode/size; queues: f/sort/dir/limit), stored and served
// verbatim. System views are seeded server-side and are undeletable.

import { http, seg } from "./api";

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
  updated_at: string; // RFC3339
  // Optimistic-concurrency token: 1 on create, +1 per accepted update.
  version: number;
  system: boolean;
}

export interface ListViewsResponse {
  views: SavedView[];
}

export async function listViews(): Promise<ListViewsResponse> {
  const resp = await http({ method: "get", url: `${getBaseUrl()}/views` });
  return resp.data;
}

export interface UpsertViewBody {
  name: string;
  target: ViewTarget;
  state: Record<string, string>;
}

// Message shown when the server refuses a write because the view moved
// under the editor (HTTP 409 on PUT).
export const VIEW_CHANGED_MESSAGE =
  "view changed, reload — someone else edited this view; reload the list and re-apply your change";

// isViewChangedError reports whether an error is the 409 answer to a stale
// version. A duplicate name also answers 409, so the body is checked too.
export function isViewChangedError(e: unknown): boolean {
  const err = e as { response?: { status?: number; data?: { error?: string } } };
  return err?.response?.status === 409 && err?.response?.data?.error === "view changed";
}

// isDuplicateNameError reports whether an error is the 409 answer to a name
// another view already uses.
export function isDuplicateNameError(e: unknown): boolean {
  const err = e as { response?: { status?: number; data?: { error?: string } } };
  return err?.response?.status === 409 && (err?.response?.data?.error ?? "").includes("already exists");
}

export async function createView(body: UpsertViewBody): Promise<SavedView> {
  const resp = await http({ method: "post", url: `${getBaseUrl()}/views`, data: body });
  return resp.data;
}

// updateView sends the version the caller read. The server refuses a stale
// version with 409 (see VIEW_CHANGED_MESSAGE) and writes nothing.
export async function updateView(
  id: string,
  version: number,
  body: Partial<UpsertViewBody>
): Promise<SavedView> {
  const resp = await http({
    method: "put",
    url: `${getBaseUrl()}/views/${seg(id)}`,
    data: { ...body, version },
  });
  return resp.data;
}

export async function deleteView(id: string): Promise<void> {
  await http({ method: "delete", url: `${getBaseUrl()}/views/${seg(id)}` });
}
