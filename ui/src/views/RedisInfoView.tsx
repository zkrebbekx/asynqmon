import { useDispatch, useSelector } from "react-redux";
import { AppState } from "../store";
import { getRedisInfoAsync } from "../actions/redisInfoActions";
import { usePolling } from "../hooks";
import { timeAgoUnix } from "../utils";
import { RedisInfo } from "../api";
import { Alert, AlertDescription, AlertTitle } from "../components/ui/alert";
import { AlertCircle } from "lucide-react";
import { Card, CardContent, CardHeader, CardTitle } from "../components/ui/card";
import QueueLocationTable from "../components/QueueLocationTable";
import SyntaxHighlighter from "../components/SyntaxHighlighter";

function MetricCard({ title, value }: { title: string; value: string }) {
  return (
    <Card>
      <CardContent className="pt-6 text-center">
        <p className="text-2xl font-semibold text-[hsl(var(--foreground))]">{value}</p>
        <p className="text-xs text-[hsl(var(--muted-foreground))] mt-1">{title}</p>
      </CardContent>
    </Card>
  );
}

function RedisMetricCards({ redisInfo }: { redisInfo: RedisInfo }) {
  return (
    <div className="space-y-6">
      <div>
        <h3 className="text-sm font-medium text-[hsl(var(--muted-foreground))] mb-3">Server</h3>
        <div className="grid grid-cols-2 gap-4">
          <MetricCard title="Version" value={redisInfo.redis_version} />
          <MetricCard title="Uptime" value={`${redisInfo.uptime_in_days} days`} />
        </div>
      </div>
      <div>
        <h3 className="text-sm font-medium text-[hsl(var(--muted-foreground))] mb-3">Memory</h3>
        <div className="grid grid-cols-3 gap-4">
          <MetricCard title="Used Memory" value={redisInfo.used_memory_human} />
          <MetricCard title="Peak Memory" value={redisInfo.used_memory_peak_human} />
          <MetricCard title="Fragmentation Ratio" value={redisInfo.mem_fragmentation_ratio} />
        </div>
      </div>
      <div>
        <h3 className="text-sm font-medium text-[hsl(var(--muted-foreground))] mb-3">Connections</h3>
        <div className="grid grid-cols-2 gap-4">
          <MetricCard title="Connected Clients" value={redisInfo.connected_clients} />
          <MetricCard title="Connected Replicas" value={redisInfo.connected_slaves} />
        </div>
      </div>
      <div>
        <h3 className="text-sm font-medium text-[hsl(var(--muted-foreground))] mb-3">Persistence</h3>
        <div className="grid grid-cols-2 gap-4">
          <MetricCard title="Last Save to Disk" value={timeAgoUnix(parseInt(redisInfo.rdb_last_save_time))} />
          <MetricCard title="Changes Since Last Dump" value={redisInfo.rdb_changes_since_last_save} />
        </div>
      </div>
    </div>
  );
}

export default function RedisInfoView() {
  const dispatch = useDispatch();
  const { loading, error, data: redisInfo, address: redisAddress, rawData: redisInfoRaw,
    cluster: redisClusterEnabled, rawClusterNodes: redisClusterNodesRaw, queueLocations } =
    useSelector((s: AppState) => s.redis);
  const pollInterval = useSelector((s: AppState) => s.settings.pollInterval);

  usePolling(() => dispatch(getRedisInfoAsync() as any), pollInterval);

  return (
    <div className="max-w-5xl mx-auto px-4 py-8 space-y-6">
      {error ? (
        <Alert variant="destructive">
          <AlertCircle className="h-4 w-4" />
          <AlertTitle>Error</AlertTitle>
          <AlertDescription>Could not retrieve Redis data — see the logs for details.</AlertDescription>
        </Alert>
      ) : (
        <>
          <div>
            <h1 className="text-2xl font-semibold">
              {redisClusterEnabled ? "Redis Cluster Info" : "Redis Info"}
            </h1>
            {!redisClusterEnabled && redisAddress && (
              <p className="text-sm text-[hsl(var(--muted-foreground))] mt-1">Connected to: {redisAddress}</p>
            )}
          </div>

          {queueLocations && queueLocations.length > 0 && (
            <div>
              <h2 className="text-sm font-medium text-[hsl(var(--muted-foreground))] mb-3">Queue Locations in Cluster</h2>
              <Card>
                <QueueLocationTable queueLocations={queueLocations} />
              </Card>
            </div>
          )}

          {redisClusterNodesRaw && (
            <div>
              <h2 className="text-sm font-medium text-[hsl(var(--muted-foreground))] mb-3">
                <a href="https://redis.io/commands/cluster-nodes" target="_blank" rel="noreferrer" className="hover:underline">CLUSTER NODES</a> Output
              </h2>
              <SyntaxHighlighter language="yaml">{redisClusterNodesRaw}</SyntaxHighlighter>
            </div>
          )}

          {redisInfo && !redisClusterEnabled && <RedisMetricCards redisInfo={redisInfo} />}

          {redisInfoRaw && (
            <div>
              <h2 className="text-sm font-medium text-[hsl(var(--muted-foreground))] mb-3">
                <a href="https://redis.io/commands/info" target="_blank" rel="noreferrer" className="hover:underline">
                  {redisClusterEnabled ? "CLUSTER INFO" : "INFO"}
                </a> Output
              </h2>
              <SyntaxHighlighter language="yaml">{redisInfoRaw}</SyntaxHighlighter>
            </div>
          )}
        </>
      )}
    </div>
  );
}
