import { FormEvent, useCallback, useMemo, useState } from 'react';
import { apiCall } from '../api';
import type {
  Model,
  ModelSyncConfig,
  ModelSyncRunResponse,
  ModelsResponse,
  Provider,
  ProvidersResponse,
} from '../types';
import { EmptyState, ErrorNote, PageHead, useAdminData, useMutation } from '../hooks';
import { Drawer } from '../components/Overlays';
import { IconPlus } from '../icons';

// "Models & Compatibility" (plan §9 + §6.4) — the central per-model editor:
// identity, provider link, capabilities, priority, and the compatibility
// policy that governs non-Claude routing on the Anthropic surface. The
// compat blob is structured here (not raw JSON): the four §6.4 settings
// have fixed value sets, so the editor renders real controls instead of a
// JSON textarea — validation errors are impossible by construction.

interface CompatForm {
  thinking_handling: 'strip' | 'map-to-reasoning-effort' | 'block';
  server_tool_emulation: 'native' | 'emulate-firecrawl' | 'off';
  max_tokens_clamp: string; // text input → number | 'off' (0 = off)
  context_window: string; // text input → number
  cache_control: 'forward' | 'drop';
}

const DEFAULT_COMPAT: CompatForm = {
  thinking_handling: 'strip',
  server_tool_emulation: 'off',
  max_tokens_clamp: '0',
  context_window: '0',
  cache_control: 'drop',
};

const CAPABILITY_FLAGS = ['streaming', 'vision', 'web_search', 'web_fetch', 'code_execution'] as const;

// Read the raw compat blob defensively: unknown/missing fields fall back
// to defaults instead of crashing the editor (the blob round-trips through
// the API verbatim; we only project the §6.4 keys we know).
function compatFromRaw(raw: unknown): CompatForm {
  if (raw === null || raw === undefined || typeof raw !== 'object') return { ...DEFAULT_COMPAT };
  const o = raw as Record<string, unknown>;
  const pick = (key: keyof CompatForm, allowed: string[], fallback: string): string => {
    const v = o[key];
    return typeof v === 'string' && allowed.includes(v) ? v : fallback;
  };
  return {
    thinking_handling: pick('thinking_handling', ['strip', 'map-to-reasoning-effort', 'block'], 'strip') as CompatForm['thinking_handling'],
    server_tool_emulation: pick(
      'server_tool_emulation',
      ['native', 'emulate-firecrawl', 'off'],
      'off',
    ) as CompatForm['server_tool_emulation'],
    max_tokens_clamp: typeof o.max_tokens_clamp === 'number' ? String(o.max_tokens_clamp) : '0',
    context_window: typeof o.context_window === 'number' ? String(o.context_window) : '0',
    cache_control: pick('cache_control', ['forward', 'drop'], 'drop') as CompatForm['cache_control'],
  };
}

function capsFromRaw(raw: unknown): Record<string, boolean> {
  const out: Record<string, boolean> = {};
  for (const flag of CAPABILITY_FLAGS) {
    out[flag] = false;
  }
  if (raw !== null && typeof raw === 'object') {
    const o = raw as Record<string, unknown>;
    if (Array.isArray(o.tools)) {
      for (const t of o.tools) {
        if (typeof t === 'string' && t in out) out[t] = true;
      }
    }
    for (const flag of CAPABILITY_FLAGS) {
      if (o[flag] === true) out[flag] = true;
    }
  }
  return out;
}

function parsePositiveInt(s: string): number | null {
  const n = Number(s);
  if (!Number.isInteger(n) || n < 0) return null;
  return n;
}

export default function Models() {
  const { data, loading, error, reload } = useAdminData<ModelsResponse>(
    (signal) => apiCall<ModelsResponse>('GET', '/api/admin/models', undefined, signal),
  );
  const { data: providersData } = useAdminData<ProvidersResponse>(
    (signal) => apiCall<ProvidersResponse>('GET', '/api/admin/providers', undefined, signal),
  );
  const models = useMemo(() => data?.models ?? [], [data]);
  const providers = useMemo(() => providersData?.providers ?? [], [providersData]);
  const providerName = useCallback(
    (id: string) => providers.find((p: Provider) => p.id === id)?.name ?? id,
    [providers],
  );

  const [editing, setEditing] = useState<Model | null>(null);
  const [creating, setCreating] = useState(false);
  const [formError, setFormError] = useState<string | null>(null);
  const [form, setForm] = useState({
    id: '',
    provider_id: '',
    name: '',
    priority: '0',
    capabilities: {} as Record<string, boolean>,
    compat: DEFAULT_COMPAT,
  });

  const save = useMutation();
  const del = useMutation();

  const openCreate = useCallback(() => {
    setForm({
      id: '',
      provider_id: providers[0]?.id ?? '',
      name: '',
      priority: '0',
      capabilities: capsFromRaw(null),
      compat: { ...DEFAULT_COMPAT },
    });
    setCreating(true);
    setEditing(null);
    setFormError(null);
  }, [providers]);

  const openEdit = useCallback((m: Model) => {
    setForm({
      id: m.id,
      provider_id: m.provider_id,
      name: m.name,
      priority: String(m.priority),
      capabilities: capsFromRaw(m.capabilities),
      compat: compatFromRaw(m.compat),
    });
    setEditing(m);
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
      if (form.provider_id === '') {
        setFormError('Pick a provider first — models belong to one.');
        return;
      }
      const priority = parsePositiveInt(form.priority);
      if (priority === null) {
        setFormError('Priority must be a non-negative integer');
        return;
      }
      const clamp = parsePositiveInt(form.compat.max_tokens_clamp);
      const window_ = parsePositiveInt(form.compat.context_window);
      if (clamp === null || window_ === null) {
        setFormError('max_tokens_clamp and context_window must be non-negative integers');
        return;
      }
      setFormError(null);
      const caps: Record<string, unknown> = { tools: Object.keys(form.capabilities).filter((k) => form.capabilities[k]) };
      for (const flag of CAPABILITY_FLAGS) {
        caps[flag] = form.capabilities[flag] === true;
      }
      const ok = await save.run(() =>
        apiCall('PUT', `/api/admin/models/${encodeURIComponent(id)}`, {
          provider_id: form.provider_id,
          name: form.name,
          capabilities: caps,
          compat: {
            thinking_handling: form.compat.thinking_handling,
            server_tool_emulation: form.compat.server_tool_emulation,
            // Wire contract (httpapi wireCompat): int, 0 = off — never the
            // string 'off', which would fail decoding and silently drop the
            // whole policy.
            max_tokens_clamp: clamp,
            context_window: window_,
            cache_control: form.compat.cache_control,
          },
          priority,
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
    async (m: Model) => {
      const ok = await del.run(() => apiCall('DELETE', `/api/admin/models/${encodeURIComponent(m.id)}`));
      if (ok) reload();
    },
    [del, reload],
  );

  // ADR-010 model sync: config load + manual run. The sync status card
  // sits above the table; "Sync now" runs a pass synchronously and
  // refreshes the list (the run summary lands in the card).
  const { data: syncData, reload: reloadSync } = useAdminData<ModelSyncConfig>(
    (signal) => apiCall<ModelSyncConfig>('GET', '/api/admin/model-sync', undefined, signal),
  );
  const syncRun = useMutation();

  const runSync = useCallback(async () => {
    const ok = await syncRun.run(() =>
      apiCall<ModelSyncRunResponse>('POST', '/api/admin/model-sync/run', {}),
    );
    if (ok !== null) {
      reloadSync();
      reload();
    }
  }, [syncRun, reloadSync, reload]);

  const toggleSync = useCallback(
    async (enabled: boolean) => {
      const ok = await syncRun.run(() => apiCall('PUT', '/api/admin/model-sync', { enabled }));
      if (ok !== null) reloadSync();
    },
    [syncRun, reloadSync],
  );

  const lastRun = syncData?.last_run ?? null;
  const lastRunText = lastRun === null || lastRun.ran_at === 0
    ? 'never run'
    : new Date(lastRun.ran_at * 1000).toLocaleString();

  const drawerError = formError ?? save.error ?? del.error;
  const drawerPending = save.pending || del.pending;

  return (
    <>
      <PageHead
        title="Models & Compatibility"
        sub="Every model's routing identity, capabilities and compatibility policy — the single place non-Claude routing on the Anthropic surface is configured."
        actions={
          <>
            <button
              type="button"
              className="btn btn-secondary"
              onClick={() => void runSync()}
              disabled={syncRun.pending}
              title="Import the model lists of every provider that offers discovery (ADR-010)"
            >
              {syncRun.pending ? 'Syncing…' : 'Sync now'}
            </button>
            <button
              type="button"
              className="btn btn-primary"
              onClick={openCreate}
              disabled={providers.length === 0}
              title={providers.length === 0 ? 'Add a provider first — models belong to one.' : undefined}
            >
              <IconPlus width="16" height="16" />
              Add model
            </button>
          </>
        }
      />
      <div className="canvas">
        {/* ADR-010 model-sync status card. */}
        <div className="w-full" style={{ marginBottom: 8 }}>
          <div className="fieldset" style={{ display: 'flex', alignItems: 'center', gap: 12, flexWrap: 'wrap', padding: '10px 14px' }}>
            <h4 style={{ margin: 0 }}>Auto-sync</h4>
            <label className="toggle" style={{ display: 'flex', gap: 6 }}>
              <input
                type="checkbox"
                checked={syncData?.enabled ?? true}
                onChange={(e) => void toggleSync(e.target.checked)}
                disabled={syncRun.pending}
              />
              <span className="tswitch" />
              enabled
            </label>
            <span className="hint" style={{ margin: 0 }}>
              every {syncData?.interval_hours ?? 24}h · last run {lastRunText}
            </span>
            {lastRun !== null && lastRun.providers !== undefined && (
              <span className="hint" style={{ margin: 0 }}>
                {lastRun.providers.map((p) => `${p.provider_id.slice(0, 12)}: +${p.added} ~${p.updated} −${p.removed}`).join(' · ')}
              </span>
            )}
            {syncRun.error !== null && (
              <span role="alert" style={{ color: 'var(--status-down)', fontSize: 'var(--fs-sm)' }}>
                {syncRun.error}
              </span>
            )}
          </div>
        </div>
        {error !== null ? (
          <div className="w-full">
            <ErrorNote message={error} />
          </div>
        ) : loading ? (
          <div className="w-full">
            <EmptyState title="Loading models" body="Fetching the model list." />
          </div>
        ) : models.length === 0 ? (
          <div className="w-full">
            <EmptyState
              title="No models registered"
              body="Register a model so routing rules can target it. Every model carries its capability flags and compatibility policy here — including thinking handling for non-Claude targets."
              action={
                providers.length === 0 ? undefined : (
                  <button type="button" className="btn btn-primary btn-sm" onClick={openCreate}>
                    <IconPlus width="14" height="14" />
                    Register the first model
                  </button>
                )
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
                      <th>Model</th>
                      <th>Provider</th>
                      <th>Thinking</th>
                      <th>Tool emulation</th>
                      <th>Priority</th>
                      <th />
                    </tr>
                  </thead>
                  <tbody>
                    {models.map((m) => {
                      const compat = compatFromRaw(m.compat);
                      return (
                        <tr key={m.id} onClick={() => openEdit(m)}>
                          <td>
                            <span className="primary">{m.name}</span>
                            <br />
                            <span className="mono">{m.id}</span>
                          </td>
                          <td>
                            {providerName(m.provider_id)}
                            <br />
                            {/* ADR-010 provenance: synced rows are the
                                auto-import's; editing one converts it to
                                manual. */}
                            <span className={m.origin === 'synced' ? 'badge info' : 'badge paused'}>
                              <span className="b-dot" />
                              {m.origin === 'synced' ? 'synced' : 'manual'}
                            </span>
                          </td>
                          <td>
                            <span className="badge info">{compat.thinking_handling}</span>
                          </td>
                          <td>
                            <span className="badge info">{compat.server_tool_emulation}</span>
                          </td>
                          <td className="num">{m.priority}</td>
                          <td>
                            <span className="row-actions">
                              <button
                                type="button"
                                className="btn btn-danger btn-sm"
                                onClick={(e) => {
                                  e.stopPropagation();
                                  void remove(m);
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
          title={creating ? 'Add model' : `Edit ${editing?.name}`}
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
              <button type="submit" className="btn btn-primary" form="model-form" disabled={drawerPending}>
                Save
              </button>
            </>
          }
        >
          <form className="form" id="model-form" onSubmit={submit}>
            {drawerError !== null && (
              <p role="alert" style={{ color: 'var(--status-down)', fontSize: 'var(--fs-sm)', margin: 0 }}>
                {drawerError}
              </p>
            )}
            <div className="form-row">
              <div className="field">
                <label htmlFor="m-id">ID</label>
                <input
                  id="m-id"
                  className="input mono"
                  type="text"
                  value={form.id}
                  onChange={(e) => setForm((f) => ({ ...f, id: e.target.value }))}
                  disabled={!creating}
                  required
                  placeholder="e.g. claude-sonnet-5"
                />
              </div>
              <div className="field has-select">
                <label htmlFor="m-provider">Provider</label>
                <select
                  id="m-provider"
                  value={form.provider_id}
                  onChange={(e) => setForm((f) => ({ ...f, provider_id: e.target.value }))}
                  required
                >
                  {providers.map((p) => (
                    <option key={p.id} value={p.id}>
                      {p.name}
                    </option>
                  ))}
                </select>
              </div>
            </div>
            <div className="form-row">
              <div className="field">
                <label htmlFor="m-name">Name</label>
                <input
                  id="m-name"
                  className="input"
                  type="text"
                  value={form.name}
                  onChange={(e) => setForm((f) => ({ ...f, name: e.target.value }))}
                  required
                  placeholder="Model name clients see"
                />
              </div>
              <div className="field">
                <label htmlFor="m-priority">Priority</label>
                <input
                  id="m-priority"
                  className="input num"
                  type="number"
                  min={0}
                  step={1}
                  value={form.priority}
                  onChange={(e) => setForm((f) => ({ ...f, priority: e.target.value }))}
                />
                <span className="hint">Higher wins when several rules match.</span>
              </div>
            </div>

            <fieldset className="fieldset">
              <legend>Capabilities</legend>
              {CAPABILITY_FLAGS.map((flag) => (
                <label key={flag} className="check" style={{ display: 'flex', marginBottom: 8 }}>
                  <input
                    type="checkbox"
                    checked={form.capabilities[flag] === true}
                    onChange={(e) =>
                      setForm((f) => ({ ...f, capabilities: { ...f.capabilities, [flag]: e.target.checked } }))
                    }
                  />
                  <span className="cbox">
                    <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="3.5" aria-hidden="true">
                      <path d="m5 13 4 4 10-10" />
                    </svg>
                  </span>
                  {flag}
                </label>
              ))}
            </fieldset>

            <fieldset className="fieldset">
              <legend>Compatibility policy (§6.4)</legend>
              <div className="field has-select">
                <label htmlFor="m-thinking">Thinking handling</label>
                <select
                  id="m-thinking"
                  value={form.compat.thinking_handling}
                  onChange={(e) =>
                    setForm((f) => ({
                      ...f,
                      compat: { ...f.compat, thinking_handling: e.target.value as CompatForm['thinking_handling'] },
                    }))
                  }
                >
                  <option value="strip">strip — remove thinking blocks + param upstream</option>
                  <option value="map-to-reasoning-effort">map-to-reasoning-effort — budget → effort</option>
                  <option value="block">block — refuse with a protocol-conform error</option>
                </select>
                <span className="hint">Never fabricates signatures; strip is the safe default for non-Claude targets.</span>
              </div>
              <div className="field has-select">
                <label htmlFor="m-emulation">Server tool emulation</label>
                <select
                  id="m-emulation"
                  value={form.compat.server_tool_emulation}
                  onChange={(e) =>
                    setForm((f) => ({
                      ...f,
                      compat: {
                        ...f.compat,
                        server_tool_emulation: e.target.value as CompatForm['server_tool_emulation'],
                      },
                    }))
                  }
                >
                  <option value="off">off — no emulation, capability gap fails the chain</option>
                  <option value="emulate-firecrawl">emulate-firecrawl — Firecrawl substitutes web tools</option>
                  <option value="native">native — provider has its own server tools</option>
                </select>
              </div>
              <div className="form-row">
                <div className="field">
                  <label htmlFor="m-clamp">max_tokens clamp</label>
                  <input
                    id="m-clamp"
                    className="input num"
                    type="number"
                    min={0}
                    step={1}
                    value={form.compat.max_tokens_clamp}
                    onChange={(e) => setForm((f) => ({ ...f, compat: { ...f.compat, max_tokens_clamp: e.target.value } }))}
                  />
                  <span className="hint">0 = off. Clamps output for targets with lower limits.</span>
                </div>
                <div className="field">
                  <label htmlFor="m-window">Context window</label>
                  <input
                    id="m-window"
                    className="input num"
                    type="number"
                    min={0}
                    step={1}
                    value={form.compat.context_window}
                    onChange={(e) => setForm((f) => ({ ...f, compat: { ...f.compat, context_window: e.target.value } }))}
                  />
                  <span className="hint">0 = unknown. Used for capability filtering and token estimates.</span>
                </div>
              </div>
              <div className="field has-select">
                <label htmlFor="m-cache">cache_control</label>
                <select
                  id="m-cache"
                  value={form.compat.cache_control}
                  onChange={(e) =>
                    setForm((f) => ({
                      ...f,
                      compat: { ...f.compat, cache_control: e.target.value as CompatForm['cache_control'] },
                    }))
                  }
                >
                  <option value="drop">drop — remove cache markers when crossing protocols</option>
                  <option value="forward">forward — pass through on Anthropic-shaped upstreams</option>
                </select>
              </div>
            </fieldset>
          </form>
        </Drawer>
      )}
    </>
  );
}