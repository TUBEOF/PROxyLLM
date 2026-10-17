import { FormEvent, useCallback, useMemo, useState } from 'react';
import { apiCall } from '../api';
import type {
  Model,
  ModelSyncConfig,
  ModelSyncRunResponse,
  ModelsResponse,
  ModelTestResponse,
  ProvidersResponse,
  ProviderTypesResponse,
} from '../types';
import { CONFIG_LIVE_MS, EmptyState, ErrorNote, PageHead, useAdminData, useLiveData, useMutation } from '../hooks';
import { LiveBadge } from '../components/LiveBadge';
import { Drawer } from '../components/Overlays';
import { IconCheck, IconClose, IconPlus, IconRefresh } from '../icons';

// "Models" (plan §9) — the central per-model editor: identity, provider
// link, capabilities, priority. The compat knobs are NOT editable anymore
// : the per-model policy derives from the row's capabilities server-side
// (thinking handling, cache markers, server-tool emulation, clamps — every
// application still recorded in route_trace, never silent). Legacy compat
// blobs on existing rows keep working as read-only overrides.
//
// Layout: provider-TYPE-grouped collapsible sections with model CARDS in the
// console's widget shape (no table — the card carries the test control and
// the capabilities read as a column of chips). Capabilities are FACTS, not
// state: only the flags the model actually has are rendered, as neutral
// chips — a row of eight tick/cross marks says mostly "not this one".
// The account is deliberately absent: models are available per provider
// TYPE, the account serving them is routing's business, and it changes
// nothing about the model. Block layout throughout — no variable-length
// text in a wrapping line above other content (the v1.0.1/v1.0.2 lesson).

const CAPABILITY_FLAGS = ['streaming', 'tools', 'vision', 'thinking', 'web_search', 'web_fetch', 'code_execution', 'decision'] as const;

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

// Sync cadence presets, in the order the select offers them. A stored value
// outside the list is appended rather than silently rounded — the control
// shows what is configured.
const INTERVAL_PRESETS = [1, 3, 6, 12, 24, 48, 168] as const;

function intervalLabel(hours: number): string {
  if (hours === 1) return 'hourly';
  if (hours === 24) return 'daily';
  if (hours === 168) return 'weekly';
  return `every ${hours} h`;
}

// TestState is one model card's test slot: a round trip in flight, or its
// lasting outcome. The outcome STAYS once it lands — the point of testing
// several models is to see afterwards which ones answered.
type TestState =
  | { status: 'running' }
  | { status: 'ok' | 'failed'; result: ModelTestResponse };

export default function Models() {
  const { data, loading, error, reload, updatedAt } = useLiveData<ModelsResponse>(
    (signal) => apiCall<ModelsResponse>('GET', '/api/admin/models', undefined, signal),
    CONFIG_LIVE_MS,
  );
  const { data: providersData } = useLiveData<ProvidersResponse>(
    (signal) => apiCall<ProvidersResponse>('GET', '/api/admin/providers', undefined, signal),
    CONFIG_LIVE_MS,
  );
  // The provider-type catalog: its labels and its order drive the grouping
  // below (same source the Providers page renders).
  const { data: typesData } = useLiveData<ProviderTypesResponse>(
    (signal) => apiCall<ProviderTypesResponse>('GET', '/api/admin/provider-types', undefined, signal),
    CONFIG_LIVE_MS,
  );
  const models = useMemo(() => data?.models ?? [], [data]);
  const providers = useMemo(() => providersData?.providers ?? [], [providersData]);
  const types = useMemo(() => typesData?.types ?? [], [typesData]);
  // Web-tool backends (Firecrawl, Ollama Cloud Web Tools) are emulator
  // endpoints for web_search/web_fetch, not chat providers: they own no
  // models, so they are neither a group on this page nor a choice in the
  // provider picker. The server refuses a model row on one for the same
  // reason (models on a web-tool backend could never route).
  const webToolTypes = useMemo(
    () => new Set(types.filter((t) => t.kind === 'web_tool').map((t) => t.type)),
    [types],
  );
  // Types the catalog does not know stay in (the UI never hides data it was
  // given) — only a catalog-declared web_tool kind is filtered.
  const llmProviders = useMemo(
    () => providers.filter((p) => !webToolTypes.has(p.type)),
    [providers, webToolTypes],
  );
  // providerNames: the test popup names the account a round trip went to,
  // and the sync summary names the accounts it touched; a provider that has
  // since been deleted falls back to its id.
  const providerNames = useMemo(
    () => new Map(providers.map((p) => [p.id, p.name])),
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
  });

  const save = useMutation();
  const del = useMutation();

  const openCreate = useCallback(() => {
    setForm({
      id: '',
      provider_id: llmProviders[0]?.id ?? '',
      name: '',
      priority: '0',
      capabilities: capsFromRaw(null),
    });
    setCreating(true);
    setEditing(null);
    setFormError(null);
  }, [llmProviders]);

  const openEdit = useCallback((m: Model) => {
    setForm({
      id: m.id,
      provider_id: m.provider_id,
      name: m.name,
      priority: String(m.priority),
      capabilities: capsFromRaw(m.capabilities),
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
      setFormError(null);
      const caps: Record<string, unknown> = { tools: Object.keys(form.capabilities).filter((k) => form.capabilities[k]) };
      // Operator save = manual selection: every flag written here is
      // pinned so the automatic capability learning stops
      // overwriting it. Unchecking removes the pin (learning resumes).
      const pinned: Record<string, boolean> = {};
      for (const flag of CAPABILITY_FLAGS) {
        const on = form.capabilities[flag] === true;
        caps[flag] = on;
        pinned[flag] = on;
      }
      caps.pinned = pinned;
      // NOTE: compat is NOT sent — the row's stored blob (legacy) stays
      // untouched; the server derives the policy from capabilities.
      const ok = await save.run(() =>
        apiCall('PUT', `/api/admin/models/${encodeURIComponent(id)}`, {
          provider_id: form.provider_id,
          name: form.name,
          capabilities: caps,
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

  // ADR-010 model sync: config load + manual run. The sync card sits above
  // the groups and owns every control (toggle, cadence, run) plus the last
  // run's per-account summary.
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

  const putSyncConfig = useCallback(
    async (body: { enabled?: boolean; interval_hours?: number }) => {
      const ok = await syncRun.run(() => apiCall('PUT', '/api/admin/model-sync', body));
      if (ok !== null) reloadSync();
    },
    [syncRun, reloadSync],
  );

  const syncEnabled = syncData?.enabled ?? true;
  const intervalHours = syncData?.interval_hours ?? 24;
  const intervalOptions = useMemo(() => {
    const opts = INTERVAL_PRESETS.map((h) => ({ hours: h as number, label: intervalLabel(h) }));
    if (!INTERVAL_PRESETS.includes(intervalHours as (typeof INTERVAL_PRESETS)[number])) {
      opts.push({ hours: intervalHours, label: intervalLabel(intervalHours) });
      opts.sort((a, b) => a.hours - b.hours);
    }
    return opts;
  }, [intervalHours]);

  const lastRun = syncData?.last_run ?? null;
  const lastRunText = lastRun === null || lastRun.ran_at === 0
    ? 'never run'
    : new Date(lastRun.ran_at * 1000).toLocaleString();

  // Model test: one minimal round trip per model card. State is per model:
  // running, or the outcome that stays for as long as the page is open.
  const [tests, setTests] = useState<Record<string, TestState>>({});
  // The opened failure popup: what was sent and what came back.
  const [testDetail, setTestDetail] = useState<{ model: Model; result: ModelTestResponse } | null>(null);

  const runTest = useCallback(async (m: Model) => {
    setTests((prev) => ({ ...prev, [m.id]: { status: 'running' } }));
    try {
      const res = await apiCall<ModelTestResponse>(
        'POST',
        `/api/admin/models/test/${encodeURIComponent(m.id)}`,
        {},
      );
      setTests((prev) => ({ ...prev, [m.id]: { status: res.ok ? 'ok' : 'failed', result: res } }));
    } catch (err) {
      // Thrown errors are the truth (useMutation().error is stale by one
      // run — the v1.0.2 lesson). A request that never reached the endpoint
      // is a failure with no upstream detail.
      setTests((prev) => ({
        ...prev,
        [m.id]: {
          status: 'failed',
          result: { ok: false, latency_ms: 0, error: err instanceof Error ? err.message : 'Request failed' },
        },
      }));
    }
  }, []);

  // Grouping: models bucket by their provider's TYPE — the operator's unit
  // ("Ollama Cloud"), not the individual account — in the catalog's type
  // order. Types with accounts but no models appear with an empty hint so
  // the grouping never hides a provider; models of deleted providers keep
  // their own group.
  const groups = useMemo(() => {
    const typeOf = new Map(providers.map((p) => [p.id, p.type]));
    const byType = new Map<string, Model[]>();
    const dangling: Model[] = [];
    for (const m of models) {
      const typ = typeOf.get(m.provider_id);
      // A model row whose provider is gone, or one sitting on a web-tool
      // backend (the API refuses to create those; a row seeded by hand is
      // surfaced here rather than hidden — the UI never drops data it was
      // given, and the operator needs to see it to delete it).
      if (typ === undefined || webToolTypes.has(typ)) {
        dangling.push(m);
        continue;
      }
      const list = byType.get(typ) ?? [];
      list.push(m);
      byType.set(typ, list);
    }
    const hasAccount = new Set<string>();
    for (const p of providers) {
      if (webToolTypes.has(p.type)) continue;
      hasAccount.add(p.type);
    }
    const out: { type: string; label: string; models: Model[] }[] = [];
    const seen = new Set<string>();
    const add = (typ: string, label: string) => {
      seen.add(typ);
      out.push({ type: typ, label, models: byType.get(typ) ?? [] });
    };
    for (const t of types) {
      if (t.kind === 'web_tool') continue; // emulator backends, never chat models
      if (hasAccount.has(t.type)) add(t.type, t.label);
    }
    // A type an account carries but the catalog does not know (renamed or
    // removed type — the UI never hides data it was given).
    for (const typ of hasAccount) {
      if (!seen.has(typ)) add(typ, typ);
    }
    // Models of deleted providers (dangling rows should not exist — the FK
    // guarantees it — but the UI never hides data it was given).
    if (dangling.length > 0) {
      out.push({ type: '', label: 'unknown provider', models: dangling });
    }
    return out;
  }, [models, providers, types, webToolTypes]);

  const drawerError = formError ?? save.error ?? del.error;
  const drawerPending = save.pending || del.pending;

  return (
    <>
      <PageHead
        title="Models"
        sub="Every model's routing identity, its capabilities and its priority — what clients request, and what routing resolves it to."
        actions={
          <>
            <LiveBadge updatedAt={updatedAt} intervalMs={CONFIG_LIVE_MS} />
            <button
              type="button"
              className="btn btn-primary"
              onClick={openCreate}
              disabled={llmProviders.length === 0}
              title={llmProviders.length === 0 ? 'Add a chat provider first — models belong to one.' : undefined}
            >
              <IconPlus width="16" height="16" />
              Add model
            </button>
          </>
        }
      />
      <div className="canvas">
        {/* ADR-010 model-sync card: state, cadence, a manual run and the last
            run's summary. Block layout (the v1.0.1/v1.0.2 lesson:
            variable-length text never rides a wrapping line above other
            content — every account gets its own lines). */}
        <div className="widget w-full" style={{ marginBottom: 8 }}>
          <div className="widget-head">
            <h3 className="title">
              <span className="ic"><IconRefresh width="14" height="14" /></span>
              Auto-sync
            </h3>
            <label className="toggle" title="Import every account's model list on the cadence below">
              <input
                type="checkbox"
                checked={syncEnabled}
                onChange={(e) => void putSyncConfig({ enabled: e.target.checked })}
                disabled={syncRun.pending}
              />
              <span className="tswitch" />
              <span className="muted">{syncEnabled ? 'on' : 'off'}</span>
            </label>
            <span className={`badge ${syncEnabled ? 'up' : 'paused'}`}>
              <span className="b-dot" />
              {syncEnabled ? 'enabled' : 'paused'}
            </span>
          </div>
          <div className="widget-body" style={{ gap: 12 }}>
            <span className="hint" style={{ margin: 0 }}>
              Mirrors each account&apos;s model list into the table below, {intervalLabel(intervalHours)}.
              Detected facts fill empty rows; your own edits and pins survive every run.
            </span>
            {/* Controls: fixed-width items only, so no wrap point can drift. */}
            <div style={{ display: 'flex', alignItems: 'center', gap: 12 }}>
              <span className="select">
                <select
                  aria-label="Sync interval"
                  value={String(intervalHours)}
                  onChange={(e) => void putSyncConfig({ interval_hours: Number(e.target.value) })}
                  disabled={syncRun.pending}
                >
                  {intervalOptions.map((o) => (
                    <option key={o.hours} value={o.hours}>
                      {o.label}
                    </option>
                  ))}
                </select>
              </span>
              <button
                type="button"
                className="btn btn-secondary btn-sm"
                onClick={() => void runSync()}
                disabled={syncRun.pending}
                title="Import the model lists of every provider that offers discovery (ADR-010)"
              >
                {syncRun.pending ? (
                  <svg className="loading-spinner" style={{ width: 14, height: 14 }} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.5" aria-hidden="true">
                    <circle cx="12" cy="12" r="9" opacity="0.25" />
                    <path d="M21 12a9 9 0 0 0-9-9" />
                  </svg>
                ) : (
                  <IconRefresh width="14" height="14" />
                )}
                {syncRun.pending ? 'Syncing…' : 'Sync now'}
              </button>
            </div>
            {syncRun.error !== null && (
              <p role="alert" className="form-note" style={{ margin: 0 }}>
                {syncRun.error}
              </p>
            )}
            {/* Last run: one block per account, so a long name or a long skip
                reason wraps inside its own lines and moves nothing else. */}
            <div className="fieldset" style={{ margin: 0 }}>
              <legend>Last run — {lastRunText}</legend>
              {lastRun === null || lastRun.providers === undefined || lastRun.providers.length === 0 ? (
                <p className="hint" style={{ margin: 0 }}>
                  {lastRun === null || lastRun.ran_at === 0
                    ? 'No run recorded yet. Run the sync once to import the model lists.'
                    : 'The last run touched no account that offers model discovery.'}
                </p>
              ) : (
                <div style={{ display: 'flex', flexDirection: 'column', gap: 8 }}>
                  {lastRun.providers.map((p) => (
                    <div key={p.provider_id}>
                      <div className="primary" style={{ overflowWrap: 'anywhere' }}>
                        {providerNames.get(p.provider_id) ?? p.provider_id}
                      </div>
                      {p.skipped > 0 ? (
                        <p className="form-note" style={{ margin: 0, overflowWrap: 'anywhere' }}>
                          skipped — {p.skipped_reason ?? 'no reason recorded'}
                        </p>
                      ) : (
                        <span className="hint" style={{ margin: 0 }}>
                          <span className="num">+{p.added}</span> added · <span className="num">~{p.updated}</span> updated ·{' '}
                          <span className="num">−{p.removed}</span> removed
                        </span>
                      )}
                    </div>
                  ))}
                </div>
              )}
            </div>
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
              body="Register a model (or run the model sync) so clients can request it — models without a matching routing rule route implicitly to their provider. Every model carries its capability flags and compatibility policy here — including thinking handling for non-Claude targets."
              action={
                llmProviders.length === 0 ? undefined : (
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
            {/* Provider-type-grouped collapsible sections: each TYPE is one
                <details> (native, no JS state — the v1.0.x screenshot lesson
                favors structural primitives), its models render as widget
                cards in a grid. Block layout only. */}
            {groups.map((g) => (
              <details key={g.type || '__unknown__'} className="fieldset" style={{ marginBottom: 8 }} open={groups.length === 1}>
                <summary style={{ cursor: 'pointer', fontWeight: 600, padding: '4px 0' }}>
                  {g.label} ({g.models.length} {g.models.length === 1 ? 'model' : 'models'})
                </summary>
                <div style={{ display: 'grid', gap: 10, gridTemplateColumns: 'repeat(auto-fill, minmax(260px, 1fr))', marginTop: 8 }}>
                  {g.models.length === 0 && (
                    <p className="hint" style={{ margin: 0 }}>
                      No models yet — run a sync or add one.
                    </p>
                  )}
                  {g.models.map((m) => {
                    const capMap = capsFromRaw(m.capabilities);
                    const caps = CAPABILITY_FLAGS.filter((flag) => capMap[flag] === true);
                    const state = tests[m.id];
                    return (
                      <div key={m.id} className="widget">
                        <div className="widget-head">
                          {/* A model name is variable-length text in a
                              non-wrapping row: it ellipsizes rather than
                              wrapping, so the card's height never depends on
                              where a name happens to break. */}
                          <h3 className="title" style={{ minWidth: 0, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }} title={m.name}>
                            {m.name}
                          </h3>
                          <span className={m.origin === 'synced' ? 'badge info' : 'badge paused'}>
                            <span className="b-dot" />
                            {m.origin === 'synced' ? 'synced' : 'manual'}
                          </span>
                        </div>
                        <div className="widget-body" style={{ gap: 10 }}>
                          <span className="mono hint" style={{ margin: 0, overflowWrap: 'anywhere' }}>
                            {m.id}
                          </span>
                          {/* Only the capabilities this model HAS: a column of
                              chips (facts, not state — hence the neutral
                              badge), never a wall of crosses. */}
                          <div style={{ display: 'flex', flexDirection: 'column', gap: 4, alignItems: 'flex-start' }}>
                            {caps.length === 0 ? (
                              <span className="hint" style={{ margin: 0 }}>
                                No capabilities detected yet — run a sync or check them in Edit.
                              </span>
                            ) : (
                              caps.map((flag) => (
                                <span key={flag} className="badge paused">
                                  {flag}
                                </span>
                              ))
                            )}
                          </div>
                          <div style={{ display: 'flex', gap: 6, marginTop: 'auto' }}>
                            {state?.status === 'running' ? (
                              <button type="button" className="btn btn-secondary btn-sm" disabled aria-label={`Testing ${m.name}`}>
                                <svg className="loading-spinner" style={{ width: 14, height: 14 }} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.5" aria-hidden="true">
                                  <circle cx="12" cy="12" r="9" opacity="0.25" />
                                  <path d="M21 12a9 9 0 0 0-9-9" />
                                </svg>
                                Testing…
                              </button>
                            ) : state?.status === 'ok' ? (
                              // The outcome stays: testing several models is
                              // meant to leave a visible record of which ones
                              // answered. A click runs the test again.
                              <button
                                type="button"
                                className="btn btn-secondary btn-sm"
                                style={{ color: 'var(--status-up)' }}
                                onClick={() => void runTest(m)}
                                title={`Answered in ${state.result.latency_ms} ms — click to test again`}
                              >
                                <IconCheck width="14" height="14" />
                                OK
                              </button>
                            ) : state?.status === 'failed' ? (
                              <button
                                type="button"
                                className="btn btn-secondary btn-sm"
                                style={{ color: 'var(--status-down)' }}
                                onClick={() => setTestDetail({ model: m, result: state.result })}
                                title="Failed — click for the request and the error"
                              >
                                <IconClose width="14" height="14" />
                                Failed
                              </button>
                            ) : (
                              <button
                                type="button"
                                className="btn btn-secondary btn-sm"
                                onClick={() => void runTest(m)}
                                title="One minimal 'Sag OK' round trip against this model's provider"
                              >
                                Test
                              </button>
                            )}
                            <button type="button" className="btn btn-primary btn-sm" onClick={() => openEdit(m)}>
                              Edit
                            </button>
                            <button
                              type="button"
                              className="btn btn-danger btn-sm"
                              onClick={() => void remove(m)}
                              disabled={drawerPending}
                            >
                              Delete
                            </button>
                          </div>
                        </div>
                      </div>
                    );
                  })}
                </div>
              </details>
            ))}
          </div>
        )}
      </div>

      {/* Test failure detail: the panel-sized overlay (the same one the Logs
          payloads use) — a wire body and an upstream error are JSON an
          operator reads, and the 420px rail cuts them to slivers. */}
      {testDetail !== null && (
        <Drawer
          title={`Test failed — ${testDetail.model.name}`}
          onClose={() => setTestDetail(null)}
          size="panel"
          footer={
            <>
              <button type="button" className="btn btn-secondary" onClick={() => setTestDetail(null)}>
                Close
              </button>
              <button
                type="button"
                className="btn btn-primary"
                onClick={() => {
                  const m = testDetail.model;
                  setTestDetail(null);
                  void runTest(m);
                }}
              >
                Test again
              </button>
            </>
          }
        >
          <div className="form">
            <div className="fieldset">
              <legend>Failure</legend>
              <p className="form-note" style={{ marginTop: 0 }}>
                {testDetail.result.error ?? 'Test failed'}
              </p>
              <dl style={{ display: 'grid', gridTemplateColumns: 'auto 1fr', gap: '6px 16px', margin: 0, fontSize: 'var(--fs-sm)' }}>
                <dt className="muted">Account</dt>
                <dd style={{ margin: 0, overflowWrap: 'anywhere' }}>
                  {providerNames.get(testDetail.model.provider_id) ?? testDetail.model.provider_id}
                </dd>
                <dt className="muted">Model</dt>
                <dd className="mono" style={{ margin: 0, overflowWrap: 'anywhere' }}>
                  {testDetail.model.id}
                </dd>
                <dt className="muted">Wire name</dt>
                <dd className="mono" style={{ margin: 0, overflowWrap: 'anywhere' }}>
                  {testDetail.model.name}
                </dd>
                <dt className="muted">Latency</dt>
                <dd className="num" style={{ margin: 0 }}>
                  {testDetail.result.latency_ms} ms
                </dd>
                {testDetail.result.detail?.http_status !== undefined && (
                  <>
                    <dt className="muted">Upstream status</dt>
                    <dd className="num" style={{ margin: 0 }}>
                      HTTP {testDetail.result.detail.http_status}
                    </dd>
                  </>
                )}
                {(testDetail.result.detail?.type !== undefined || testDetail.result.detail?.code !== undefined) && (
                  <>
                    <dt className="muted">Error</dt>
                    <dd className="mono" style={{ margin: 0, overflowWrap: 'anywhere' }}>
                      {[testDetail.result.detail?.type, testDetail.result.detail?.code].filter(Boolean).join(' · ')}
                    </dd>
                  </>
                )}
              </dl>
              {testDetail.result.detail?.message !== undefined && testDetail.result.detail.message !== '' && (
                <pre
                  className="mono"
                  style={{ margin: '10px 0 0', whiteSpace: 'pre-wrap', wordBreak: 'break-word', fontSize: 12, maxHeight: 220, overflow: 'auto' }}
                >
                  {testDetail.result.detail.message}
                </pre>
              )}
              {testDetail.result.detail === undefined && (
                <p className="hint" style={{ marginBottom: 0 }}>
                  No upstream response was reached — the request did not leave the proxy.
                </p>
              )}
            </div>
            <div className="fieldset">
              <legend>Sent request</legend>
              <p className="hint" style={{ marginTop: 0 }}>
                The wire body of the round trip: 16-token budget, no stream, one message.
                {testDetail.result.request === undefined || testDetail.result.request === ''
                  ? ' This provider cannot render a preview, so the body could not be captured.'
                  : ''}
              </p>
              {testDetail.result.request !== undefined && testDetail.result.request !== '' && (
                <SentRequest body={testDetail.result.request} />
              )}
            </div>
          </div>
        </Drawer>
      )}

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
              <p role="alert" className="form-note">
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
                  {llmProviders.map((p) => (
                    <option key={p.id} value={p.id}>
                      {p.name}
                    </option>
                  ))}
                  {/* A legacy row on a now-filtered provider (web-tool, or a
                      deleted account): the picker keeps offering what the row
                      actually has, so opening and saving a row never silently
                      moves it to another account. */}
                  {!llmProviders.some((p) => p.id === form.provider_id) && form.provider_id !== '' && (
                    <option value={form.provider_id}>{providerNames.get(form.provider_id) ?? form.provider_id}</option>
                  )}
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
              <span className="hint" style={{ display: 'block' }}>
                Manual selection pins each flag — the automatic learning (a real upstream
                rejection writes the observed gap) never overwrites a pinned flag.
              </span>
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

          </form>
        </Drawer>
      )}
    </>
  );
}

// SentRequest renders the captured wire body as indented JSON — the same
// viewer the Logs payloads use, for the same reason: what the provider's
// encoder produced is JSON, and reflowing it into prose would misrepresent
// it.
function SentRequest({ body }: { body: string }) {
  let pretty = body;
  try {
    pretty = JSON.stringify(JSON.parse(body), null, 2);
  } catch {
    // Not JSON (a truncation marker, or a provider that encodes otherwise):
    // show what was recorded.
  }
  return (
    <pre
      className="mono"
      style={{ margin: 0, whiteSpace: 'pre-wrap', wordBreak: 'break-word', fontSize: 12, maxHeight: 420, overflow: 'auto' }}
    >
      {pretty}
    </pre>
  );
}
