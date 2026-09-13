// Full-screen loading state for route-level lazy() boundaries (B-2).
// Kit classes only — spinner is a bare inline SVG (no emoji, no images).
export default function Loading({ label = 'Loading…' }: { label?: string }) {
  return (
    <div className="loading" role="status" aria-live="polite">
      <svg className="loading-spinner" viewBox="0 0 24 24" aria-hidden="true">
        <circle cx="12" cy="12" r="9" fill="none" stroke="currentColor" strokeWidth="2.5" opacity="0.2" />
        <path d="M21 12a9 9 0 0 0-9-9" fill="none" stroke="currentColor" strokeWidth="2.5" strokeLinecap="round" />
      </svg>
      <span className="muted">{label}</span>
    </div>
  );
}