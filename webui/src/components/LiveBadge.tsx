// LiveBadge (2026-10-08): the one live indicator every data page uses.
// A green dot with the kit's pulsing ring while the page is receiving
// fresh data, degrading to a quiet "PAUSED" when it is not.
//
// Two feeds drive it, because the two kinds of page are live in different
// ways: a polled page is live while its last fetch succeeded and is recent
// (`updatedAt`), while the Logs page is live only while its SSE stream is
// actually connected (`connected`) — a healthy poll there still means the
// operator is looking at data up to an interval old. Every page therefore
// shows the same badge with the same meaning: "what you see is current".
import { useEffect, useState } from 'react';

export type LiveBadgeProps = {
  /** Timestamp (ms) of the last successful fetch; null before the first. */
  updatedAt?: number | null;
  /** SSE connected state, when the page has a stream (overrides updatedAt). */
  connected?: boolean;
  /** Polling interval (ms): the staleness window before the badge pauses. */
  intervalMs?: number;
};

// staleAfter allows one missed poll plus slack before a polled page stops
// claiming to be live — a badge that stays green through a dead loop would
// be worse than no badge.
const STALE_SLACK_MS = 2500;

function ago(updatedAt: number | null | undefined): string {
  if (updatedAt === null || updatedAt === undefined) return 'never updated';
  const secs = Math.max(Math.round((Date.now() - updatedAt) / 1000), 0);
  if (secs < 2) return 'updated just now';
  if (secs < 60) return `updated ${secs}s ago`;
  return `updated ${Math.round(secs / 60)}m ago`;
}

export function LiveBadge({ updatedAt = null, connected, intervalMs = 0 }: LiveBadgeProps) {
  // now ticks so a page whose poll has silently died flips to PAUSED
  // instead of showing a stale "LIVE" forever. The tick only exists for
  // that staleness check — on a stream-fed page `connected` decides the
  // state alone, so the 1 Hz re-render would be pure waste.
  const streamed = typeof connected === 'boolean';
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    if (streamed) return;
    const t = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(t);
  }, [streamed]);
  const stale =
    !streamed &&
    (updatedAt === null ||
      now - updatedAt > intervalMs + STALE_SLACK_MS);
  const live = streamed ? connected === true : !stale;

  return (
    <span
      className={`badge ${live ? 'up' : 'paused'}`}
      title={streamed ? 'Live stream of new requests' : ago(updatedAt)}
    >
      <span className={`status-dot ${live ? 'up' : 'paused'}`} />
      {live ? 'LIVE' : 'PAUSED'}
    </span>
  );
}
