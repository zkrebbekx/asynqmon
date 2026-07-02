import { useSelector } from "react-redux";
import { AppState, useAppDispatch } from "../store";
import { listSchedulerEntriesAsync } from "../actions/schedulerEntriesActions";
import { usePolling } from "../hooks";
import { Alert, AlertDescription, AlertTitle } from "../components/ui/alert";
import { AlertCircle } from "lucide-react";
import SchedulerEntriesTable from "../components/SchedulerEntriesTable";

export default function SchedulersView() {
  const dispatch = useAppDispatch();
  const { error, data: entries } = useSelector((s: AppState) => s.schedulerEntries);
  const pollInterval = useSelector((s: AppState) => s.settings.pollInterval);

  usePolling(() => dispatch(listSchedulerEntriesAsync()), pollInterval);

  return (
    <div className="max-w-7xl mx-auto px-4 py-8">
      {error ? (
        <Alert variant="destructive">
          <AlertCircle className="h-4 w-4" />
          <AlertTitle>Error</AlertTitle>
          <AlertDescription>Could not retrieve scheduler data — see the logs for details.</AlertDescription>
        </Alert>
      ) : (
        <div className="rounded-lg border border-[hsl(var(--border))] bg-[hsl(var(--card))]">
          <div className="px-6 py-4 border-b border-[hsl(var(--border))]">
            <h2 className="text-base font-semibold">Scheduler Entries</h2>
          </div>
          <SchedulerEntriesTable entries={entries} />
        </div>
      )}
    </div>
  );
}
