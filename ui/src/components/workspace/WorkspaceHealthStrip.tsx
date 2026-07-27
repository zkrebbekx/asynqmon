// Queue Workspace header health strip (build contract §3.3): the numbers you
// drilled in for, on arrival — size by state, oldest-pending age, latency,
// consumers (expandable to names), paused-since, groups, archive-cap chip.
// Counts come from the live per-queue state_counts pipeline; ages/rates come
// from this queue's stats-cache row; consumers from the coverage join.

import { ReactNode, useState } from "react";
import { Link } from "react-router-dom";
import { ChevronDown, ChevronUp } from "lucide-react";
import type { CoverageRow, FleetQueueRow } from "../../api-fleet";
import {
  ageTone,
  formatErrorRate,
  formatLatencyMs,
  humanizeMs,
} from "../../lib/fleet";
import { ARCHIVE_CAP } from "../../lib/workspace";
import { paths } from "../../paths";
import { FcChip, MicroLabel } from "../FleetBits";
import { cn } from "../../lib/utils";

interface Props {
  qname: string;
  fleetRow: FleetQueueRow | null;
  // True when the /api/fleet endpoints are absent (stats engine disabled) —
  // age/rate cells render "—" with a tooltip instead of pretending zero.
  fleetUnavailable: boolean;
  counts: Record<string, number> | null;
  coverageRow: CoverageRow | null;
  nowMs: number;
}

function Cell({
  label,
  children,
  className,
}: {
  label: ReactNode;
  children: ReactNode;
  className?: string;
}) {
  return (
    <div
      className={cn(
        "min-w-[92px] border-l border-[var(--fc-line2)] px-3 py-2 first:border-l-0",
        className
      )}
    >
      <MicroLabel>{label}</MicroLabel>
      <div className="mt-0.5 text-[15px] font-semibold tabular-nums text-[var(--fc-ink)]">
        {children}
      </div>
    </div>
  );
}

function fmt(n: number | undefined | null): string {
  if (n === undefined || n === null) return "—";
  if (n < 0) return "—"; // -1 = unknowable (state_counts contract)
  return n.toLocaleString("en-US");
}

export default function WorkspaceHealthStrip({
  qname,
  fleetRow,
  fleetUnavailable,
  counts,
  coverageRow,
  nowMs,
}: Props) {
  const [consumersOpen, setConsumersOpen] = useState(false);

  const count = (state: string): number | null =>
    counts?.[state] ?? (fleetRow ? (fleetRow as unknown as Record<string, number>)[state] : null);

  const oldestMs = fleetRow?.oldest_pending_age_ms ?? null;
  const tone = ageTone(oldestMs);
  const pausedSince = fleetRow?.paused_since ? Date.parse(fleetRow.paused_since) : NaN;
  const archived = count("archived");
  const orphanCandidates = fleetRow?.orphan_candidates ?? 0;
  const pastDue = fleetRow?.past_due_scheduled ?? 0;

  const servers = coverageRow?.consumers ?? [];
  const slots = coverageRow?.total_concurrency ?? 0;

  return (
    <div className="rounded-lg border border-[var(--fc-line)] bg-[var(--fc-panel)]">
      <div className="flex flex-wrap items-stretch">
        <Cell label="state">
          {fleetRow?.paused ? (
            <span className="inline-flex items-center gap-1.5">
              <FcChip tone="warn">paused</FcChip>
              {Number.isFinite(pausedSince) && (
                <span className="text-[11px] font-normal text-[var(--fc-ink3)]">
                  {humanizeMs(nowMs - pausedSince)}
                </span>
              )}
            </span>
          ) : fleetRow ? (
            <FcChip tone="good">running</FcChip>
          ) : (
            <span className="text-[var(--fc-ink3)]">—</span>
          )}
        </Cell>
        <Cell label="pending">{fmt(count("pending"))}</Cell>
        <Cell label="oldest pending">
          <span
            title={
              fleetUnavailable
                ? "needs the stats engine — unavailable"
                : "age of the oldest pending task (pending_since)"
            }
            className={cn(
              tone === "crit" && "text-[var(--fc-crit)]",
              tone === "warn" && "text-[var(--fc-warn)]"
            )}
          >
            {oldestMs !== null && oldestMs > 0 ? humanizeMs(oldestMs) : "—"}
          </span>
        </Cell>
        <Cell label="latency">
          <span title={fleetUnavailable ? "needs the stats engine — unavailable" : undefined}>
            {fleetRow ? formatLatencyMs(fleetRow.latency_ms) : "—"}
          </span>
        </Cell>
        <Cell label="active">
          <span className="inline-flex items-center gap-1.5">
            {fmt(count("active"))}
            {orphanCandidates > 0 && (
              <FcChip tone="warn" title="expired leases beyond grace, debounced across sweeps">
                orphans {orphanCandidates}
              </FcChip>
            )}
          </span>
        </Cell>
        <Cell label="retry">{fmt(count("retry"))}</Cell>
        <Cell label="scheduled">
          <span className="inline-flex items-center gap-1.5">
            {fmt(count("scheduled"))}
            {pastDue > 0 && (
              <FcChip tone="warn" title="fire time behind now — forwarder may be stalled">
                past-due {pastDue}
              </FcChip>
            )}
          </span>
        </Cell>
        <Cell label="archived">
          <span className="inline-flex items-center gap-1.5">
            {fmt(archived)}
            {archived !== null && archived >= ARCHIVE_CAP && (
              <FcChip
                tone="warn"
                title="at asynq's 10k trim cap — every further archive discards the oldest entry"
              >
                cap
              </FcChip>
            )}
          </span>
        </Cell>
        <Cell label="completed">{fmt(count("completed"))}</Cell>
        <Cell label="groups">
          <span title="task groups in this queue (aggregating)">
            {fmt(fleetRow?.groups ?? count("groups"))}
          </span>
        </Cell>
        <Cell label="error rate 24h">
          {fleetRow ? formatErrorRate(fleetRow.error_rate) : "—"}
        </Cell>
        <Cell label="consumers" className="min-w-[130px]">
          <button
            onClick={() => setConsumersOpen((o) => !o)}
            title={
              coverageRow
                ? "servers consuming this queue — click to expand names"
                : "coverage join unavailable"
            }
            className="inline-flex items-center gap-1 hover:text-[var(--fc-acc)]"
          >
            {coverageRow ? (
              <span className={cn(servers.length === 0 && "font-semibold text-[var(--fc-crit)]")}>
                {servers.length} srv / {slots} slots
              </span>
            ) : (
              "—"
            )}
            {coverageRow &&
              (consumersOpen ? <ChevronUp size={13} /> : <ChevronDown size={13} />)}
          </button>
        </Cell>
      </div>

      {/* Expandable consumer names (§3.3 header: "expandable to names") */}
      {consumersOpen && coverageRow && (
        <div className="border-t border-[var(--fc-line2)] bg-[var(--fc-bg)]/60 px-3 py-2">
          {servers.length === 0 ? (
            <span className="text-xs text-[var(--fc-crit)]">
              No live server consumes this queue — pending work will not run.{" "}
              <Link to={paths().SERVERS} className="text-[var(--fc-acc)] hover:underline">
                Open Workers →
              </Link>
            </span>
          ) : (
            <div className="flex flex-wrap items-center gap-x-4 gap-y-1">
              {servers.map((c) => (
                <Link
                  key={c.server_id}
                  to={paths().SERVERS}
                  className="font-mono text-[11.5px] text-[var(--fc-acc)] hover:underline"
                  title={`server ${c.server_id}`}
                >
                  {c.host}:{c.pid}
                  <span className="text-[var(--fc-ink3)]"> · weight {c.weight}</span>
                  {c.strict_priority && (
                    <span className="ml-1 text-[var(--fc-warn)]">strict</span>
                  )}
                </Link>
              ))}
            </div>
          )}
        </div>
      )}
    </div>
  );
}
