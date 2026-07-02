import { useEffect, useMemo } from "react";
import { useSelector } from "react-redux";
import { useParams } from "react-router-dom";
import { AppState, useAppDispatch } from "../store";
import { listQueuesAsync } from "../actions/queuesActions";
import { useQuery } from "../hooks";
import QueueBreadcrumb from "../components/QueueBreadcrumb";
import QueueInfoBanner from "../components/QueueInfoBanner";
import TasksTableContainer from "../components/TasksTableContainer";

const validStatus = ["active", "pending", "aggregating", "scheduled", "retry", "archived", "completed"];
const defaultStatus = "active";

export default function TasksView() {
  const dispatch = useAppDispatch();
  const { qname } = useParams<{ qname: string }>();
  const queuesData = useSelector((s: AppState) => s.queues.data);
  const queues = useMemo(() => queuesData.map((q) => q.name), [queuesData]);
  const query = useQuery();

  let selected = query.get("status");
  if (!selected || !validStatus.includes(selected)) {
    selected = defaultStatus;
  }

  useEffect(() => {
    dispatch(listQueuesAsync());
  }, [dispatch]);

  return (
    <div className="max-w-7xl mx-auto px-4 py-6 space-y-4">
      <QueueBreadcrumb queues={queues} queueName={qname} />
      <div className="border-b border-[hsl(var(--border))] pb-4">
        <QueueInfoBanner qname={qname!} />
      </div>
      <TasksTableContainer queue={qname!} selected={selected} />
    </div>
  );
}
