import { FormEvent, useCallback, useEffect, useMemo, useState } from 'react';
import { apiCall } from '../api';
import type {
  CapabilityFallbacks,
  ModelsResponse,
  ProviderTypesResponse,
  RoutingRule,
  RoutingStrategyResponse,
  RulesResponse,
  SettingsResponse,
  Strategy,
} from '../types';
import { CONFIG_LIVE_MS, EmptyState, ErrorNote, PageHead, useAdminData, useLiveData, useMutation } from '../hooks';
import { LiveBadge } from '../components/LiveBadge';
import { Drawer } from '../components/Overlays';
import PrioList from '../components/PrioList';
import AliasEditor from '../components/AliasEditor';
import { IconPlus } from '../icons';

// The model alias map's settings key (the KV the router loads; the
// interactive editor writes the same JSON object the old textarea did).
const ALIASES_KEY = 'model_aliases';

// Routing (plan §9 + ADR-009 + ADR-010): the global strategy (Round Robin
// default / Fill-First), rules with per-rule strategy overrides, the
// capability fallback lists, and the model alias map. The admin API's rule model
// is match_model → chain ([]string of provider types, provider ids, or
// provider/model pairs) + priority + enabled + strategy; the chain editor
// is an ordered list the operator can append to and reorder (PrioList
// drag-drop), since the chain IS the fallback order (Analysis E). Type
// entries expand to all accounts of that type at request time — new
// accounts join without rule edits. The ADR-010 fallback lists (vision /
// web_search / web_fetch) are global PrioLists: vision entries are
// provider IDs/types appended to vision-less chains, web entries are
// web_tool accounts tried in order by the emulator pool. The global
// allow/warn/block switch stays on Models (the model-surface policy).

const STRATEGIES: { value: Strategy; label: string; desc: string }[] = [
  { value: 'round_robin', label: 'Round Robin', desc: 'Requests rotate across all accounts of a type — even load distribution.' },
  { value: 'fill_first', label: 'Fill-First', desc: 'Account 1 serves everything until it fails (rate limit / breaker) — then the next takes over.' },
];

// CapabilityFallbacksCard (ADR-010 + 2026-09-21 automatic capabilities):
// the three global fallback lists, each a drag-drop PrioList. Vision lists
// MODELS (routing IDs) tried by the executor after the routed chain hit a
// real upstream capability gap; web_search/web_fetch list web-tool
// accounts (today: firecrawl) the emulator pool tries in order. One save
// writes all three.
function CapabilityFallbacksCard() {
  const { data, loading, error, reload, updatedAt } = useLiveData<CapabilityFallbacks>(
    (signal) => apiCall<CapabilityFallbacks>('GET', '/api/admin/capability-fallbacks', undefined, signal),
    CONFIG_LIVE_MS,
  );
  // Models (for the vision picker) + web-tool accounts (for the web pickers).
  const { data: modelsData } = useAdminData<ModelsResponse>(
    (signal) => apiCall<ModelsResponse>('GET', '/api/admin/models', undefined, signal),
  );
  const { data: catalog } = useAdminData<ProviderTypesResponse>(
    (signal) => apiCall<ProviderTypesResponse>('GET', '/api/admin/provider-types', undefined, signal),
  );
  const { data: providersData } = useAdminData<ProvidersResponseLike>(
    (signal) => apiCall<ProvidersResponseLike>('GET', '/api/admin/providers', undefined, signal),
  );

  const [saving, setSaving] = useState(false);
  const [form, setForm] = useState<CapabilityFallbacks | null>(null);
  const [seeded, setSeeded] = useState(false);
  const [saveError, setSaveError] = useState<string | null>(null);

  useEffect(() => {
    if (seeded || data === null) return;
    setForm({ vision: data.vision, web_search: data.web_search, web_fetch: data.web_fetch });
    setSeeded(true);
  }, [seeded, data]);

  // Vision fallback options are MODELS: the fallback names
  // which model serves the capability when the routed model rejected it.
  const modelOptions = useMemo(() => {
    const providerLabel = new Map((providersData?.providers ?? []).map((p) => [p.id, p.name]));
    return (modelsData?.models ?? []).map((m) => ({
      id: m.id,
      label: `${m.id}${m.name !== m.id ? ` (${m.name})` : ''}${providerLabel.get(m.provider_id) ? ` — ${providerLabel.get(m.provider_id)}` : ''}`,
    }));
  }, [modelsData, providersData]);

  const webToolOptions = useMemo(() => {
    const webTypes = new Set((catalog?.types ?? []).filter((t) => t.kind === 'web_tool').map((t) => t.type));
    return (providersData?.providers ?? [])
      .filter((p) => webTypes.has(p.type))
      .map((p) => ({ id: p.id, label: `${p.name} (${p.type})` }));
  }, [catalog, providersData]);
  const modelLabels = useMemo(() => new Map(modelOptions.map((o) => [o.id, o.label])), [modelOptions]);
  const webToolLabels = useMemo(() => new Map(webToolOptions.map((o) => [o.id, o.label])), [webToolOptions]);

  const submit = useCallback(async () => {
    if (form === null) return;
    setSaving(true);
    setSaveError(null);
    try {
      await apiCall('PUT', '/api/admin/capability-fallbacks', {
        vision: form.vision,
        web_search: form.web_search,
        web_fetch: form.web_fetch,
      });
    } catch (err) {
      setSaving(false);
      setSaveError(err instanceof Error ? err.message : 'Request failed');
      return;
    }
    setSaving(false);
    reload();
  }, [form, reload]);

  return (
    <div className="w-full" style={{ marginBottom: 8 }}>
      <fieldset className="fieldset">
        <legend>
          Capability fallbacks
          <span style={{ marginLeft: 8, verticalAlign: 'middle' }}>
            <LiveBadge updatedAt={updatedAt} intervalMs={CONFIG_LIVE_MS} />
          </span>
        </legend>
        <p className="hint" style={{ marginTop: 0 }}>
          Priority lists consulted when the routed chain cannot serve a
          request. Vision: models tried when the routed model rejected the
          request at the upstream (e.g. image input). Web search / fetch:
          web-tool accounts (e.g. Firecrawl) the proxy tries in order.
          Every use is traced in the logs.
        </p>
        {(error ?? saveError) !== null && (
          <p role="alert" className="form-note">
            {error ?? saveError}
          </p>
        )}
        {loading || form === null ? (
          <p className="hint">Loading fallback lists…</p>
        ) : (
          <div style={{ display: 'grid', gap: 20, gridTemplateColumns: 'repeat(auto-fit, minmax(280px, 1fr))' }}>
            <div>
              <h4 style={{ margin: '0 0 6px' }}>Vision fallback</h4>
              <PrioList
                label="Vision fallback priority list"
                items={form.vision}
                onChange={(vision) => setForm((f) => (f === null ? f : { ...f, vision }))}
                options={modelOptions}
                resolveLabel={(id) => modelLabels.get(id) ?? id}
                addLabel="Add model to vision fallback"
                emptyHint="No vision fallback — requests the routed model cannot serve fail with the upstream error."
              />
            </div>
            <div>
              <h4 style={{ margin: '0 0 6px' }}>Web search fallback</h4>
              <PrioList
                label="Web search fallback priority list"
                items={form.web_search}
                onChange={(web_search) => setForm((f) => (f === null ? f : { ...f, web_search }))}
                options={webToolOptions}
                resolveLabel={(id) => webToolLabels.get(id) ?? id}
                addLabel="Add web-tool account to search fallback"
                emptyHint="No web search fallback — emulated search uses the single configured emulator."
              />
            </div>
            <div>
              <h4 style={{ margin: '0 0 6px' }}>Web fetch fallback</h4>
              <PrioList
                label="Web fetch fallback priority list"
                items={form.web_fetch}
                onChange={(web_fetch) => setForm((f) => (f === null ? f : { ...f, web_fetch }))}
                options={webToolOptions}
                resolveLabel={(id) => webToolLabels.get(id) ?? id}
                addLabel="Add web-tool account to fetch fallback"
                emptyHint="No web fetch fallback — emulated fetch uses the single configured emulator."
              />
            </div>
          </div>
        )}
        <div style={{ marginTop: 12 }}>
          <button type="button" className="btn btn-primary btn-sm" disabled={saving || form === null} onClick={() => void submit()}>
            Save fallbacks
          </button>
        </div>
      </fieldset>
    </div>
  );
}

// ProvidersResponseLike mirrors the /api/admin/providers list shape (the
// Providers page owns the canonical type; here only id/type/name matter).
interface ProvidersResponseLike {
  providers: { id: string; type: string; name: string }[];
}

// AliasCard: the model alias map, relocated here from
// Models — it IS routing configuration (it rewrites the client-requested
// name before any rule matches). The interactive AliasEditor (rows of
// alias → target picker) replaced the raw-JSON textarea; writes ride the
// same settings KV, so the router's live alias reload applies untouched.
function AliasCard() {
  const { data: settingsData, reload: reloadSettings } = useAdminData<SettingsResponse>(
    (signal) => apiCall<SettingsResponse>('GET', '/api/admin/settings', undefined, signal),
  );
  const { data: modelsData } = useAdminData<ModelsResponse>(
    (signal) => apiCall<ModelsResponse>('GET', '/api/admin/models', undefined, signal),
  );
  const options = useMemo(
    () => (modelsData?.models ?? []).map((m) => ({
      id: m.id,
      label: `${m.id}${m.name !== m.id ? ` (${m.name})` : ''}`,
    })),
    [modelsData],
  );

  const [aliases, setAliases] = useState<Record<string, string> | null>(null);
  const [seeded, setSeeded] = useState(false);
  const save = useMutation();

  useEffect(() => {
    if (seeded || settingsData === null) return;
    const raw = settingsData.entries.find((e) => e.key === ALIASES_KEY)?.value ?? '';
    const parsed: Record<string, string> = {};
    if (raw !== '') {
      try {
        const p: unknown = JSON.parse(raw);
        if (p !== null && typeof p === 'object' && !Array.isArray(p)) {
          for (const [k, v] of Object.entries(p as Record<string, unknown>)) {
            if (typeof v === 'string' && v !== '') parsed[k] = v;
          }
        }
      } catch {
        // A corrupted blob starts the editor empty and is replaced on the
        // next save — never a hard failure (the server load's posture).
      }
    }
    setAliases(parsed);
    setSeeded(true);
  }, [seeded, settingsData]);

  const submit = useCallback(async () => {
    if (aliases === null) return;
    // run() resolves the value on success, null on failure — the thrown
    // path's error lands in save.error, which the render shows directly
    // (reading save.error HERE would see the previous run's state — the
    // v1.0.2 stale-mutation lesson; branch on the ok return instead).
    const ok = await save.run(() =>
      apiCall('PUT', '/api/admin/settings', { key: ALIASES_KEY, value: JSON.stringify(aliases) }),
    );
    if (ok !== null) reloadSettings();
  }, [aliases, save, reloadSettings]);

  const dirty = useMemo(() => {
    if (settingsData === null || aliases === null) return false;
    const raw = settingsData.entries.find((e) => e.key === ALIASES_KEY)?.value ?? '{}';
    try {
      return JSON.stringify(JSON.parse(raw)) !== JSON.stringify(aliases);
    } catch {
      return true;
    }
  }, [settingsData, aliases]);

  return (
    <div className="w-full" style={{ marginBottom: 8 }}>
      <fieldset className="fieldset">
        <legend>
          Model Aliases
        </legend>
        <p className="hint" style={{ marginTop: 0 }}>
          Client-requested names rewritten to registered models before routing —
          a client&apos;s <span className="mono">sonnet</span>/<span className="mono">haiku</span> family names map onto
          whatever model serves them here.
        </p>
        {save.error !== null && (
          <p role="alert" className="form-note">{save.error}</p>
        )}
        {aliases === null ? (
          <p className="hint">Loading aliases…</p>
        ) : (
          <>
            <AliasEditor
              value={aliases}
              onChange={setAliases}
              options={options}
              disabled={save.pending}
            />
            <div style={{ marginTop: 10 }}>
              <button
                type="button"
                className="btn btn-primary btn-sm"
                disabled={save.pending || !dirty}
                title={dirty ? undefined : 'No changes to save'}
                onClick={() => void submit()}
              >
                {save.pending ? 'Saving…' : 'Save aliases'}
              </button>
            </div>
          </>
        )}
      </fieldset>
    </div>
  );
}

export default function Routing() {
  const { data, loading, error, reload, updatedAt } = useLiveData<RulesResponse>(
    (signal) => apiCall<RulesResponse>('GET', '/api/admin/routing', undefined, signal),
    CONFIG_LIVE_MS,
  );
  const { data: modelsData } = useAdminData<ModelsResponse>(
    (signal) => apiCall<ModelsResponse>('GET', '/api/admin/models', undefined, signal),
  );
  const { data: providersData } = useAdminData<ProvidersResponseLike>(
    (signal) => apiCall<ProvidersResponseLike>('GET', '/api/admin/providers', undefined, signal),
  );
  const { data: strategyData, reload: reloadStrategy } = useAdminData<RoutingStrategyResponse>(
    (signal) => apiCall<RoutingStrategyResponse>('GET', '/api/admin/routing-strategy', undefined, signal),
  );
  const rules = useMemo(() => data?.rules ?? [], [data]);
  const models = useMemo(() => modelsData?.models ?? [], [modelsData]);
  // Display labels for chain entries: a chain stores MODEL ids, and an
  // operator picking one should see the provider it belongs to rather than
  // the bare id.
  const chainProviderLabel = useMemo(
    () => new Map((providersData?.providers ?? []).map((p) => [p.id, p.name])),
    [providersData],
  );
  const chainOptions = useMemo(
    () =>
      models.map((m) => {
        const prov = chainProviderLabel.get(m.provider_id);
        return { id: m.id, label: `${m.id}${prov ? ` — ${prov}` : ''}` };
      }),
    [models, chainProviderLabel],
  );
  const chainLabels = useMemo(() => new Map(chainOptions.map((o) => [o.id, o.label])), [chainOptions]);
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
          <>
            <LiveBadge updatedAt={updatedAt} intervalMs={CONFIG_LIVE_MS} />
            <button type="button" className="btn btn-primary" onClick={openCreate}>
              <IconPlus width="16" height="16" />
              Add rule
            </button>
          </>
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
              <p role="alert" className="form-note">
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
        {/* ADR-010: the three global capability fallback lists. */}
        <CapabilityFallbacksCard />
        {/* The model alias map — relocated from Models. */}
        <AliasCard />
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
              body="A rule matches a model pattern and holds the ordered fallback chain. Rules are optional: models without a matching rule route implicitly to their registered provider (traced in every request log) — rules add fallback chains, priorities, and rotation."
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
                            <span className="mono">{rule.chain.map((id) => chainLabels.get(id) ?? id).join(' → ')}</span>
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
              <p role="alert" className="form-note">
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
                <span className="hint">Exact name, wildcard, or an alias from the alias map.</span>
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
              <PrioList
                label="Fallback chain priority list"
                items={form.chain}
                onChange={(chain) => setForm((f) => ({ ...f, chain }))}
                options={chainOptions}
                resolveLabel={(id) => chainLabels.get(id) ?? id}
                addLabel="Append model to chain"
                emptyHint="Empty chain — no request can be routed under this rule."
              />
            </fieldset>
          </form>
        </Drawer>
      )}
    </>
  );
}