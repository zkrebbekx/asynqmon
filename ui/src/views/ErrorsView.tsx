// Errors — failure-signature explorer (build contract §3.6, phase 9):
// "What is failing, since when, and can I fix it in one action?"
//
// Signatures are tracked entities from the errsig index: normalized template
// with ~PLACEHOLDER~ highlighting, count (≥-prefixed when any source queue
// is archive-trimmed), first/last seen, trend badge, blast-radius chips,
// sample task peeks into the Tasks drawer, and signature-scoped bulk verbs
// flowing into the standard job runner (BulkJobModal).
//
// Deliberately absent until phase 10 (no fabricated charts): count-over-time
// and deploy-marker sparklines — the detail panel renders the counter
// cross-check numbers instead, labeled.

import { useCallback, useEffect, useState } from "react";
import { Link, useSearchParams } from "react-router-dom";
import { useSelector } from "react-redux";
import { Info } from "lucide-react";
import { AppState } from "../store";
import { usePolling } from "../hooks";
import {
  ErrorSignatureRow,
  GetErrorSignatureResponse,
  ListErrorSignaturesResponse,
  getErrorSignature,
  listErrorSignatures,
} from "../api-errors";
import { JobVerb } from "../api";
import { paths } from "../paths";
import { timeAgo, toErrorString } from "../utils";
import {
  SIG_SCOPE_STATES,
  SigScopeState,
  blastLabel,
  formatSigCount,
  sigErrorClause,
  sigScopeAql,
  sigTasksSearch,
  sigVerbsForState,
  templateSegments,
  trendTone,
} from "../lib/errsig";
import { cn, clickableRowClass, clickableRowProps } from "../lib/utils";
import { FcChip, MicroLabel } from "../components/FleetBits";
import BulkJobModal, { BulkJobScope } from "../components/BulkJobModal";
import PageShell from "../components/PageShell";

const fmt = (n: number | undefined | null) =>
  n === undefined || n === null || !Number.isFinite(n) ? "—" : n.toLocaleString("en-US");

// The §3.6 data-honesty ribbon, verbatim.
const HONESTY_RIBBON =
  "Signatures are indexed from the archived tail (exact) and periodic bounded scans of " +
  "retry states (sampled). Magnitudes are cross-checked against failure counters. During " +
  "mass failures, archived data is trimmed at 10k/queue — counts are lower bounds.";

/**************************************************************
                    Template rendering
 **************************************************************/

function TemplateText({ template, className }: { template: string; className?: string }) {
  return (
    <code className={cn("font-mono text-[11.5px] text-[var(--fc-ink)]", className)}>
      {templateSegments(template).map((seg, i) =>
        seg.placeholder ? (
          <span key={i} className="rounded bg-[var(--fc-acc-bg)] px-0.5 font-semibold text-[var(--fc-acc)]">
            {seg.text}
          </span>
        ) : (
          <span key={i}>{seg.text}</span>
        )
      )}
    </code>
  );
}

function TrendChip({ trend }: { trend: string }) {
  return <FcChip tone={trendTone(trend)}>{trend}</FcChip>;
}

/**************************************************************
                      Signature row
 **************************************************************/

function SignatureRowView({
  row,
  selected,
  onSelect,
}: {
  row: ErrorSignatureRow;
  selected: boolean;
  onSelect: () => void;
}) {
  return (
    <tr
      {...clickableRowProps(onSelect)}
      className={cn(
        "border-b border-[var(--fc-line2)] last:border-b-0 hover:bg-[var(--fc-raise)]",
        clickableRowClass,
        selected && "bg-[var(--fc-acc-bg)]"
      )}
    >
      <td className="max-w-[420px] overflow-hidden text-ellipsis whitespace-nowrap px-2.5 py-1.5">
        <TemplateText template={row.template} />
        {row.count_is_lower_bound && (
          <FcChip tone="warn" className="ml-1.5" title="a source queue is at the 10k archive trim cap — this count is a lower bound">
            lower bound
          </FcChip>
        )}
      </td>
      <td className="num-cell font-semibold">{formatSigCount(row.count, row.count_is_lower_bound)}</td>
      <td className="px-2.5 py-1.5">
        <TrendChip trend={row.trend} />
      </td>
      <td className="num-cell text-[var(--fc-ink3)]" title={row.first_seen}>
        {row.first_seen ? timeAgo(row.first_seen) : "—"}
      </td>
      <td className="num-cell text-[var(--fc-ink3)]" title={row.last_seen}>
        {row.last_seen ? timeAgo(row.last_seen) : "—"}
      </td>
      <td className="whitespace-nowrap px-2.5 py-1.5 text-[11.5px] text-[var(--fc-ink3)]">
        {blastLabel(row.queues)}
      </td>
      <td className="whitespace-nowrap px-2.5 py-1.5 text-[11.5px]">
        {row.sample_task_refs.slice(0, 3).map((ref) => (
          <Link
            key={`${ref.queue}/${ref.id}`}
            to={`${paths().TASKS}${sigTasksSearch(row.template, "archived", ref)}`}
            onClick={(e) => e.stopPropagation()}
            className="mr-1.5 font-mono text-[var(--fc-acc)] hover:underline"
            title={`peek ${ref.queue}/${ref.id} in the Tasks drawer`}
          >
            {ref.id.slice(0, 6)}…
          </Link>
        ))}
      </td>
    </tr>
  );
}

/**************************************************************
                      Detail panel
 **************************************************************/

function SignatureDetail({
  detail,
  onVerb,
}: {
  detail: GetErrorSignatureResponse;
  onVerb: (verb: JobVerb, scope: BulkJobScope) => void;
}) {
  const [scopeState, setScopeState] = useState<SigScopeState>("retry");
  const row = detail.signature;
  // The modal scope carries the bare error clause (its `state` field names
  // the state); the display echo below uses the self-contained form.
  const clause = sigErrorClause(row.template);
  const aql = sigScopeAql(row.template, scopeState);
  const verbs = sigVerbsForState(scopeState);
  const verbTitle: Record<string, string> = {
    run: "Retry all with this signature… (throttled by default)",
    archive: "Archive all with this signature…",
    delete: "Delete all with this signature…",
  };

  return (
    <div className="mt-2.5 rounded-lg border border-[var(--fc-line)] bg-[var(--fc-panel)]">
      <div className="flex flex-wrap items-center gap-2 border-b border-[var(--fc-line2)] px-3 py-2">
        <TrendChip trend={row.trend} />
        <TemplateText template={row.template} className="text-xs" />
        <span className="flex-1" />
        <span className="text-[10.5px] text-[var(--fc-ink3)]">
          first seen {row.first_seen ? timeAgo(row.first_seen) : "—"} · last seen{" "}
          {row.last_seen ? timeAgo(row.last_seen) : "—"}
        </span>
      </div>

      <div className="grid grid-cols-1 lg:grid-cols-[1fr_320px]">
        {/* Left: magnitude cross-check (count-over-time is phase 10) */}
        <div className="px-3.5 py-3">
          <MicroLabel className="mb-1.5">
            occurrences — count-over-time arrives with ring buffers (phase 10); until then, the
            counter cross-check:
          </MicroLabel>
          <div className="space-y-1 text-xs">
            <div className="flex items-baseline gap-2">
              <span className="w-56 text-[var(--fc-ink3)]">index-derived count</span>
              <b className="font-mono tabular-nums">
                {formatSigCount(row.count, row.count_is_lower_bound)}
              </b>
              <span className="text-[10.5px] text-[var(--fc-ink3)]">
                = {fmt(row.archived_count)} archived (exact) + {fmt(row.retry_sampled_count)} retry
                (sampled)
              </span>
            </div>
            <div className="flex items-baseline gap-2">
              <span className="w-56 text-[var(--fc-ink3)]">failed today, affected queues</span>
              <b className="font-mono tabular-nums">{fmt(detail.failed_today_queues_total)}</b>
              <span className="text-[10.5px] text-[var(--fc-ink3)]">
                counter-derived — all failures in those queues, not only this signature
              </span>
            </div>
            <div className="flex items-baseline gap-2">
              <span className="w-56 text-[var(--fc-ink3)]">failed today, all queues</span>
              <b className="font-mono tabular-nums">{fmt(detail.counter_failed_today)}</b>
              <span className="text-[10.5px] text-[var(--fc-ink3)]">counters are never trimmed</span>
            </div>
          </div>

          <MicroLabel className="mb-1 mt-3.5">affected queue × type matrix</MicroLabel>
          <table className="w-full text-xs">
            <thead>
              <tr className="text-left text-[10.5px] uppercase tracking-[0.06em] text-[var(--fc-ink3)]">
                <th className="py-1 pr-2 font-medium">Queue</th>
                <th className="py-1 pr-2 font-medium">Type</th>
                <th className="py-1 pr-2 text-right font-medium">Count</th>
                <th className="py-1 pr-2 text-right font-medium">Failed today (queue)</th>
                <th className="py-1 font-medium">Source</th>
              </tr>
            </thead>
            <tbody>
              {row.queues.map((c) => {
                const counter = detail.failed_today_by_queue.find((f) => f.queue === c.queue);
                return (
                  <tr key={`${c.queue}|${c.type}`} className="border-t border-[var(--fc-line2)]">
                    <td className="py-1 pr-2 font-mono text-[var(--fc-acc)]">{c.queue}</td>
                    <td className="py-1 pr-2 font-mono text-[var(--fc-ink2)]">{c.type}</td>
                    <td className="py-1 pr-2 text-right font-mono tabular-nums">{fmt(c.count)}</td>
                    <td className="py-1 pr-2 text-right font-mono tabular-nums text-[var(--fc-ink3)]">
                      {counter ? fmt(counter.failed) : "—"}
                    </td>
                    <td className="py-1">
                      {c.sampled ? (
                        <FcChip tone="mut" title={`${c.archived} archived (exact) + ${c.retry_sampled} retry (sampled)`}>
                          + sampled
                        </FcChip>
                      ) : (
                        <FcChip tone="mut">exact</FcChip>
                      )}
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>

        {/* Right: blast radius + signature-scoped verbs + samples */}
        <div className="flex flex-col gap-2 border-t border-[var(--fc-line2)] px-3.5 py-3 lg:border-l lg:border-t-0">
          <MicroLabel>blast radius</MicroLabel>
          <div className="flex flex-wrap gap-1">
            {row.queues.map((c) => (
              <FcChip key={`${c.queue}|${c.type}`} tone="mut">
                {c.queue} · {c.type}
              </FcChip>
            ))}
          </div>

          {!window.READ_ONLY && (
            <>
              <MicroLabel className="mt-1.5">signature-scoped actions — act on</MicroLabel>
              <div className="flex gap-1">
                {SIG_SCOPE_STATES.map((st) => (
                  <button
                    key={st}
                    onClick={() => setScopeState(st)}
                    className={cn(
                      "rounded px-2 py-0.5 font-mono text-[11px]",
                      scopeState === st
                        ? "bg-[var(--fc-acc-bg)] font-semibold text-[var(--fc-acc)]"
                        : "text-[var(--fc-ink3)] hover:bg-[var(--fc-raise)]"
                    )}
                  >
                    state={st}
                  </button>
                ))}
              </div>
              {aql && clause ? (
                <>
                  {verbs.map((verb) => (
                    <button
                      key={verb}
                      onClick={() =>
                        onVerb(verb, { queue: "all", state: scopeState, q: "", meta: [], aql: clause })
                      }
                      className={cn(
                        "rounded-md border px-2.5 py-1.5 text-left text-xs transition-colors hover:bg-[var(--fc-raise)]",
                        verb === "delete"
                          ? "border-[var(--fc-crit)]/40 text-[var(--fc-crit)]"
                          : "border-[var(--fc-line)] text-[var(--fc-ink)]"
                      )}
                    >
                      {verb === "run" ? "↻ " : verb === "archive" ? "⊞ " : "✕ "}
                      {verbTitle[verb]}
                    </button>
                  ))}
                  <div className="text-[10.5px] leading-snug text-[var(--fc-ink3)]">
                    scope: <code className="font-mono">{aql}</code> — matches the raw-error
                    prefix, which can be broader than this exact signature; the job previews and
                    re-verifies every task before acting.
                  </div>
                </>
              ) : (
                <div className="text-[10.5px] text-[var(--fc-ink3)]">
                  This template is all placeholders — no literal fragment is selective enough to
                  scope a bulk verb on. Use the Tasks console instead.
                </div>
              )}
            </>
          )}

          <MicroLabel className="mt-1.5">sample tasks</MicroLabel>
          <div className="flex flex-wrap gap-1.5 text-[11.5px]">
            {row.sample_task_refs.length === 0 && (
              <span className="text-[var(--fc-ink3)]">none captured yet</span>
            )}
            {row.sample_task_refs.map((ref) => (
              <Link
                key={`${ref.queue}/${ref.id}`}
                to={`${paths().TASKS}${sigTasksSearch(row.template, "archived", ref)}`}
                className="font-mono text-[var(--fc-acc)] hover:underline"
                title={`${ref.queue}/${ref.id}`}
              >
                {ref.id.slice(0, 8)}…
              </Link>
            ))}
          </div>
          <Link
            to={`${paths().TASKS}${sigTasksSearch(row.template, "archived")}`}
            className="mt-0.5 text-xs text-[var(--fc-acc)] hover:underline"
          >
            ⌕ Open in Tasks console →
          </Link>
        </div>
      </div>
    </div>
  );
}

/**************************************************************
                        ErrorsView
 **************************************************************/

type SortKey = "last_seen" | "count";

export default function ErrorsView() {
  const pollInterval = useSelector((s: AppState) => s.settings.pollInterval);
  const [searchParams, setSearchParams] = useSearchParams();
  const selectedSig = searchParams.get("sig") ?? "";
  const sort: SortKey = searchParams.get("sort") === "count" ? "count" : "last_seen";

  const [resp, setResp] = useState<ListErrorSignaturesResponse | null>(null);
  const [detail, setDetail] = useState<GetErrorSignatureResponse | null>(null);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);

  // Bulk-verb modal state (§4.3 flow, shared machinery).
  const [job, setJob] = useState<{ verb: JobVerb; scope: BulkJobScope } | null>(null);

  const fetchList = useCallback(async () => {
    try {
      const r = await listErrorSignatures({ sort, limit: 50 });
      setResp(r);
      setError("");
    } catch (e) {
      setError(toErrorString(e as Parameters<typeof toErrorString>[0]));
    } finally {
      setLoading(false);
    }
  }, [sort]);

  usePolling(fetchList, pollInterval, [sort]);

  // Detail follows the selection; refreshed on every list poll so the
  // cross-check numbers track the ribbon's stamps.
  useEffect(() => {
    let stale = false;
    if (!selectedSig) {
      setDetail(null);
      return;
    }
    getErrorSignature(selectedSig)
      .then((d) => {
        if (!stale) setDetail(d);
      })
      .catch(() => {
        if (!stale) setDetail(null);
      });
    return () => {
      stale = true;
    };
  }, [selectedSig, resp]);

  const select = (sig: string) => {
    setSearchParams(
      (prev) => {
        const p = new URLSearchParams(prev);
        if (p.get("sig") === sig) p.delete("sig");
        else p.set("sig", sig);
        return p;
      },
      { replace: false }
    );
  };
  const setSort = (s: SortKey) => {
    setSearchParams((prev) => {
      const p = new URLSearchParams(prev);
      if (s === "last_seen") p.delete("sort");
      else p.set("sort", s);
      return p;
    });
  };

  const rows = resp?.signatures ?? [];
  const neverIndexed = resp !== null && resp.tail_indexed_at === "" && resp.retry_sampled_at === "";

  return (
    <PageShell>
      {/* §3.6 data-honesty ribbon, verbatim */}
      <div className="mb-2.5 flex items-start gap-2 rounded-md border border-[var(--fc-line)] bg-[var(--fc-panel)] px-3 py-2 text-xs text-[var(--fc-ink2)]">
        <Info size={14} className="mt-0.5 shrink-0 text-[var(--fc-acc)]" />
        <span>{HONESTY_RIBBON}</span>
      </div>

      {error && (
        <div className="mb-2.5 rounded-md border border-[var(--fc-crit)]/40 bg-[var(--fc-crit-bg)] px-3 py-2 text-xs text-[var(--fc-crit)]">
          Error signatures unavailable: {error}
        </div>
      )}

      <div className="rounded-lg border border-[var(--fc-line)] bg-[var(--fc-panel)]">
        <div className="flex flex-wrap items-center gap-2 border-b border-[var(--fc-line2)] px-3 py-2 text-xs">
          <span className="font-semibold text-[var(--fc-ink)]">
            Failure signatures{resp ? ` · ${fmt(resp.total_signatures)}` : ""}
          </span>
          {resp && resp.honesty.trimmed_queues.length > 0 && (
            <FcChip tone="warn">
              lower bounds — {resp.honesty.trimmed_queues.length} queue
              {resp.honesty.trimmed_queues.length > 1 ? "s" : ""} at archive cap
            </FcChip>
          )}
          <span className="flex-1" />
          {resp && (
            <span className="text-[10.5px] text-[var(--fc-ink3)]">
              index {formatSigCount(resp.index_count_total, resp.honesty.trimmed_queues.length > 0)}{" "}
              · counters {fmt(resp.counter_failed_today)} failed today · tail{" "}
              {resp.tail_indexed_at ? timeAgo(resp.tail_indexed_at) : "never"} · sample{" "}
              {resp.retry_sampled_at ? timeAgo(resp.retry_sampled_at) : "never"}
            </span>
          )}
          <div className="flex gap-1">
            {(["last_seen", "count"] as const).map((s) => (
              <button
                key={s}
                onClick={() => setSort(s)}
                className={cn(
                  "rounded px-2 py-0.5 text-[11px]",
                  sort === s
                    ? "bg-[var(--fc-acc-bg)] font-semibold text-[var(--fc-acc)]"
                    : "text-[var(--fc-ink3)] hover:bg-[var(--fc-raise)]"
                )}
              >
                {s === "last_seen" ? "recent" : "top count"}
              </button>
            ))}
          </div>
        </div>

        <div className="overflow-x-auto">
          <table className="w-full border-collapse text-xs">
            <thead>
              <tr className="text-left text-[10.5px] uppercase tracking-[0.06em] text-[var(--fc-ink3)]">
                <th className="px-2.5 py-1.5 font-medium">Signature</th>
                <th className="px-2.5 py-1.5 text-right font-medium">Count</th>
                <th className="px-2.5 py-1.5 font-medium">Trend</th>
                <th className="px-2.5 py-1.5 text-right font-medium">First seen</th>
                <th className="px-2.5 py-1.5 text-right font-medium">Last seen</th>
                <th className="px-2.5 py-1.5 font-medium">Blast</th>
                <th className="px-2.5 py-1.5 font-medium">Samples</th>
              </tr>
            </thead>
            <tbody>
              {rows.map((row) => (
                <SignatureRowView
                  key={row.sig}
                  row={row}
                  selected={row.sig === selectedSig}
                  onSelect={() => select(row.sig)}
                />
              ))}
            </tbody>
          </table>
          {loading && !resp && (
            <div className="px-3 py-8 text-center text-xs text-[var(--fc-ink3)]">Loading…</div>
          )}
          {!loading && rows.length === 0 && !error && (
            <div className="px-3 py-8 text-center text-xs text-[var(--fc-ink3)]">
              {neverIndexed
                ? "The error-signature indexer hasn't completed a sweep yet — signatures appear within ~15s of the first archived or retrying failure."
                : "No failure signatures indexed — nothing has failed into retry or the archive."}
            </div>
          )}
        </div>
      </div>

      {detail && <SignatureDetail detail={detail} onVerb={(verb, scope) => setJob({ verb, scope })} />}

      {job && (
        <BulkJobModal
          open
          verb={job.verb}
          scope={job.scope}
          onClose={() => setJob(null)}
        />
      )}
    </PageShell>
  );
}
