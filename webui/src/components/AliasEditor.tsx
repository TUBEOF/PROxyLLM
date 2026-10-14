import { useMemo, useState } from 'react';

// AliasEditor: the interactive model alias map — rows of
// alias → model-target pairs instead of the raw JSON textarea. Every alias
// is one row: a text input (the alias clients send, e.g. "sonnet") and a
// model picker (the registered model it routes to) with a free-text escape
// for targets the model list does not know yet (a stored id of a deleted
// row must stay correctable, never hidden — the honest-data rule).
//
// The contract rides the same settings KV ('model_aliases', JSON object
// alias → model-id); only the editor shape changed. Duplicate empty rows
// are tolerated locally; the onChange payload is always the sanitized map
// (last duplicate wins, matching JSON.parse-of-object semantics).

export interface AliasEntry {
  alias: string;
  target: string;
}

interface AliasEditorProps {
  // The sanitized alias→target map (the settings KV's parsed value).
  value: Record<string, string>;
  onChange: (next: Record<string, string>) => void;
  // The registered models the target picker offers (id + label).
  options: { id: string; label: string }[];
  // Pending flag disabling the controls while a save is in flight.
  disabled?: boolean;
}

// freePick marks a target value that is not among options — rendered as an
// editable text field until the operator picks a registered model.
function isFreePick(target: string, ids: Set<string>): boolean {
  return target !== '' && !ids.has(target);
}

export default function AliasEditor({ value, onChange, options, disabled = false }: AliasEditorProps) {
  const ids = useMemo(() => new Set(options.map((o) => o.id)), [options]);
  const rows = useMemo(() => Object.entries(value).map(([alias, target]) => ({ alias, target })), [value]);
  // draft holds the row being typed (a NEW alias not yet committed to value).
  // Committing on-blur/enter keeps onChange free of half-typed aliases.
  const [draftAlias, setDraftAlias] = useState('');
  const [draftTarget, setDraftTarget] = useState('');

  const commitDraft = () => {
    const alias = draftAlias.trim();
    if (alias === '' || draftTarget === '') return;
    onChange({ ...value, [alias]: draftTarget });
    setDraftAlias('');
    setDraftTarget('');
  };

  const setEntry = (alias: string, target: string) => {
    if (target === '') {
      // An emptied target removes the alias (an entry that routes nowhere
      // must not persist).
      const next = { ...value };
      delete next[alias];
      onChange(next);
      return;
    }
    onChange({ ...value, [alias]: target });
  };

  const removeEntry = (alias: string) => {
    const next = { ...value };
    delete next[alias];
    onChange(next);
  };

  return (
    <div className="alias-editor" role="group" aria-label="Model aliases">
      {rows.length === 0 && draftTarget === '' && draftAlias === '' && (
        <p className="hint" style={{ margin: 0 }}>
          No aliases — client model names route as-is. An alias maps one
          client-requested name (e.g. <span className="mono">sonnet</span>) onto a registered model.
        </p>
      )}
      {rows.map(({ alias, target }) => {
        const free = isFreePick(target, ids);
        return (
          <div key={alias} className="alias-row">
            <input
              className="input mono alias-input"
              type="text"
              defaultValue={alias}
              disabled={disabled}
              onBlur={(e) => {
                const next = e.target.value.trim();
                if (next === '' || next === alias) {
                  e.target.value = alias;
                  return;
                }
                // Rename: re-key the entry (last wins — the same rule JSON
                // object semantics give).
                const nextMap = { ...value };
                delete nextMap[alias];
                nextMap[next] = target;
                onChange(nextMap);
              }}
              aria-label={`Alias name for ${alias}`}
            />
            {free ? (
              /* Free-pick escape: the stored target is not a registered
                 model — editable so the operator can fix or re-pick it. */
              <input
                className="input mono alias-input free-pick"
                type="text"
                defaultValue={target}
                disabled={disabled}
                onFocus={(e) => e.target.select()}
                onBlur={(e) => setEntry(alias, e.target.value.trim())}
                aria-label={`Target model for ${alias} (unregistered id, edit freely)`}
              />
            ) : (
              <select
                className="input alias-input"
                value={target}
                disabled={disabled}
                onChange={(e) => setEntry(alias, e.target.value)}
                aria-label={`Target model for ${alias}`}
              >
                <option value="">Pick a registered model…</option>
                {options.map((o) => (
                  <option key={o.id} value={o.id}>
                    {o.label}
                  </option>
                ))}
              </select>
            )}
            <button
              type="button"
              className="btn btn-icon btn-sm"
              disabled={disabled}
              onClick={() => removeEntry(alias)}
              aria-label={`Remove alias ${alias}`}
            >
              <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.2" aria-hidden="true">
                <path d="m6 6 12 12M18 6 6 18" />
              </svg>
            </button>
          </div>
        );
      })}
      {/* New row: alias text + model picker; committed as a pair. */}
      <div className="alias-row alias-row-new">
        <input
          className="input mono alias-input"
          type="text"
          value={draftAlias}
          disabled={disabled}
          placeholder="alias (e.g. sonnet)"
          spellCheck={false}
          onChange={(e) => setDraftAlias(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === 'Enter') {
              e.preventDefault();
              commitDraft();
            }
          }}
          aria-label="New alias name"
        />
        <select
          className="input alias-input"
          value={draftTarget}
          disabled={disabled}
          onChange={(e) => setDraftTarget(e.target.value)}
          aria-label="New alias target model"
        >
          <option value="">Pick a model…</option>
          {options.map((o) => (
            <option key={o.id} value={o.id}>
              {o.label}
            </option>
          ))}
        </select>
        <button
          type="button"
          className="btn btn-secondary btn-sm"
          disabled={disabled || draftAlias.trim() === '' || draftTarget === ''}
          onClick={commitDraft}
        >
          Add
        </button>
      </div>
      {rows.some(({ target }) => isFreePick(target, ids)) && (
        <p className="hint" style={{ margin: 0 }}>
          Targets shown as text fields are not registered models — pick a model or fix the id.
        </p>
      )}
    </div>
  );
}