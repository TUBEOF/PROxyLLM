import { useId } from 'react';
import { formatCountdown, formatResetInstant } from '../time';

// A limit window's remaining share as a horizontal bar. The bar shows what
// is LEFT and shrinks as the quota is consumed: green while there is room,
// orange in the last half, red under 20%. The three tones are discrete
// tokens (the design-rule hue windows forbid interpolating between them);
// the fill carries a same-hue alpha gradient for the soft edge.
export type UsageTone = 'up' | 'warn' | 'down' | 'none';

export function usageTone(pct: number | null): UsageTone {
  if (pct === null || Number.isNaN(pct)) return 'none';
  if (pct < 20) return 'down';
  if (pct < 50) return 'warn';
  return 'up';
}

export function ResetCountdown({
  reset,
  windowName,
  nowMs,
}: {
  reset: number;
  windowName: string;
  nowMs: number;
}) {
  const id = useId();
  const secs = reset - Math.floor(nowMs / 1000);
  return (
    <span
      className="tooltip hint usage-meter-reset-copy"
      tabIndex={0}
      aria-describedby={id}
    >
      {secs <= 0 ? 'resetting…' : `resets in ${formatCountdown(secs)}`}
      <span className="tt" role="tooltip" id={id}>
        {`${windowName} window resets ${formatResetInstant(reset)}`}
      </span>
    </span>
  );
}

export function UsageBar({
  label,
  pct,
  reading,
  resetAt,
  windowName,
  nowMs,
  unlimited = false,
  note,
}: {
  label: string;
  pct: number | null;
  reading: string;
  resetAt?: number;
  windowName: string;
  nowMs: number;
  unlimited?: boolean;
  note?: string;
}) {
  const tone = usageTone(pct);
  const rounded = pct === null ? null : Math.round(Math.max(0, Math.min(100, pct)));
  return (
    <div className="usage-meter" data-tone={tone}>
      <div className="usage-meter-head">
        <span className="usage-meter-label">{label}</span>
        <span className="usage-meter-value">{reading}</span>
      </div>
      {unlimited ? (
        <span className="badge up usage-meter-unlimited">unlimited</span>
      ) : (
        <div
          className="usage-meter-track"
          role={rounded === null ? undefined : 'meter'}
          aria-label={`${windowName} remaining`}
          aria-valuemin={rounded === null ? undefined : 0}
          aria-valuemax={rounded === null ? undefined : 100}
          aria-valuenow={rounded ?? undefined}
          aria-valuetext={rounded === null ? undefined : reading}
        >
          {rounded !== null && (
            <div className="usage-meter-fill" style={{ width: `${rounded}%` }} />
          )}
        </div>
      )}
      {note !== undefined && note !== '' && (
        <span className="usage-meter-note">{note}</span>
      )}
      {resetAt !== undefined && resetAt > 0 && (
        <div className="usage-meter-reset">
          <ResetCountdown reset={resetAt} windowName={windowName} nowMs={nowMs} />
        </div>
      )}
    </div>
  );
}
