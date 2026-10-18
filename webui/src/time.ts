// Time formatting for the console. One function per question, free of React
// and the DOM: the e2e specs stub the API, and these derivations stay
// ordinary functions.

// formatCountdown renders a duration in the terms an operator reads limits
// in: a session window is hours, a weekly window days — never "169h".
export function formatCountdown(secs: number): string {
  if (secs < 60) return '<1m';
  const m = Math.floor(secs / 60);
  if (m < 60) return `${m}m`;
  const h = Math.floor(m / 60);
  const remMin = m % 60;
  if (h < 24) return remMin > 0 ? `${h}h ${remMin}m` : `${h}h`;
  const d = Math.floor(h / 24);
  const remHours = h % 24;
  return remHours > 0 ? `${d}d ${remHours}h` : `${d}d`;
}

// formatDateTime renders an instant in the BROWSER's locale and timezone:
// the exact date and time the reader can hold against their own clock. The
// zone abbreviation rides the value itself (timeZoneName), so nobody has to
// wonder which clock it is.
export function formatDateTime(unixSeconds: number): string {
  return new Date(unixSeconds * 1000).toLocaleString(undefined, {
    year: 'numeric',
    month: 'short',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
    timeZoneName: 'short',
  });
}

// formatResetInstant is formatDateTime for a limit window's reset — the same
// reading, named for the call site that renders a tooltip.
export function formatResetInstant(unixSeconds: number): string {
  return formatDateTime(unixSeconds);
}

// relativeAge renders a past instant as an age ("2h ago"), the same reading
// the LiveBadge uses for staleness.
export function relativeAge(unixSeconds: number, nowMs: number): string {
  const secs = Math.max(0, Math.floor(nowMs / 1000) - unixSeconds);
  if (secs < 60) return 'just now';
  const m = Math.floor(secs / 60);
  if (m < 60) return `${m}m ago`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h}h ago`;
  return `${Math.floor(h / 24)}d ago`;
}
