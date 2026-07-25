// Queue Workspace right rail (build contract §3.3(c)):
//   (a) this queue's error clusters with per-cluster bulk verbs,
//   (b) consumers panel (coverage row: servers, weights, strict warning),
//   (c) retry-ETA histogram (exact) + pending-wait histogram (sampled,
//       head/tail-biased — labeled verbatim),
//   (d) memory attribution — sample-on-demand with a "sampled" stamp.

import { ReactNode } from "react";
import { Link } from "react-router-dom";
import prettyBytes from "pretty-bytes";
import type { JobVerb } from "../../api";
import type {
  CoverageRow,
  PendingWaitSampleResponse,
  RetryHistogramResponse,
} from "../../api-fleet";
import {
  ErrorCluster,
  pendingWaitView,
  retryHistogramView,
} from "../../lib/workspace";
import { COVERAGE_TONE, coverageChipLabel } from "../../lib/workers";
import { paths } from "../../paths";
import { FcChip } from "../FleetBits";
import { Button } from "../ui/button";
import { cn } from "../../lib/utils";

export interface MemorySample {
  bytes: number | null;
  sampledAt: Date | null;
  sampling: boolean;
  onSample: () => void;
}

interface Props {
  clusters: ErrorCluster[];
  clustersTruncated: boolean;
  clustersScanned: number;
  clustersUnavailable: boolean;
  coverageRow: CoverageRow | null;
  retryHist: RetryHistogramResponse | null;
  waitSample: PendingWaitSampleResponse | null;
  memory: MemorySample;
  onClusterVerb: (verb: JobVerb, state: "retry" | "archived", signature: string) => void;
}

function Panel({ title, chip, children }: { title: ReactNode; chip?: ReactNode; children: ReactNode }) {
  return (
    <div className="rounded-lg border border-[var(--fc-line)] bg-[var(--fc-panel)]">
      <div className="flex items-center gap-2 border-b border-[var(--fc-line2)] px-3 py-2 text-xs font-semibold text-[var(--fc-ink)]">
        {title}
        {chip}
      </div>
      {children}
    </div>
  );
}

function Caption({ children }: { children: ReactNode }) {
  return <div className="px-3 pb-2.5 pt-1.5 text-[10.5px] leading-snug text-[var(--fc-ink3)]">{children}</div>;
}

// Bars renders a histogram as flex columns; height 0 draws a 2px baseline
// nub so empty buckets stay visible as "zero", not missing.
function Bars({
  bars,
  hotIndex,
  titles,
}: {
  bars: number[];
  hotIndex?: number;
  titles: string[];
}) {
  return (
    <div className="flex h-16 items-end gap-[3px] px-3 pt-2">
      {bars.map((h, i) => (
        <span
          key={i}
          title={titles[i]}
          className={cn(
            "min-h-[2px] flex-1 rounded-t-sm",
            i === hotIndex ? "bg-[var(--fc-crit)]" : "bg-[var(--fc-acc-dim)]"
          )}
          style={{ height: `${Math.max(h, 2)}%` }}
        />
      ))}
    </div>
  );
}

export default function WorkspaceRail({
  clusters,
  clustersTruncated,
  clustersScanned,
  clustersUnavailable,
  coverageRow,
  retryHist,
  waitSample,
  memory,
  onClusterVerb,
}: Props) {
  const retryView = retryHistogramView(retryHist);
  const waitView = pendingWaitView(waitSample);
  const maxCluster = clusters.length > 0 ? clusters[0].total : 0;

  return (
    <div className="flex flex-col gap-2.5">
      {/* (a) Error clusters */}
      <Panel
        title="Error clusters · this queue"
        chip={
          clustersTruncated ? (
            <FcChip tone="warn" title={`bounded scan stopped early after ${clustersScanned.toLocaleString()} tasks — counts are lower bounds`}>
              partial
            </FcChip>
          ) : undefined
        }
      >
        {clustersUnavailable ? (
          <Caption>error clusters unavailable — the aggregate endpoint did not answer.</Caption>
        ) : clusters.length === 0 ? (
          <Caption>No errors in retry or archived. Quiet is good.</Caption>
        ) : (
          <div>
            {clusters.map((c) => (
              <div key={c.label} className="border-b border-[var(--fc-line2)] px-3 py-2 last:border-b-0">
                <code
                  className="block truncate font-mono text-[11px] text-[var(--fc-ink)]"
                  title={c.label}
                >
                  {c.label}
                </code>
                <div className="mt-1 flex items-center gap-2">
                  <span className="h-1 w-24 overflow-hidden rounded bg-[var(--fc-raise)]">
                    <span
                      className="block h-full rounded bg-[var(--fc-crit)]"
                      style={{ width: `${maxCluster > 0 ? Math.max(4, Math.round((c.total / maxCluster) * 100)) : 0}%` }}
                    />
                  </span>
                  <span className="font-mono text-[11px] tabular-nums">
                    <b>{c.total.toLocaleString()}</b>
                    <span className="text-[var(--fc-ink3)]">
                      {" "}
                      · retry {c.retryCount.toLocaleString()} · archived {c.archivedCount.toLocaleString()}
                    </span>
                  </span>
                </div>
                {!window.READ_ONLY && (
                  <div className="mt-1.5 flex gap-1.5">
                    {(c.retryCount > 0 || c.archivedCount > 0) && (
                      <Button
                        size="sm"
                        variant="outline"
                        className="h-6 text-[10.5px]"
                        title="Bulk run every task with this signature — throttled background job"
                        onClick={() =>
                          onClusterVerb("run", c.retryCount > 0 ? "retry" : "archived", c.label)
                        }
                      >
                        Retry cluster…
                      </Button>
                    )}
                    {c.retryCount > 0 && (
                      <Button
                        size="sm"
                        variant="outline"
                        className="h-6 text-[10.5px]"
                        title="Bulk archive the retry-state tasks with this signature"
                        onClick={() => onClusterVerb("archive", "retry", c.label)}
                      >
                        Archive…
                      </Button>
                    )}
                    {c.archivedCount > 0 && (
                      <Button
                        size="sm"
                        variant="outline"
                        className="h-6 text-[10.5px] text-red-500 hover:text-red-600"
                        title="Bulk delete the archived tasks with this signature — gone forever"
                        onClick={() => onClusterVerb("delete", "archived", c.label)}
                      >
                        Delete…
                      </Button>
                    )}
                  </div>
                )}
              </div>
            ))}
            <Caption>
              clusters group by last error (asynq stores only the most recent) over a
              bounded scan of retry + archived.
            </Caption>
          </div>
        )}
      </Panel>

      {/* (b) Consumers */}
      <Panel
        title="Consumers"
        chip={
          coverageRow ? (
            <FcChip tone={COVERAGE_TONE[coverageRow.status]}>
              {coverageChipLabel(coverageRow.status)}
            </FcChip>
          ) : undefined
        }
      >
        {!coverageRow ? (
          <Caption>coverage join unavailable.</Caption>
        ) : coverageRow.consumers.length === 0 ? (
          <div className="px-3 py-2.5 text-xs text-[var(--fc-crit)]">
            No live server consumes this queue — pending work will not run.{" "}
            <Link to={paths().SERVERS} className="text-[var(--fc-acc)] hover:underline">
              Open Workers →
            </Link>
          </div>
        ) : (
          <div>
            <div className="flex flex-col gap-1.5 px-3 py-2 text-xs">
              {coverageRow.consumers.map((c) => (
                <div key={c.server_id} className="flex items-center justify-between gap-2">
                  <Link
                    to={paths().SERVERS}
                    className="truncate font-mono text-[11.5px] text-[var(--fc-acc)] hover:underline"
                  >
                    {c.host}:{c.pid}
                  </Link>
                  <span className="whitespace-nowrap font-mono text-[11px] text-[var(--fc-ink2)]">
                    weight {c.weight}
                    {c.strict_priority && <span className="text-[var(--fc-warn)]"> · strict</span>}
                  </span>
                </div>
              ))}
            </div>
            <Caption>
              {coverageRow.status === "starved"
                ? "strict priority: this queue's low weight can starve behind permanently-busy higher priorities."
                : coverageRow.consumers.some((c) => c.strict_priority)
                  ? "strict priority is on for at least one server — lower-weight queues can starve."
                  : "strict priority off — no starvation risk from this config."}{" "}
              {coverageRow.total_concurrency.toLocaleString()} worker slots shared across
              each server's whole queue map.
            </Caption>
          </div>
        )}
      </Panel>

      {/* (c1) Retry-ETA histogram */}
      <Panel title="Retry ETA · next 30m" chip={<FcChip tone="mut" title="pipelined ZCOUNT windows over retry scores — a count, not a sample">exact</FcChip>}>
        {!retryView ? (
          <Caption>retry histogram unavailable.</Caption>
        ) : retryView.total === 0 ? (
          <Caption>No retries queued.</Caption>
        ) : (
          <div>
            <Bars
              bars={retryView.bars}
              hotIndex={retryView.hotIndex}
              titles={retryView.bars.map(
                (_, i) => `${retryView.bucketLabel(i)}: ${retryHist!.buckets[i].toLocaleString()} tasks`
              )}
            />
            <Caption>
              <b className="text-[var(--fc-ink2)]">
                {retryView.next5m.toLocaleString()} tasks fire in the next 5m
              </b>
              {retryView.stormLabel && <> — {retryView.stormLabel}</>}
              {retryView.pastDue > 0 && (
                <> · {retryView.pastDue.toLocaleString()} already past due</>
              )}
              {retryView.beyond > 0 && (
                <> · {retryView.beyond.toLocaleString()} beyond 30m</>
              )}
            </Caption>
          </div>
        )}
      </Panel>

      {/* (c2) Pending-wait histogram */}
      <Panel title="Pending wait · sampled">
        {!waitView ? (
          <Caption>pending-wait sample unavailable.</Caption>
        ) : waitView.listLen === 0 ? (
          <Caption>No pending backlog.</Caption>
        ) : (
          <div>
            <Bars
              bars={waitView.bars}
              titles={waitView.buckets.map((b) => `${b.label}: ${b.count} sampled`)}
            />
            <div className="flex justify-between px-3 pt-1 text-[9.5px] text-[var(--fc-ink3)]">
              <span>{waitView.buckets[0].label}</span>
              <span>{waitView.buckets[waitView.buckets.length - 1].label}</span>
            </div>
            <Caption>
              {/* §3.3(c): the honesty label travels from the API verbatim. */}
              <b className="text-[var(--fc-warn)]">{waitView.method}</b> — pending is a Redis
              LIST; probes drawn from the head/tail bands ({waitView.sampled} of{" "}
              {waitView.listLen.toLocaleString()}), not uniform.
            </Caption>
          </div>
        )}
      </Panel>

      {/* (d) Memory — sample on demand */}
      <Panel title="Memory">
        <div className="flex items-center justify-between gap-2 px-3 py-2.5">
          <div>
            <div className="text-[15px] font-semibold tabular-nums">
              {memory.bytes !== null ? prettyBytes(memory.bytes) : "—"}
            </div>
            <div className="text-[10.5px] text-[var(--fc-ink3)]">
              {memory.sampledAt
                ? `sampled ${memory.sampledAt.toLocaleTimeString()}`
                : "not sampled yet — MEMORY USAGE runs only on demand"}
            </div>
          </div>
          <Button
            size="sm"
            variant="outline"
            className="h-7 text-[11px]"
            disabled={memory.sampling}
            onClick={memory.onSample}
          >
            {memory.sampling ? "Sampling…" : "Sample now (~2s)"}
          </Button>
        </div>
      </Panel>
    </div>
  );
}
