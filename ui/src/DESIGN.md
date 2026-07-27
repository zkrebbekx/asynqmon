# Fleet Console design system (ui/src)

One visual system across every route. If a surface doesn't look like it came
from the Fleet screen, it's wrong.

## Tokens

All color comes from the `--fc-*` custom properties in `index.css` (raw hex,
light theme re-picked — not inverted). Consume them as
`text-[var(--fc-ink2)]`, `bg-[var(--fc-panel)]`, etc.

| Token | Role |
|---|---|
| `--fc-bg` | app canvas (behind everything) |
| `--fc-panel` | card / panel / popover / input surface |
| `--fc-raise` | raised-on-panel surface: hovers, tracks, code-ish chips, skeletons |
| `--fc-line` | panel borders, table header rule |
| `--fc-line2` | subtle internal rules: row borders, panel-header underline |
| `--fc-ink` / `--fc-ink2` / `--fc-ink3` | primary / secondary / faint text |
| `--fc-acc` (+`-fg`, `-dim`, `-bg`) | interaction only: links, active nav/tabs/pills, focus, primary buttons. `--fc-acc-fg` is the on-accent text color |
| `--fc-good/warn/crit` (+`-bg`) | status. Never color alone — pair with a form cue (chip text, stripe, dot) |
| `--fc-shadow` | overlay elevation (dialogs, popovers, drawer) |

The legacy shadcn names (`--background`, `--card`, `--border`, …) still exist
for the `components/ui/*` library but are **aliases into the fc palette**
(see the mapping comment in `index.css`). Never add new colors there; never
use raw Tailwind palette colors (`red-500`, `amber-400`, …) for UI chrome.

## Page shell

Every routed view renders inside `<PageShell>` (`components/PageShell.tsx`):
full-width up to `max-w-[1560px]`, centered beyond, `px-4 py-4`. No view
defines its own outer container.

```tsx
<PageShell
  className="space-y-3"        // vertical rhythm between sections (see below)
  title="Queue Metrics"        // optional 15px semibold title row
  sub="muted annotation"       // optional line under the title
  eyebrow="section"            // optional MicroLabel caps line above it
  actions={<Controls />}       // right-aligned controls in the title row
>
```

- Dense dashboards (Fleet, Queues, Errors, Workers, Hygiene, queue
  workspace) omit the title row — the topbar breadcrumb owns page identity.
- Pages that need an in-page header (Tasks, Operations, Metrics, Redis,
  Settings) use `title` (+`sub`/`actions`). Never render an `<h1>` yourself.
- A deliberately narrow form page caps itself with `className="max-w-2xl"`
  (Settings is the only one).

## Spacing scale

- Page padding: `px-4 py-4` (PageShell — don't add more).
- Section gap: `space-y-3` (10–12px family). Fleet manages its own `mb-1.5`
  / `mb-3` rhythm; everything else uses `space-y-3` (Ops uses `space-y-4`
  for its larger blocks).
- Panel header row: `px-3 py-2`; panel body padding `px-3 py-2`/`py-3`.
- Table cells: `px-2.5 py-1.5` (dense) to `py-2`; grids of tiles: `gap-2`.

## Panels / cards

One card chrome, exported from `PageShell.tsx`:

```
panelClass = "rounded-lg border border-[var(--fc-line)] bg-[var(--fc-panel)]"
```

Panel header row: `border-b border-[var(--fc-line2)] px-3 py-2 text-xs
font-semibold text-[var(--fc-ink)]`, with a `font-normal text-[var(--fc-ink3)]`
inline annotation for counts/caveats. KPI/metric tiles are `panelClass` +
`MicroLabel` eyebrow + left-aligned `font-mono text-[19px] font-semibold
tabular-nums` value (see FleetView `KpiTile`, RedisInfoView `MetricTile`) —
never centered big numbers.

## Tables

- Header cells: `theadClass` (PageShell) — letterspaced caps, 10.5px,
  `text-[var(--fc-ink3)]`, `border-b border-[var(--fc-line)]`.
- Body cells: `cellClass` — `border-b border-[var(--fc-line2)]`.
- Numerics are always `font-mono tabular-nums`, right-aligned when columnar.
- Row hover: `hover:bg-[var(--fc-raise)]`. Clickable rows use
  `clickableRowClass` + `clickableRowProps` (lib/utils).
- The shadcn `components/ui/table` primitives carry the same styling for the
  legacy per-state tables — use either, they must look identical.

## Chips, labels, keys (components/FleetBits.tsx)

- `FcChip` tone `crit|warn|good|info|mut` — status chips, caps 10.5px.
  Tone is always paired with a form cue (the chip text itself, `SEV_STRIPE`
  stripes, dots) — never color alone.
- `MicroLabel` — letterspaced-caps eyebrow for tile labels and section
  micro-headers.
- `Kbd` — keyboard-key chip.
- Active nav/tab/pill state everywhere:
  `bg-[var(--fc-acc-bg)] font-semibold text-[var(--fc-acc)]`; inactive:
  `text-[var(--fc-ink2)] hover:bg-[var(--fc-raise)] hover:text-[var(--fc-ink)]`.

## Banners

- info: `border-[var(--fc-line)] bg-[var(--fc-panel)]`
- warn: `border-[var(--fc-warn)]/45 bg-[var(--fc-warn-bg)] text-[var(--fc-warn)]`
- crit: `border-[var(--fc-crit)]/40 bg-[var(--fc-crit-bg)] text-[var(--fc-crit)]`
- Whole-scope (bulk) bars are the dashed-warn variant; selection bars the
  `--fc-acc` variant (see TasksGlobalView).

## When to use what

- New page → `PageShell` + `panelClass` sections. Copy an existing fc view
  (FleetView, QueuesDirectoryView, ErrorsView are the reference).
- Status/state → `FcChip`; metric → KPI-tile pattern; code/payload →
  `SyntaxHighlighter` inside a panel.
- Dialogs/toasts/inputs → the `components/ui/*` primitives (they are
  token-aliased to fc) or the inline fc input pattern
  (`border-[var(--fc-line)] bg-[var(--fc-panel)] … placeholder:text-[var(--fc-ink3)]`).
- Task deep links → `taskDetailsPath()` (opens the Tasks console drawer);
  the old `/queues/:q/tasks/:id` route is only a redirect.

Both themes are first-class: check every change in light AND dark. If you
need a new color, add a token to `index.css` for both themes — don't inline.
