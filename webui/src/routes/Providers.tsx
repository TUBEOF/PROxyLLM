import { FormEvent, useCallback, useMemo, useState } from 'react';
import { apiCall } from '../api';
import type { Provider, ProvidersResponse } from '../types';
import { EmptyState, ErrorNote, PageHead, useAdminData, useMutation } from '../hooks';
import { Drawer } from '../components/Overlays';
import { IconPlus } from '../icons';

// Providers (plan §9): table + drawer editor. The admin API stores the
// config blob as raw JSON — the editor round-trips it through a textarea
// with client-side validation before PUT. The Codex OAuth flow UI (start/
// poll/callback) is Phase 2 Task 5 scope — this route shows the honest
// "not yet available" note rather than a dead button.

const PROVIDER_TYPES = [
  'custom-openai',
  'ollama-cloud',
  'codex-oauth',
] as const;

function statusOf(p: Provider): { label: string; cls: string } {
  // Honest status derivation: a provider row exists with a config → it is
  // configured; health states arrive with the live health gate (Phase 1
  // routing surface) and are not part of this table yet.
  const hasConfig = p.config !== null && p.config !== undefined && Object.keys(p.config as object).length > 0;
  if (!hasConfig) {
    return { label: 'No config', cls: 'paused' };
  }
  return { label: 'Configured', cls: 'info' };
}

function jsonOr(value: unknown, fallback: string): string {
  if (value === null || value === undefined) return fallback;
  try {
    return JSON.stringify(value, null, 2) ?? fallback;
  } catch {
    return fallback;
  }
}

export default function Providers() {
  const { data, loading, error, reload } = useAdminData<ProvidersResponse>(
    (signal) => apiCall<ProvidersResponse>('GET', '/api/admin/providers', undefined, signal),
  );
  const providers = useMemo(() => data?.providers ?? [], [data]);

  const [editing, setEditing] = useState<Provider | null>(null);
  const [creating, setCreating] = useState(false);
  const [form, setForm] = useState({ id: '', type: 'custom-openai', name: '', config: '{}' });
  const [formError, setFormError] = useState<string | null>(null);

  const save = useMutation();
  const del = useMutation();

  const openCreate = useCallback(() => {
    setForm({ id: '', type: 'custom-openai', name: '', config: '{}' });
    setCreating(true);
    setEditing(null);
  }, []);

  const openEdit = useCallback((p: Provider) => {
    setForm({ id: p.id, type: p.type, name: p.name, config: jsonOr(p.config, '{}') });
    setEditing(p);
    setCreating(false);
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
      let configBlob: unknown;
      try {
        configBlob = form.config.trim() === '' ? undefined : JSON.parse(form.config);
      } catch {
        setFormError('Config is not valid JSON');
        return;
      }
      const id = creating ? form.id.trim() : form.id;
      if (id === '') {
        setFormError('ID is required');
        return;
      }
      setFormError(null);
      const ok = await save.run(() =>
        apiCall('PUT', `/api/admin/providers/${encodeURIComponent(id)}`, {
          type: form.type,
          name: form.name,
          config: configBlob,
        }),
      );
      if (ok) {
        closeDrawer();
        reload();
      }
    },
    [form, creating, save, closeDrawer, reload],
  );

  const remove = useCallback(
    async (p: Provider) => {
      const ok = await del.run(() => apiCall('DELETE', `/api/admin/providers/${encodeURIComponent(p.id)}`));
      if (ok) reload();
    },
    [del, reload],
  );

  const drawerError = formError ?? save.error ?? del.error;
  const drawerPending = save.pending || del.pending;

  return (
    <>
      <PageHead
        title="Providers"
        sub="Upstream providers this proxy can route to. Configs are stored encrypted at rest; secrets never come back to the browser."
        actions={
          <button type="button" className="btn btn-primary" onClick={openCreate}>
            <IconPlus width="16" height="16" />
            Add provider
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
            <EmptyState title="Loading providers" body="Fetching the provider list." />
          </div>
        ) : providers.length === 0 ? (
          <div className="w-full">
            <EmptyState
              title="No providers configured"
              body="Add a provider so the proxy has an upstream to route requests to. Custom OpenAI-compatible endpoints need a base URL in their config."
              action={
                <button type="button" className="btn btn-primary btn-sm" onClick={openCreate}>
                  <IconPlus width="14" height="14" />
                  Add the first provider
                </button>
              }
            />
          </div>
        ) : (
          <div className="w-full">
            <div className="table-wrap">
              <div className="table-scroll">
                <table className="table">
                  <thead>
                    <tr>
                      <th>Name</th>
                      <th>ID</th>
                      <th>Type</th>
                      <th>Status</th>
                      <th />
                    </tr>
                  </thead>
                  <tbody>
                    {providers.map((p) => {
                      const st = statusOf(p);
                      return (
                        <tr key={p.id} onClick={() => openEdit(p)}>
                          <td className="primary">{p.name}</td>
                          <td className="mono">{p.id}</td>
                          <td>{p.type}</td>
                          <td>
                            <span className={`badge ${st.cls}`}>
                              <span className="b-dot" />
                              {st.label}
                            </span>
                          </td>
                          <td>
                            <span className="row-actions">
                              <button
                                type="button"
                                className="btn btn-danger btn-sm"
                                onClick={(e) => {
                                  e.stopPropagation();
                                  void remove(p);
                                }}
                              >
                                Delete
                              </button>
                            </span>
                          </td>
                        </tr>
                      );
                    })}
                  </tbody>
                </table>
              </div>
            </div>
          </div>
        )}
      </div>

      {(editing !== null || creating) && (
        <Drawer
          title={creating ? 'Add provider' : `Edit ${editing?.name}`}
          onClose={closeDrawer}
          footer={
            <>
              {!creating && editing !== null && (
                <button
                  type="button"
                  className="btn btn-danger"
                  onClick={() => void remove(editing)}
                  disabled={drawerPending}
                >
                  Delete
                </button>
              )}
              <button type="button" className="btn btn-secondary" onClick={closeDrawer} disabled={drawerPending}>
                Cancel
              </button>
              <button type="submit" className="btn btn-primary" form="provider-form" disabled={drawerPending}>
                Save
              </button>
            </>
          }
        >
          <form className="form" id="provider-form" onSubmit={submit}>
            {drawerError !== null && (
              <p role="alert" style={{ color: 'var(--status-down)', fontSize: 'var(--fs-sm)', margin: 0 }}>
                {drawerError}
              </p>
            )}
            <div className="form-row">
              <div className="field">
                <label htmlFor="p-id">ID</label>
                <input
                  id="p-id"
                  className="input mono"
                  type="text"
                  value={form.id}
                  onChange={(e) => setForm((f) => ({ ...f, id: e.target.value }))}
                  disabled={!creating}
                  required
                  placeholder="e.g. ollama-cloud"
                />
                <span className="hint">Stable identifier used by models and routing rules.</span>
              </div>
              <div className="field has-select">
                <label htmlFor="p-type">Type</label>
                <select
                  id="p-type"
                  value={form.type}
                  onChange={(e) => setForm((f) => ({ ...f, type: e.target.value }))}
                >
                  {PROVIDER_TYPES.map((t) => (
                    <option key={t} value={t}>
                      {t}
                    </option>
                  ))}
                </select>
                <span className="hint">codex-oauth sign-in flow arrives in the next milestone.</span>
              </div>
            </div>
            <div className="field">
              <label htmlFor="p-name">Name</label>
              <input
                id="p-name"
                className="input"
                type="text"
                value={form.name}
                onChange={(e) => setForm((f) => ({ ...f, name: e.target.value }))}
                required
                placeholder="Display name"
              />
            </div>
            <div className="field">
              <label htmlFor="p-config">Config (JSON)</label>
              <textarea
                id="p-config"
                className="textarea mono"
                rows={8}
                value={form.config}
                onChange={(e) => setForm((f) => ({ ...f, config: e.target.value }))}
                spellCheck={false}
              />
              <span className="hint">
                Type-specific settings (e.g. base_url for custom-openai). Values are stored encrypted at rest.
                Secret-shaped keys are rejected server-side.
              </span>
            </div>
          </form>
        </Drawer>
      )}
    </>
  );
}