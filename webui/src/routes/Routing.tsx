import { FormEvent, useCallback, useMemo, useState } from 'react';
import { apiCall } from '../api';
import type { ModelsResponse, RoutingRule, RoutingStrategyResponse, RulesResponse, Strategy } from '../types';
import { EmptyState, ErrorNote, PageHead, useAdminData, useMutation } from '../hooks';
import { Drawer } from '../components/Overlays';
import { IconPlus } from '../icons';

// Routing (plan §9 + ADR-009): the global strategy (Round Robin default /
// Fill-First) and rules with per-rule strategy overrides. The admin API's
// rule model is match_model → chain ([]string of provider types, provider
// ids, or provider/model pairs) + priority + enabled + strategy; the chain
// editor is an ordered list the operator can append to and reorder, since
// the chain IS the fallback order (Analysis E). Type entries expand to all
// accounts of that type at request time — new accounts join without rule
// edits. The alias map and the global allow/warn/block switch ride the
// settings surface (config KV) — they live on /settings.

const STRATEGIES: { value: Strategy; label: string; desc: string }[] = [
  { value: 'round_robin', label: 'Round Robin', desc: 'Requests rotate across all accounts of a type — even load distribution.' },
  { value: 'fill_first', label: 'Fill-First', desc: 'Account 1 serves everything until it fails (rate limit / breaker) — then the next takes over.' },
];

export default function Routing() {
  const { data, loading, error, reload } = useAdminData<RulesResponse>(
    (signal) => apiCall<RulesResponse>('GET', '/api/admin/routing', undefined, signal),
  );
  const { data: modelsData } = useAdminData<ModelsResponse>(
    (signal) => apiCall<ModelsResponse>('GET', '/api/admin/models', undefined, signal),
  );
  const { data: strategyData, reload: reloadStrategy } = useAdminData<RoutingStrategyResponse>(
    (signal) => apiCall<RoutingStrategyResponse>('GET', '/api/admin/routing-strategy', undefined, signal),
  );
  const rules = useMemo(() => data?.rules ?? [], [data]);
  const models = useMemo(() => modelsData?.models ?? [], [modelsData]);
  const modelIDs = useMemo(() => models.map((m) => m.id), [models]);
  const globalStrategy = strategyData?.strategy ?? 'round_robin';

  const [editing, setEditing] = useState<RoutingRule | null>(null);
  const [creating, setCreating] = useState(false);
  const [formError, setFormError] = useState<string | null>(null);
  const [form, setForm] = useState({
    id: '',
    match_model: '',
    chain: [] as string[],
    priority: '0',
    enabled: true,
    strategy: '',
  });

  const save = useMutation();
  const del = useMutation();
  const saveGlobal = useMutation();

  const setGlobalStrategy = useCallback(
    async (value: Strategy) => {
      const ok = await saveGlobal.run(() =>
        apiCall('PUT', '/api/admin/routing-strategy', { strategy: value }),
      );
      if (ok !== null) reloadStrategy();
    },
    [saveGlobal, reloadStrategy],
  );

  const openCreate = useCallback(() => {
    setForm({ id: '', match_model: '', chain: [], priority: '0', enabled: true, strategy: '' });
    setCreating(true);
    setEditing(null);
    setFormError(null);
  }, []);

  const openEdit = useCallback((rule: RoutingRule) => {
    setForm({
      id: rule.id,
      match_model: rule.match_model,
      chain: [...rule.chain],
      priority: String(rule.priority),
      enabled: rule.enabled,
      strategy: rule.strategy ?? '',
    });
    setEditing(rule);
    setCreating(false);
    setFormError(null);
  }, []);

  const closeDrawer = useCallback(() => {
    setEditing(null);
    setCreating(false);
    setFormError(null);
    save.clearError();
  }, [save]);

  const appendChain = useCallback(
    (modelID: string) => {
      if (modelID === '') return;
      setForm((f) => (f.chain.includes(modelID) ? f : { ...f, chain: [...f.chain, modelID] }));
    },
    [],
  );

  const move = useCallback((index: number, dir: -1 | 1) => {
    setForm((f) => {
      const next = [...f.chain];
      const target = index + dir;
      if (target < 0 || target >= next.length) return f;
      const a = next[index];
      const b = next[target];
      if (a === undefined || b === undefined) return f;
      next[index] = b;
      next[target] = a;
      return { ...f, chain: next };
    });
  }, []);

  const submit = useCallback(
    async (e: FormEvent) => {
      e.preventDefault();
      const id = creating ? form.id.trim() : form.id;
      if (id === '') {
        setFormError('ID is required');
        return;
      }
      if (form.match_model.trim() === '') {
        setFormError('Match pattern is required (exact name, wildcard, or alias)');
        return;
      }
      const priority = Number(form.priority);
      if (!Number.isInteger(priority) || priority < 0) {
        setFormError('Priority must be a non-negative integer');
        return;
      }
      setFormError(null);
      const ok = await save.run(() =>
        apiCall('PUT', `/api/admin/routing/${encodeURIComponent(id)}`, {
          match_model: form.match_model,
          chain: form.chain,
          priority,
          enabled: form.enabled,
          strategy: form.strategy,
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
    async (rule: RoutingRule) => {
      const ok = await del.run(() => apiCall('DELETE', `/api/admin/routing/${encodeURIComponent(rule.id)}`));
      if (ok) reload();
    },
    [del, reload],
  );

  const drawerError = formError ?? save.error ?? del.error;
  const drawerPending = save.pending || del.pending;

  return (
    <>
      <PageHead
        title="Routing"
        sub="Fallback chains per model pattern. Every transition at request time is traced in the logs — fallbacks are never silent."
        actions={
          <button type="button" className="btn btn-primary" onClick={openCreate}>
            <IconPlus width="16" height="16" />
            Add rule
          </button>
        }
      />
      <div className="canvas">
        {/* Global strategy (ADR-009): the default for every rule that does
            not pin its own. Rules can override per drawer below. Visible
            radios, not the .toggle switch kit (that hides its inputs). */}
        <div className="w-full" style={{ marginBottom: 8 }}>
          <fieldset className="fieldset">
            <legend>Strategy</legend>
          <div style={{ display: 'flex', flexDirection: 'column', gap: 10 }}>
            {STRATEGIES.map((s) => (
              <label key={s.value} style={{ display: 'flex', gap: 10, alignItems: 'flex-start', cursor: 'pointer' }}>
                <input
                  type="radio"
                  name="global-strategy"
                  checked={globalStrategy === s.value}
                  onChange={() => void setGlobalStrategy(s.value)}
                  disabled={saveGlobal.pending}
                  style={{ marginTop: 3 }}
                />
                <span>
                  <strong>{s.label}</strong>
                  <br />
                  <span className="hint">{s.desc}</span>
                </span>
              </label>
            ))}
            {saveGlobal.error !== null && (
              <p role="alert" style={{ color: 'var(--status-down)', fontSize: 'var(--fs-sm)', margin: 0 }}>
                {saveGlobal.error}
              </p>
            )}
          </div>
          <span className="hint" style={{ marginTop: 8, display: 'block' }}>
            The global default applies to every rule without its own strategy. Per-rule
            overrides are set in each rule.
          </span>
          </fieldset>
        </div>
        {error !== null ? (
          <div className="w-full">
            <ErrorNote message={error} />
          </div>
        ) : loading ? (
          <div className="w-full">
            <EmptyState title="Loading routing rules" body="Fetching the rule list." />
          </div>
        ) : rules.length === 0 ? (
          <div className="w-full">
            <EmptyState
              title="No routing rules"
              body="A rule matches a model pattern and holds the ordered fallback chain. Without rules, no request can be routed — add one pointing at a registered model."
              action={
                <button type="button" className="btn btn-primary btn-sm" onClick={openCreate}>
                  <IconPlus width="14" height="14" />
                  Add the first rule
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
                      <th>Match</th>
                      <th>Chain</th>
                      <th className="num">Priority</th>
                      <th>Strategy</th>
                      <th>Status</th>
                      <th />
                    </tr>
                  </thead>
                  <tbody>
                    {rules.map((rule) => (
                      <tr key={rule.id} onClick={() => openEdit(rule)}>
                        <td>
                          <span className="primary">{rule.match_model}</span>
                          <br />
                          <span className="mono">{rule.id}</span>
                        </td>
                        <td>
                          {rule.chain.length === 0 ? (
                            <span className="badge down">empty chain</span>
                          ) : (
                            <span className="mono">{rule.chain.join(' → ')}</span>
                          )}
                        </td>
                        <td className="num">{rule.priority}</td>
                        <td>
                          {rule.strategy === '' ? (
                            <span className="badge info">
                              <span className="b-dot" />
                              inherits ({globalStrategy === 'round_robin' ? 'Round Robin' : 'Fill-First'})
                            </span>
                          ) : (
                            <span className="badge info">
                              <span className="b-dot" />
                              {rule.strategy === 'round_robin' ? 'Round Robin' : 'Fill-First'}
                            </span>
                          )}
                        </td>
                        <td>
                          {rule.enabled ? (
                            <span className="badge up">
                              <span className="b-dot" />
                              active
                            </span>
                          ) : (
                            <span className="badge paused">
                              <span className="b-dot" />
                              disabled
                            </span>
                          )}
                        </td>
                        <td>
                          <span className="row-actions">
                            <button
                              type="button"
                              className="btn btn-danger btn-sm"
                              onClick={(e) => {
                                e.stopPropagation();
                                void remove(rule);
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
          </div>
        )}
      </div>

      {(editing !== null || creating) && (
        <Drawer
          title={creating ? 'Add routing rule' : `Edit ${editing?.match_model}`}
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
              <button type="submit" className="btn btn-primary" form="routing-form" disabled={drawerPending}>
                Save
              </button>
            </>
          }
        >
          <form className="form" id="routing-form" onSubmit={submit}>
            {drawerError !== null && (
              <p role="alert" style={{ color: 'var(--status-down)', fontSize: 'var(--fs-sm)', margin: 0 }}>
                {drawerError}
              </p>
            )}
            <div className="form-row">
              <div className="field">
                <label htmlFor="r-id">ID</label>
                <input
                  id="r-id"
                  className="input mono"
                  type="text"
                  value={form.id}
                  onChange={(e) => setForm((f) => ({ ...f, id: e.target.value }))}
                  disabled={!creating}
                  required
                />
              </div>
              <div className="field">
                <label htmlFor="r-match">Match pattern</label>
                <input
                  id="r-match"
                  className="input mono"
                  type="text"
                  value={form.match_model}
                  onChange={(e) => setForm((f) => ({ ...f, match_model: e.target.value }))}
                  placeholder="exact-model or prefix*"
                  required
                />
                <span className="hint">Exact name, wildcard, or a Claude Code alias from the alias map.</span>
              </div>
            </div>
            <div className="field">
              <label htmlFor="r-priority">Priority</label>
              <input
                id="r-priority"
                className="input num"
                type="number"
                min={0}
                step={1}
                value={form.priority}
                onChange={(e) => setForm((f) => ({ ...f, priority: e.target.value }))}
              />
              <span className="hint">Higher wins when several rules match.</span>
            </div>
            <div className="field has-select">
              <label htmlFor="r-strategy">Strategy</label>
              <select
                id="r-strategy"
                value={form.strategy}
                onChange={(e) => setForm((f) => ({ ...f, strategy: e.target.value }))}
              >
                <option value="">Inherit global ({globalStrategy === 'round_robin' ? 'Round Robin' : 'Fill-First'})</option>
                <option value="round_robin">Round Robin</option>
                <option value="fill_first">Fill-First</option>
              </select>
              <span className="hint">
                How requests spread across the chain{'&apos;'}s accounts. Inherit uses the global
                default.
              </span>
            </div>
            <label className="toggle" style={{ display: 'flex' }}>
              <input
                type="checkbox"
                checked={form.enabled}
                onChange={(e) => setForm((f) => ({ ...f, enabled: e.target.checked }))}
              />
              <span className="tswitch" />
              Rule enabled
            </label>

            <fieldset className="fieldset">
              <legend>Fallback chain (order matters)</legend>
              {form.chain.length === 0 ? (
                <p className="hint">Empty chain — no request can be routed under this rule.</p>
              ) : (
                <div style={{ display: 'flex', flexDirection: 'column', gap: 6 }}>
                  {form.chain.map((modelID, i) => (
                    <div
                      key={`${modelID}-${i}`}
                      style={{ display: 'flex', alignItems: 'center', gap: 8, fontSize: 'var(--fs-sm)' }}
                    >
                      <span className="mono">{`${i + 1}. ${modelID}`}</span>
                      <span style={{ marginLeft: 'auto', display: 'inline-flex', gap: 4 }}>
                        <button type="button" className="btn btn-icon btn-sm" onClick={() => move(i, -1)} disabled={i === 0} aria-label="Move up">
                          <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.2" aria-hidden="true">
                            <path d="m6 14 6-6 6 6" />
                          </svg>
                        </button>
                        <button
                          type="button"
                          className="btn btn-icon btn-sm"
                          onClick={() => move(i, 1)}
                          disabled={i === form.chain.length - 1}
                          aria-label="Move down"
                        >
                          <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.2" aria-hidden="true">
                            <path d="m6 10 6 6 6-6" />
                          </svg>
                        </button>
                        <button
                          type="button"
                          className="btn btn-icon btn-sm"
                          onClick={() => setForm((f) => ({ ...f, chain: f.chain.filter((_, j) => j !== i) }))}
                          aria-label="Remove from chain"
                        >
                          <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.2" aria-hidden="true">
                            <path d="m6 6 12 12M18 6 6 18" />
                          </svg>
                        </button>
                      </span>
                    </div>
                  ))}
                </div>
              )}
              <div className="field has-select" style={{ marginTop: 12 }}>
                <label htmlFor="r-append">Append model to chain</label>
                <select id="r-append" value="" onChange={(e) => appendChain(e.target.value)}>
                  <option value="">Pick a registered model…</option>
                  {modelIDs.map((id) => (
                    <option key={id} value={id}>
                      {id}
                    </option>
                  ))}
                </select>
                {modelIDs.length === 0 && (
                  <span className="hint">No models registered yet — register models first (Models &amp; Compatibility).</span>
                )}
              </div>
            </fieldset>
          </form>
        </Drawer>
      )}
    </>
  );
}