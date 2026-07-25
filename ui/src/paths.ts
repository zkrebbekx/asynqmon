export const paths = () => ({
  HOME: `${window.ROOT_PATH}/`, // Fleet landing
  QUEUES: `${window.ROOT_PATH}/queues`, // Queues Directory
  SETTINGS: `${window.ROOT_PATH}/settings`,
  SERVERS: `${window.ROOT_PATH}/servers`,
  SCHEDULERS: `${window.ROOT_PATH}/schedulers`,
  QUEUE_DETAILS: `${window.ROOT_PATH}/queues/:qname`,
  TASKS: `${window.ROOT_PATH}/tasks`,
  REDIS: `${window.ROOT_PATH}/redis`,
  TASK_DETAILS: `${window.ROOT_PATH}/queues/:qname/tasks/:taskId`,
  QUEUE_METRICS: `${window.ROOT_PATH}/q/metrics`,
});

/**************************************************************
                        Path Helper functions
 **************************************************************/

export function queueDetailsPath(qname: string, taskStatus?: string): string {
  const path = paths().QUEUE_DETAILS.replace(":qname", qname);
  if (taskStatus) {
    return `${path}?status=${taskStatus}`;
  }
  return path;
}

export function taskDetailsPath(qname: string, taskId: string): string {
  return paths()
    .TASK_DETAILS.replace(":qname", qname)
    .replace(":taskId", taskId);
}

// attentionFindingPath resolves an attention finding's suggested query to the
// surface that can answer it (Tasks console or Queues Directory), with the
// query pre-filled in the URL. The pure mapping lives in lib/fleet.ts.
import { attentionTarget } from "./lib/fleet";

export function attentionFindingPath(suggestedQuery: string): string {
  const target = attentionTarget(suggestedQuery);
  const base = target.kind === "tasks" ? paths().TASKS : paths().QUEUES;
  return `${base}${target.search}`;
}

/**************************************************************
                        URL Params
 **************************************************************/

export interface QueueDetailsRouteParams {
  qname: string;
}

export interface TaskDetailsRouteParams {
  qname: string;
  taskId: string;
}
