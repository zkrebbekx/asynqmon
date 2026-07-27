// Legacy task-details route (`/queues/:qname/tasks/:taskId`) — the standalone
// details page was superseded by the Tasks-console peek drawer, which shows a
// strict superset of its data (journey graph, flow, observed runs, metadata
// pivots, clone-and-edit) on the fc design system. Old deep links and
// bookmarks keep working: this route redirects (replace, so Back is not
// trapped) to the console scoped to the task's queue with the task peeked.
// The drawer resolves the peek by queue/id independently of the result list,
// so the task renders whatever state it is in.

import { Navigate, useParams } from "react-router-dom";
import { paths } from "../paths";
import { taskDrawerSearch } from "../lib/urlstate";

export default function TaskDetailsRedirect() {
  const { qname, taskId } = useParams<{ qname: string; taskId: string }>();
  if (!qname || !taskId) return <Navigate to={paths().TASKS} replace />;
  return <Navigate to={`${paths().TASKS}${taskDrawerSearch(qname, taskId)}`} replace />;
}
