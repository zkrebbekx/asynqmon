import { MicroLabel } from "../components/FleetBits";

export default function PageNotFoundView() {
  return (
    <div className="flex h-full min-h-[400px] flex-col items-center justify-center">
      <MicroLabel>page not found</MicroLabel>
      <div className="mt-1 font-mono text-5xl font-semibold tabular-nums text-[var(--fc-ink)]">
        404
      </div>
      <p className="mt-2 text-sm text-[var(--fc-ink2)]">
        Nothing lives at this address — check the URL or use the nav.
      </p>
    </div>
  );
}
