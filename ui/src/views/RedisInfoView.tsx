import { useSelector } from "react-redux";
import { AppState, useAppDispatch } from "../store";
import { getRedisInfoAsync } from "../actions/redisInfoActions";
import { usePolling } from "../hooks";
import { timeAgoUnix } from "../utils";
import { RedisInfo } from "../api";
import { MicroLabel } from "../components/FleetBits";
import PageShell, { panelClass } from "../components/PageShell";
import QueueLocationTable from "../components/QueueLocationTable";
import SyntaxHighlighter from "../components/SyntaxHighlighter";

// Metric tile matching the Fleet KPI tile: caps eyebrow + left-aligned mono
// value on a fc panel surface.
function MetricTile({ label, value }: { label: string; value: string }) {
  return (
    <div className="rounded-lg border border-[var(--fc-line)] bg-[var(--fc-panel)] px-3 pb-2.5 pt-2.5">
      <MicroLabel>{label}</MicroLabel>
      <div className="mt-[3px] font-mono text-[19px] font-semibold tabular-nums tracking-[-0.01em] text-[var(--fc-ink)]">
        {value}
      </div>
    </div>
  );
}

function RedisMetricTiles({ redisInfo }: { redisInfo: RedisInfo }) {
  return (
    <div className="space-y-3">
      <div>
        <MicroLabel className="mb-1.5">Server</MicroLabel>
        <div className="grid grid-cols-2 gap-2 lg:grid-cols-4">
          <MetricTile label="version" value={redisInfo.redis_version} />
          <MetricTile label="uptime" value={`${redisInfo.uptime_in_days} days`} />
        </div>
      </div>
      <div>
        <MicroLabel className="mb-1.5">Memory</MicroLabel>
        <div className="grid grid-cols-3 gap-2 lg:grid-cols-4">
          <MetricTile label="used memory" value={redisInfo.used_memory_human} />
          <MetricTile label="peak memory" value={redisInfo.used_memory_peak_human} />
          <MetricTile label="fragmentation ratio" value={redisInfo.mem_fragmentation_ratio} />
        </div>
      </div>
      <div>
        <MicroLabel className="mb-1.5">Connections</MicroLabel>
        <div className="grid grid-cols-2 gap-2 lg:grid-cols-4">
          <MetricTile label="connected clients" value={redisInfo.connected_clients} />
          <MetricTile label="connected replicas" value={redisInfo.connected_slaves} />
        </div>
      </div>
      <div>
        <MicroLabel className="mb-1.5">Persistence</MicroLabel>
        <div className="grid grid-cols-2 gap-2 lg:grid-cols-4">
          <MetricTile
            label="last save to disk"
            value={timeAgoUnix(parseInt(redisInfo.rdb_last_save_time))}
          />
          <MetricTile
            label="changes since last dump"
            value={redisInfo.rdb_changes_since_last_save}
          />
        </div>
      </div>
    </div>
  );
}

export default function RedisInfoView() {
  const dispatch = useAppDispatch();
  const { error, data: redisInfo, address: redisAddress, rawData: redisInfoRaw,
    cluster: redisClusterEnabled, rawClusterNodes: redisClusterNodesRaw, queueLocations } =
    useSelector((s: AppState) => s.redis);
  const pollInterval = useSelector((s: AppState) => s.settings.pollInterval);

  usePolling(() => dispatch(getRedisInfoAsync()), pollInterval);

  if (error) {
    return (
      <PageShell>
        <div className="rounded-md border border-[var(--fc-crit)]/40 bg-[var(--fc-crit-bg)] px-3 py-2 text-xs text-[var(--fc-crit)]">
          Could not retrieve Redis data — see the logs for details.
        </div>
      </PageShell>
    );
  }

  return (
    <PageShell
      className="space-y-3"
      title={redisClusterEnabled ? "Redis Cluster Info" : "Redis Info"}
      sub={!redisClusterEnabled && redisAddress ? `Connected to: ${redisAddress}` : undefined}
    >
      {queueLocations && queueLocations.length > 0 && (
        <section className={panelClass}>
          <div className="border-b border-[var(--fc-line2)] px-3 py-2 text-xs font-semibold text-[var(--fc-ink)]">
            Queue Locations in Cluster
          </div>
          <QueueLocationTable queueLocations={queueLocations} />
        </section>
      )}

      {redisClusterNodesRaw && (
        <section className={panelClass}>
          <div className="border-b border-[var(--fc-line2)] px-3 py-2 text-xs font-semibold text-[var(--fc-ink)]">
            <a
              href="https://redis.io/commands/cluster-nodes"
              target="_blank"
              rel="noreferrer"
              className="hover:underline"
            >
              CLUSTER NODES
            </a>{" "}
            <span className="font-normal text-[var(--fc-ink3)]">output</span>
          </div>
          <div className="px-3 py-2">
            <SyntaxHighlighter language="yaml">{redisClusterNodesRaw}</SyntaxHighlighter>
          </div>
        </section>
      )}

      {redisInfo && !redisClusterEnabled && <RedisMetricTiles redisInfo={redisInfo} />}

      {redisInfoRaw && (
        <section className={panelClass}>
          <div className="border-b border-[var(--fc-line2)] px-3 py-2 text-xs font-semibold text-[var(--fc-ink)]">
            <a
              href="https://redis.io/commands/info"
              target="_blank"
              rel="noreferrer"
              className="hover:underline"
            >
              {redisClusterEnabled ? "CLUSTER INFO" : "INFO"}
            </a>{" "}
            <span className="font-normal text-[var(--fc-ink3)]">output</span>
          </div>
          <div className="px-3 py-2">
            <SyntaxHighlighter language="yaml">{redisInfoRaw}</SyntaxHighlighter>
          </div>
        </section>
      )}
    </PageShell>
  );
}
