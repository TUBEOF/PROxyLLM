import { FormEvent, useCallback, useEffect, useMemo, useState } from 'react';
import { apiCall } from '../api';
import type { SettingsEntry, SettingsResponse } from '../types';
import { EmptyState, ErrorNote, PageHead, useAdminData, useMutation } from '../hooks';
import { Drawer } from '../components/Overlays';
import { IconPlus } from '../icons';

// Settings (plan §9): the config-KV override layer (Analysis C top layer —
// DB overrides sit above defaults/file/ENV). Everything the proxy reads as
// a runtime override lives here as key/value pairs. Two keys get structured
// editors instead of freeform rows because they carry fixed value sets:
//  - anthropic_surface_non_claude: allow | warn | block (§6.4 global switch)
//  - model_aliases: JSON object mapping Claude Code aliases → model ids (C6)
// Everything else is a plain string KV pair edited in a drawer.

const NON_CLAUDE_VALUES = ['allow', 'warn', 'block'] as const;
const NON_CLAUDE_KEY = 'anthropic_surface_non_claude';
const ALIASES_KEY = 'model_aliases';

export default function Settings() {
  const { data, loading, error, reload } = useAdminData<SettingsResponse>(
    (signal) => apiCall<SettingsResponse>('GET', '/api/admin/settings', undefined, signal),
  );
  const entries = useMemo(() => data?.entries ?? [], [data]);

  const [editing, setEditing] = useState<SettingsEntry | null>(null);
  const [creating, setCreating] = useState(false);
  const [formError, setFormError] = useState<string | null>(null);
  const [form, setForm] = useState({ key: '', value: '' });
  const [aliasesText, setAliasesText] = useState('');
  const [aliasesError, setAliasesError] = useState<string | null>(null);

  const save = useMutation();
  const del = useMutation();
  const saveAlias = useMutation();
  const saveSwitch = useMutation();

  const valueOf = useCallback((key: string): string => entries.find((e) => e.key === key)?.value ?? '', [entries]);
  const nonClaudeValue = useMemo(() => {
    const raw = valueOf(NON_CLAUDE_KEY);
    return (NON_CLAUDE_VALUES as readonly string[]).includes(raw) ? raw : '';
  }, [valueOf]);
  const isDefault = nonClaudeValue === '';

  // Seed the alias textarea from the loaded override once per load — an
  // accidental save must not clobber an existing map with an empty object.
  useEffect(() => {
    setAliasesText(entries.find((e) => e.key === ALIASES_KEY)?.value ?? '');
  }, [entries]);

  const openCreate = useCallback(() => {
    setForm({ key: '', value: '' });
    setCreating(true);
    setEditing(null);
    setFormError(null);
  }, []);

  const openEdit = useCallback((e: SettingsEntry) => {
    setForm({ key: e.key, value: e.value });
    setEditing(e);
    setCreating(false);
    setFormError(null);
  }, []);

  const closeDrawer = useCallback(() => {
    setEditing(null);
    setCreating(false);
    setFormError(null);
    save.clearError();
  }, [save]);

  const submit = useCallback(
    async (e: FormEvent) => {
      e.preventDefault();
      const key = creating ? form.key.trim() : form.key;
      if (key === '') {
        setFormError('Key is required');
        return;
      }
      setFormError(null);
      const ok = await save.run(() => apiCall('PUT', '/api/admin/settings', { key, value: form.value }));
      if (ok !== null) {
        closeDrawer();
        reload();
      }
    },
    [form, creating, save, closeDrawer, reload],
  );

  const remove = useCallback(
    async (e: SettingsEntry) => {
      const ok = await del.run(() => apiCall('DELETE', `/api/admin/settings/${encodeURIComponent(e.key)}`));
      if (ok !== null) reload();
    },
    [del, reload],
  );

  const setNonClaude = useCallback(
    async (value: string) => {
      const ok = await saveSwitch.run(() => apiCall('PUT', '/api/admin/settings', { key: NON_CLAUDE_KEY, value }));
      if (ok !== null) reload();
    },
    [saveSwitch, reload],
  );

  const submitAliases = useCallback(
    async (e: FormEvent) => {
      e.preventDefault();
      let parsed: unknown;
      try {
        parsed = aliasesText.trim() === '' ? {} : JSON.parse(aliasesText);
      } catch {
        setAliasesError('Alias map is not valid JSON');
        return;
      }
      if (parsed === null || typeof parsed !== 'object' || Array.isArray(parsed)) {
        setAliasesError('Alias map must be a JSON object of alias → model id');
        return;
      }
      for (const [alias, target] of Object.entries(parsed as Record<string, unknown>)) {
        if (typeof target !== 'string' || target === '') {
          setAliasesError(`Target for "${alias}" must be a non-empty model id string`);
          return;
        }
      }
      setAliasesError(null);
      const ok = await saveAlias.run(() =>
        apiCall('PUT', '/api/admin/settings', { key: ALIASES_KEY, value: JSON.stringify(parsed) }),
      );
      if (ok !== null) reload();
    },
    [aliasesText, saveAlias, reload],
  );

  const drawerError = formError ?? save.error ?? del.error;

  return (
    <>
      <PageHead
        title="Settings"
        sub="Runtime config overrides (defaults → file → ENV → this layer). Deleting a key falls back to the next layer down."
        actions={
          <button type="button" className="btn btn-primary" onClick={openCreate}>
            <IconPlus width="16" height="16" />
            Add key
          </button>
        }
      />
      <div className="canvas">
        {error !== null ? (
          <div className="w-full">
            <ErrorNote message={error} />
          </div>
        ) : loading ? (
          <div className="w-full">
            <EmptyState title="Loading settings" body="Fetching the config layer." />
          </div>
        ) : (
          <div className="w-full">
            <div className="widget w-full">
              <div className="widget-head">
                <h3 className="title">Non-Claude routing on the Anthropic surface</h3>
              </div>
              <div className="widget-body">
                <p className="hint" style={{ marginTop: 0 }}>
                  Global switch for routing Claude Code to non-Claude models (§6.4). The per-model compatibility policy
                  in Models &amp; Compatibility governs the details; this is the safety net.
                </p>
                <div className="rpills" role="radiogroup" aria-label="Non-Claude routing mode">
                  {NON_CLAUDE_VALUES.map((mode) => (
                    <button
                      key={mode}
                      type="button"
                      className={`rpill${(!isDefault && nonClaudeValue === mode) || (isDefault && mode === 'allow') ? ' active' : ''}`}
                      role="radio"
                      aria-checked={(!isDefault && nonClaudeValue === mode) || (isDefault && mode === 'allow')}
                      onClick={() => void setNonClaude(mode)}
                      disabled={saveSwitch.pending}
                    >
                      {mode}
                    </button>
                  ))}
                </div>
                <p className="hint">
                  {isDefault
                    ? 'No override set — the default is allow.'
                    : `Override: ${nonClaudeValue}. allow passes requests through; warn annotates the route trace; block rejects with a clear error.`}
                </p>
                {saveSwitch.error !== null && <ErrorNote message={saveSwitch.error} />}
              </div>
            </div>

            <div className="widget w-full">
              <div className="widget-head">
                <h3 className="title">Claude Code alias map</h3>
              </div>
              <div className="widget-body">
                <p className="hint" style={{ marginTop: 0 }}>
                  Maps Claude Code model aliases (C6: ANTHROPIC_MODEL, ANTHROPIC_DEFAULT_*_MODEL) to registered model
                  ids. Routing rules can also match an alias directly.
                </p>
                <form className="form" onSubmit={submitAliases}>
                  <div className="field">
                    <label htmlFor="s-aliases">Aliases (JSON object)</label>
                    <textarea
                      id="s-aliases"
                      className="textarea mono"
                      rows={5}
                      value={aliasesText}
                      onChange={(e) => setAliasesText(e.target.value)}
                      placeholder={'{"haiku": "gpt-5-mini", "sonnet": "llama-4-sonnet"}'}
                      spellCheck={false}
                    />
                    <span className="hint">
                      Loaded override:{' '}
                      {valueOf(ALIASES_KEY) === '' ? 'none — set the object above and save.' : valueOf(ALIASES_KEY)}
                    </span>
                  </div>
                  {aliasesError !== null && (
                    <p role="alert" style={{ color: 'var(--status-down)', fontSize: 'var(--fs-sm)', margin: 0 }}>
                      {aliasesError}
                    </p>
                  )}
                  {saveAlias.error !== null && (
                    <p role="alert" style={{ color: 'var(--status-down)', fontSize: 'var(--fs-sm)', margin: 0 }}>
                      {saveAlias.error}
                    </p>
                  )}
                  <button type="submit" className="btn btn-primary btn-sm" disabled={saveAlias.pending}>
                    Save alias map
                  </button>
                </form>
              </div>
            </div>

            <div className="widget w-full">
              <div className="widget-head">
                <h3 className="title">Config overrides</h3>
              </div>
              <div className="widget-body">
                {entries.length === 0 ? (
                  <EmptyState
                    title="No overrides set"
                    body="Every key is falling back to defaults, config file, or ENV. Add a key here to override the layer below."
                    action={
                      <button type="button" className="btn btn-primary btn-sm" onClick={openCreate}>
                        <IconPlus width="14" height="14" />
                        Add the first key
                      </button>
                    }
                  />
                ) : (
                  <div className="table-wrap">
                    <div className="table-scroll">
                      <table className="table">
                        <thead>
                          <tr>
                            <th>Key</th>
                            <th>Value</th>
                            <th />
                          </tr>
                        </thead>
                        <tbody>
                          {entries.map((e) => (
                            <tr key={e.key} onClick={() => openEdit(e)}>
                              <td className="primary mono">{e.key}</td>
                              <td className="mono">{e.value}</td>
                              <td>
                                <span className="row-actions">
                                  <button
                                    type="button"
                                    className="btn btn-danger btn-sm"
                                    onClick={(ev) => {
                                      ev.stopPropagation();
                                      void remove(e);
                                    }}
                                  >
                                    Delete
                                  </button>
                                </span>
                              </td>
                            </tr>
                          ))}
                        </tbody>
                      </table>
                    </div>
                  </div>
                )}
                {del.error !== null && <ErrorNote message={del.error} />}
              </div>
            </div>
          </div>
        )}
      </div>

      {(editing !== null || creating) && (
        <Drawer
          title={creating ? 'Add config key' : `Edit ${editing?.key}`}
          onClose={closeDrawer}
          footer={
            <>
              {!creating && editing !== null && (
                <button
                  type="button"
                  className="btn btn-danger"
                  onClick={() => void remove(editing)}
                  disabled={save.pending || del.pending}
                >
                  Delete
                </button>
              )}
              <button type="button" className="btn btn-secondary" onClick={closeDrawer} disabled={save.pending || del.pending}>
                Cancel
              </button>
              <button type="submit" className="btn btn-primary" form="setting-form" disabled={save.pending || del.pending}>
                Save
              </button>
            </>
          }
        >
          <form className="form" id="setting-form" onSubmit={submit}>
            {drawerError !== null && (
              <p role="alert" style={{ color: 'var(--status-down)', fontSize: 'var(--fs-sm)', margin: 0 }}>
                {drawerError}
              </p>
            )}
            <div className="field">
              <label htmlFor="s-key">Key</label>
              <input
                id="s-key"
                className="input mono"
                type="text"
                value={form.key}
                onChange={(e) => setForm((f) => ({ ...f, key: e.target.value }))}
                disabled={!creating}
                required
                placeholder="e.g. retention_days"
              />
              <span className="hint">Stable config key the proxy reads at runtime.</span>
            </div>
            <div className="field">
              <label htmlFor="s-value">Value</label>
              <textarea
                id="s-value"
                className="textarea mono"
                rows={4}
                value={form.value}
                onChange={(e) => setForm((f) => ({ ...f, value: e.target.value }))}
                spellCheck={false}
              />
              <span className="hint">
                Stored as a string — JSON values for structured settings (e.g. the alias map) are pasted as JSON text.
              </span>
            </div>
          </form>
        </Drawer>
      )}
    </>
  );
}