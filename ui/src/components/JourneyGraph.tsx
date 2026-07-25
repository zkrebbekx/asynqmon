import { TaskInfo } from "../api";
import {
  deriveJourney,
  journeyEdges,
  JourneyEdge,
  JourneyState,
  JOURNEY_STATES,
  STATE_TONE,
} from "../lib/journey";
import { durationBefore, durationSince, timeAgo } from "../utils";

// Inline SVG of the asynq state machine with this task's traversed path
// highlighted (geometry and look from the approved fleet-console mockup).
// Path derivation lives in lib/journey.ts; this component only maps it to
// shapes, tones, and the duration labels the stored fields support.

const NODE_H = 32;
const NODES: Record<JourneyState, { x: number; y: number; w: number }> = {
  scheduled: { x: 16, y: 30, w: 80 },
  pending: { x: 146, y: 30, w: 80 },
  aggregating: { x: 276, y: 30, w: 80 },
  active: { x: 406, y: 30, w: 80 },
  completed: { x: 536, y: 30, w: 88 },
  retry: { x: 332, y: 106, w: 110 },
  archived: { x: 548, y: 112, w: 76 },
};

// The pending→active arc bows over aggregating: tasks that never grouped
// skip that state, and the arc says so visually.
const EDGES: Array<{ id: JourneyEdge; d: string; loop?: boolean }> = [
  { id: "scheduled→pending", d: "M96 46 H136" },
  { id: "pending→aggregating", d: "M226 46 H266" },
  { id: "aggregating→active", d: "M356 46 H396" },
  { id: "pending→active", d: "M216 30 C 252 6, 380 6, 438 28" },
  { id: "active→completed", d: "M486 46 H526" },
  { id: "active→retry", d: "M470 62 C500 92, 470 122, 442 122", loop: true },
  { id: "retry→pending", d: "M330 122 C270 122, 232 96, 214 66" },
  { id: "retry→archived", d: "M442 138 C480 158, 520 150, 545 132", loop: true },
];

const TONE_COLOR: Record<string, string> = {
  good: "var(--fc-good)",
  warn: "var(--fc-warn)",
  crit: "var(--fc-crit)",
  info: "var(--fc-acc)",
  mut: "var(--fc-acc)", // neutral states still pulse in the accent, not gray
};
const TONE_BG: Record<string, string> = {
  good: "var(--fc-good-bg)",
  warn: "var(--fc-warn-bg)",
  crit: "var(--fc-crit-bg)",
  info: "var(--fc-acc-bg)",
  mut: "var(--fc-acc-bg)",
};

const LABEL_FONT = "10px ui-monospace, Menlo, monospace";
const NODE_FONT = "600 10.5px ui-monospace, Menlo, monospace";

interface EdgeLabel {
  x: number;
  y: number;
  text: string;
  tone?: "crit" | "dim";
}

// Duration labels only where TaskInfo supports them: pending_since exists
// only while pending, start_time only while active, next_process_at for
// scheduled/retry — everything else is omitted rather than fabricated.
function deriveLabels(task: TaskInfo, retryAttempts: number): EdgeLabel[] {
  const labels: EdgeLabel[] = [];
  if (retryAttempts > 0) {
    labels.push({
      x: 502,
      y: 98,
      text: `${retryAttempts} attempt${retryAttempts === 1 ? "" : "s"}`,
      tone: "crit",
    });
  }
  switch (task.state) {
    case "pending":
      if (task.pending_since) {
        labels.push({ x: 186, y: 16, text: `waiting ${durationSince(task.pending_since)}` });
      }
      break;
    case "active":
      if (task.start_time) {
        labels.push({ x: 446, y: 16, text: `running ${durationSince(task.start_time)}` });
      }
      break;
    case "scheduled":
      if (task.next_process_at) {
        labels.push({ x: 56, y: 80, text: `runs ${durationBefore(task.next_process_at)}` });
      }
      break;
    case "retry":
      if (task.next_process_at) {
        labels.push({ x: 256, y: 112, text: `next ${durationBefore(task.next_process_at)}` });
      }
      break;
    case "completed":
      if (task.completed_at) {
        labels.push({ x: 580, y: 80, text: timeAgo(task.completed_at), tone: "dim" });
      }
      break;
    case "archived":
      if (retryAttempts > 0) {
        labels.push({ x: 500, y: 158, text: `at attempt ${task.retried}/${task.max_retry}` });
      }
      break;
  }
  return labels;
}

function Arrowhead({ id, fill }: { id: string; fill: string }) {
  return (
    <marker
      id={id}
      viewBox="0 0 8 8"
      refX="7"
      refY="4"
      markerWidth="7"
      markerHeight="7"
      orient="auto"
    >
      <path d="M0 0L8 4L0 8z" fill={fill} />
    </marker>
  );
}

export default function JourneyGraph({ task }: { task: TaskInfo }) {
  const journey = deriveJourney(task);
  const edges = journeyEdges(journey);
  const labels = deriveLabels(task, journey.retryAttempts);

  const traversedList = JOURNEY_STATES.filter((s) => journey.traversed.has(s));
  const ariaLabel =
    `Task state machine. Traversed: ${traversedList.join(", ") || "unknown"}. ` +
    `Current state: ${journey.current ?? task.state}` +
    (journey.retryAttempts > 0 ? ` after ${journey.retryAttempts} retries.` : ".");

  return (
    <div className="overflow-x-auto rounded-md border border-[var(--fc-line)] bg-[var(--fc-bg)] px-1 py-1.5">
      {/* Scales down to the drawer width (min-width keeps labels legible on
          narrow viewports, with the wrapper scrolling as the fallback). */}
      <svg
        viewBox="0 0 640 168"
        role="img"
        aria-label={ariaLabel}
        className="mx-auto block h-auto w-full min-w-[520px]"
      >
        <defs>
          <Arrowhead id="fc-ah-dim" fill="var(--fc-line)" />
          <Arrowhead id="fc-ah-hit" fill="var(--fc-acc)" />
          <Arrowhead id="fc-ah-crit" fill="var(--fc-crit)" />
        </defs>

        {/* edges (drawn first so nodes sit on top) */}
        {EDGES.map((e) => {
          const hit = edges.has(e.id);
          const stroke = !hit
            ? "var(--fc-line)"
            : e.loop
              ? "var(--fc-crit)"
              : "var(--fc-acc)";
          const marker = !hit ? "fc-ah-dim" : e.loop ? "fc-ah-crit" : "fc-ah-hit";
          return (
            <path
              key={e.id}
              d={e.d}
              fill="none"
              stroke={stroke}
              strokeWidth={hit ? 1.8 : 1.4}
              markerEnd={`url(#${marker})`}
            />
          );
        })}

        {/* nodes */}
        {JOURNEY_STATES.map((s) => {
          const n = NODES[s];
          const isCurrent = journey.current === s;
          const isHit = journey.traversed.has(s);
          const tone = TONE_COLOR[STATE_TONE[s]];
          const rectProps = isCurrent
            ? { fill: TONE_BG[STATE_TONE[s]], stroke: tone, strokeWidth: 1.6 }
            : isHit
              ? { fill: "var(--fc-acc-bg)", stroke: "var(--fc-acc)", strokeWidth: 1 }
              : { fill: "var(--fc-raise)", stroke: "var(--fc-line)", strokeWidth: 1 };
          const textFill = isCurrent ? tone : isHit ? "var(--fc-ink)" : "var(--fc-ink3)";
          return (
            <g key={s}>
              <rect
                x={n.x}
                y={n.y}
                width={n.w}
                height={NODE_H}
                rx={6}
                {...rectProps}
                // Dash the aggregating box when this task never grouped — the
                // optional-path cue from the mockup.
                strokeDasharray={s === "aggregating" && !journey.viaGroup ? "3 3" : undefined}
              />
              <text
                x={n.x + n.w / 2}
                y={n.y + 20}
                textAnchor="middle"
                fill={textFill}
                style={{ font: NODE_FONT }}
              >
                {s}
              </text>
              {isCurrent && (
                <circle
                  className="fc-pulse"
                  cx={n.x + n.w - 11}
                  cy={n.y + 16}
                  r={4}
                  fill={tone}
                />
              )}
            </g>
          );
        })}

        {/* duration / attempt labels */}
        {labels.map((l) => (
          <text
            key={`${l.x},${l.y}`}
            x={l.x}
            y={l.y}
            textAnchor="middle"
            fill={l.tone === "crit" ? "var(--fc-crit)" : l.tone === "dim" ? "var(--fc-ink3)" : "var(--fc-ink2)"}
            style={{ font: LABEL_FONT, fontWeight: l.tone === "crit" ? 700 : 400 }}
          >
            {l.text}
          </text>
        ))}
      </svg>
    </div>
  );
}
