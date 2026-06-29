import { Link } from "react-router-dom";
import { ChevronRight } from "lucide-react";
import { paths, queueDetailsPath } from "../paths";

interface Props {
  queues: string[];
  queueName?: string;
  taskId?: string;
}

export default function QueueBreadcrumb({ queues, queueName, taskId }: Props) {
  const appPaths = paths();
  return (
    <nav className="flex items-center gap-1 text-sm text-[hsl(var(--muted-foreground))]">
      <Link to={appPaths.HOME} className="hover:text-[hsl(var(--foreground))] transition-colors">
        Queues
      </Link>
      {queueName && (
        <>
          <ChevronRight size={14} />
          {taskId ? (
            <Link
              to={queueDetailsPath(queueName)}
              className="hover:text-[hsl(var(--foreground))] transition-colors"
            >
              {queueName}
            </Link>
          ) : (
            <span className="text-[hsl(var(--foreground))]">{queueName}</span>
          )}
        </>
      )}
      {taskId && (
        <>
          <ChevronRight size={14} />
          <span className="text-[hsl(var(--foreground))] font-mono text-xs">{taskId}</span>
        </>
      )}
    </nav>
  );
}
