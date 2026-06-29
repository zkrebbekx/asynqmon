import { useSelector, useDispatch } from "react-redux";
import { AppState } from "../store";
import { listServersAsync } from "../actions/serversActions";
import { usePolling } from "../hooks";
import { Alert, AlertDescription, AlertTitle } from "../components/ui/alert";
import { AlertCircle } from "lucide-react";
import ServersTable from "../components/ServersTable";

export default function ServersView() {
  const dispatch = useDispatch();
  const { loading, error, data: servers, } = useSelector((s: AppState) => s.servers);
  const pollInterval = useSelector((s: AppState) => s.settings.pollInterval);

  usePolling(() => dispatch(listServersAsync() as any), pollInterval);

  return (
    <div className="max-w-7xl mx-auto px-4 py-8">
      {error ? (
        <Alert variant="destructive">
          <AlertCircle className="h-4 w-4" />
          <AlertTitle>Error</AlertTitle>
          <AlertDescription>Could not retrieve servers live data — see the logs for details.</AlertDescription>
        </Alert>
      ) : (
        <div className="rounded-lg border border-[hsl(var(--border))] bg-[hsl(var(--card))]">
          <div className="px-6 py-4 border-b border-[hsl(var(--border))]">
            <h2 className="text-base font-semibold">Servers</h2>
          </div>
          <ServersTable servers={servers} />
        </div>
      )}
    </div>
  );
}
