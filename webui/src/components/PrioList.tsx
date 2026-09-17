import { useState } from 'react';

// PrioList (ADR-010): an ordered string list with native HTML5 drag-drop
// reordering plus keyboard/pointer fallbacks (↑/↓/✕ buttons — the same
// affordances the routing chain editor already used, so the ordering stays
// reachable without a pointer). No drag-drop library: the bundle budget
// (≤200 KB gzip initial load) forbids one, and HTML5 DnD covers reorder
// within one list natively.
//
// The component is fully controlled: the parent owns `items` and receives
// the reordered array (plus add/remove) through callbacks.

interface PrioListProps {
  items: string[];
  onChange: (items: string[]) => void;
  // Accessible name for the list region.
  label: string;
  // Empty-state hint when no entries are configured.
  emptyHint?: string;
  // ARIA label of the add select's first option; defaults to "Pick an entry…".
  addLabel?: string;
  // Available entries the picker offers (id + display label); entries
  // already on the list are filtered out.
  options: { id: string; label: string }[];
}

export default function PrioList({ items, onChange, label, emptyHint, addLabel, options }: PrioListProps) {
  const [dragIndex, setDragIndex] = useState<number | null>(null);
  const [overIndex, setOverIndex] = useState<number | null>(null);

  const move = (from: number, to: number) => {
    if (to < 0 || to >= items.length || from === to) return;
    const next = [...items];
    const [moved] = next.splice(from, 1);
    if (moved === undefined) return;
    next.splice(to, 0, moved);
    onChange(next);
  };

  const remove = (index: number) => {
    onChange(items.filter((_, i) => i !== index));
  };

  const add = (id: string) => {
    if (id === '' || items.includes(id)) return;
    onChange([...items, id]);
  };

  // Drag-drop reorder: dropping on index i moves the dragged row there.
  const onDrop = (index: number) => {
    if (dragIndex !== null) move(dragIndex, index);
    setDragIndex(null);
    setOverIndex(null);
  };

  const available = options.filter((o) => !items.includes(o.id));

  return (
    <div
      role="group"
      aria-label={label}
      style={{ display: 'flex', flexDirection: 'column', gap: 6 }}
      onDragEnd={() => {
        setDragIndex(null);
        setOverIndex(null);
      }}
    >
      {items.length === 0 ? (
        <p className="hint" style={{ margin: 0 }}>
          {emptyHint ?? 'No entries configured — the fallback is inactive.'}
        </p>
      ) : (
        items.map((entry, i) => (
          <div
            key={`${entry}-${i}`}
            draggable
            onDragStart={() => setDragIndex(i)}
            onDragOver={(e) => {
              e.preventDefault();
              setOverIndex(i);
            }}
            onDrop={(e) => {
              e.preventDefault();
              onDrop(i);
            }}
            style={{
              display: 'flex',
              alignItems: 'center',
              gap: 8,
              fontSize: 'var(--fs-sm)',
              padding: '4px 6px',
              borderRadius: 6,
              background: overIndex === i && dragIndex !== null && dragIndex !== i
                ? 'var(--bg-elev, rgba(127,127,127,0.12))'
                : undefined,
              cursor: 'grab',
            }}
          >
            <span aria-hidden="true" style={{ opacity: 0.5, fontSize: 'var(--fs-xs)' }}>⠿</span>
            <span className="mono">{`${i + 1}. ${entry}`}</span>
            <span style={{ marginLeft: 'auto', display: 'inline-flex', gap: 4 }}>
              <button
                type="button"
                className="btn btn-icon btn-sm"
                onClick={() => move(i, i - 1)}
                disabled={i === 0}
                aria-label={`Move ${entry} up`}
              >
                <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.2" aria-hidden="true">
                  <path d="m6 14 6-6 6 6" />
                </svg>
              </button>
              <button
                type="button"
                className="btn btn-icon btn-sm"
                onClick={() => move(i, i + 1)}
                disabled={i === items.length - 1}
                aria-label={`Move ${entry} down`}
              >
                <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.2" aria-hidden="true">
                  <path d="m6 10 6 6 6-6" />
                </svg>
              </button>
              <button
                type="button"
                className="btn btn-icon btn-sm"
                onClick={() => remove(i)}
                aria-label={`Remove ${entry}`}
              >
                <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.2" aria-hidden="true">
                  <path d="m6 6 12 12M18 6 6 18" />
                </svg>
              </button>
            </span>
          </div>
        ))
      )}
      <div className="field has-select" style={{ marginTop: 4 }}>
        <select
          aria-label={addLabel ?? 'Add entry'}
          value=""
          onChange={(e) => {
            add(e.target.value);
            e.target.value = '';
          }}
        >
          <option value="">{addLabel ?? 'Pick an entry…'}</option>
          {available.map((o) => (
            <option key={o.id} value={o.id}>
              {o.label}
            </option>
          ))}
        </select>
      </div>
    </div>
  );
}