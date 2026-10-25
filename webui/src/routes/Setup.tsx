import { useCallback, useEffect, useMemo, useState } from 'react';
import { useNavigate } from 'react-router-dom';
import { apiCall } from '../api';
import { copyText } from '../clipboard';
import OAuthSignIn from '../components/OAuthSignIn';
import { EmptyState, ErrorNote, PageHead } from '../hooks';
import { IconCheck, IconCopy } from '../icons';
import type {
  Account,
  CapabilityFallbacks,
  CreatedKey,
  Model,
  OnboardingResponse,
  OnboardingState,
  ProviderType,
  ProviderTypeDetailResponse,
  ProviderTypesResponse,
} from '../types';

// First-run wizard (ADR-020): a lazy page, not a modal — it carries forms,
// a device sign-in and a display-once key. Steps:
//   0 Welcome
//   1 First provider + a real model test
//   2 Web-search / web-fetch provider + a credential probe
//   3 Optional fallbacks (vision, web)
//   4 Aliases (wildcard / exact presets)
//   5 API key (display once)
//   6 Done
// Progress is persisted after every step; "Skip for now" dismisses the
// wizard without claiming completion. The status is recomputed from the
// live lists server-side, so deleting a provider later re-opens the card.

const STEPS = [
  { id: 'welcome', label: 'Welcome' },
  { id: 'provider', label: 'Provider' },
  { id: 'web_tools', label: 'Web tools' },
  { id: 'fallbacks', label: 'Fallbacks' },
  { id: 'routing', label: 'Aliases' },
  { id: 'key', label: 'API key' },
  { id: 'done', label: 'Done' },
] as const;

type StepId = (typeof STEPS)[number]['id'];

const ALIAS_PRESETS = ['claude-*', 'gpt-*', 'sonnet', 'opus', 'haiku', 'fable', 'proxy-chat'];

function errorMessage(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

function stringSettings(raw: unknown, fields: ProviderType['settings_fields']): Record<string, string> {
  const out: Record<string, string> = {};
  const source = (raw !== null && typeof raw === 'object' ? raw : {}) as Record<string, unknown>;
  for (const field of fields) {
    const value = source[field.key];
    out[field.key] = value === undefined || value === null ? '' : String(value);
  }
  return out;
}

function capabilityValue(model: Model, key: string): boolean {
  const caps = (model.capabilities ?? {}) as Record<string, unknown>;
  return caps[key] === true;
}

export default function Setup() {
  const navigate = useNavigate();
  const [status, setStatus] = useState<OnboardingResponse | null>(null);
  const [types, setTypes] = useState<ProviderType[]>([]);
  const [step, setStep] = useState<StepId>('welcome');
  const [completed, setCompleted] = useState<Set<string>>(() => new Set());
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [note, setNote] = useState<string | null>(null);

  // Provider step
  const [llmType, setLlmType] = useState('ollama-cloud');
  const [providerDetail, setProviderDetail] = useState<ProviderTypeDetailResponse | null>(null);
  const [settingsForm, setSettingsForm] = useState<Record<string, string>>({});
  const [accountName, setAccountName] = useState('Primary account');
  const [apiKey, setApiKey] = useState('');
  const [oauthDraftId, setOauthDraftId] = useState<string | null>(null);
  const [providerId, setProviderId] = useState<string | null>(null);
  const [models, setModels] = useState<Model[]>([]);
  const [modelTest, setModelTest] = useState<{ ok: boolean; detail: string } | null>(null);

  // Web-tools step
  const [webType, setWebType] = useState('ollama-cloud-web');
  const [webName, setWebName] = useState('Web tools account');
  const [webKey, setWebKey] = useState('');
  const [webAccountId, setWebAccountId] = useState<string | null>(null);
  const [webTest, setWebTest] = useState<{ ok: boolean; detail: string } | null>(null);

  // Fallbacks + aliases
  const [visionModel, setVisionModel] = useState('');
  const [useWebFallback, setUseWebFallback] = useState(true);
  const [aliasTarget, setAliasTarget] = useState('');
  const [aliasPresets, setAliasPresets] = useState<Set<string>>(() => new Set(['claude-*']));

  // API key
  const [keyName, setKeyName] = useState('my-client');
  const [createdKey, setCreatedKey] = useState<CreatedKey | null>(null);
  const [copied, setCopied] = useState(false);

  const llmTypes = useMemo(() => types.filter((t) => t.kind === 'llm'), [types]);
  const webTypes = useMemo(() => types.filter((t) => t.kind === 'web_tool'), [types]);
  const selectedLlm = useMemo(() => llmTypes.find((t) => t.type === llmType) ?? null, [llmTypes, llmType]);
  const selectedWeb = useMemo(() => webTypes.find((t) => t.type === webType) ?? null, [webTypes, webType]);
  const stepIndex = STEPS.findIndex((s) => s.id === step);
  const canContinue = step === 'welcome' || completed.has(step);

  const saveProgress = useCallback(
    async (next: Set<string>, extra: { dismissed?: boolean; completedAt?: number } = {}) => {
      const state = await apiCall<{ state: OnboardingState }>('PUT', '/api/admin/onboarding', {
        version: 1,
        completed: [...next],
        dismissed: extra.dismissed ?? false,
        completed_at: extra.completedAt ?? 0,
      });
      setCompleted(new Set(state.state.completed));
      return state.state;
    },
    [],
  );

  const load = useCallback(async () => {
    try {
      const [onboarding, catalog] = await Promise.all([
        apiCall<OnboardingResponse>('GET', '/api/admin/onboarding'),
        apiCall<ProviderTypesResponse>('GET', '/api/admin/provider-types'),
      ]);
      setStatus(onboarding);
      setTypes(catalog.types);
      setCompleted(new Set(onboarding.state.completed));
      const firstLlm = catalog.types.find((t) => t.kind === 'llm');
      if (firstLlm !== undefined) setLlmType(firstLlm.type);
      const firstWeb = catalog.types.find((t) => t.kind === 'web_tool');
      if (firstWeb !== undefined) setWebType(firstWeb.type);
    } catch (err) {
      setError(errorMessage(err));
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  // Load the selected type's settings + accounts.
  useEffect(() => {
    if (selectedLlm === null) return;
    let cancelled = false;
    apiCall<ProviderTypeDetailResponse>(
      'GET',
      `/api/admin/provider-types/${encodeURIComponent(selectedLlm.type)}`,
    )
      .then((detail) => {
        if (cancelled) return;
        setProviderDetail(detail);
        setSettingsForm(stringSettings(detail.settings, selectedLlm.settings_fields));
      })
      .catch((err: unknown) => {
        if (!cancelled) setError(errorMessage(err));
      });
    return () => {
      cancelled = true;
    };
  }, [selectedLlm]);

  const saveTypeSettings = async () => {
    if (selectedLlm === null || selectedLlm.settings_fields.length === 0) return;
    await apiCall('PUT', `/api/admin/provider-types/${encodeURIComponent(selectedLlm.type)}/settings`, {
      settings: settingsForm,
    });
  };

  const runSyncAndTest = async (ownerId: string) => {
    await apiCall('POST', '/api/admin/model-sync/run', {});
    const list = await apiCall<{ models: Model[] }>('GET', '/api/admin/models');
    const mine = list.models.filter((m) => m.provider_id === ownerId);
    setModels(list.models);
    const first = mine[0];
    if (first === undefined) {
      setModelTest({
        ok: false,
        detail: 'The provider returned no models. You can register one later on the Models page.',
      });
      return;
    }
    if (aliasTarget === '') setAliasTarget(first.id);
    if (visionModel === '' && capabilityValue(first, 'vision')) setVisionModel(first.id);
    const result = await apiCall<{ ok: boolean; latency_ms?: number; reply?: string; error?: string; detail?: { message?: string } }>(
      'POST',
      `/api/admin/models/test/${first.id.split('/').map(encodeURIComponent).join('/')}`,
    );
    setModelTest({
      ok: result.ok,
      detail: result.ok
        ? `Answered in ${result.latency_ms ?? 0} ms: ${result.reply ?? ''}`
        : result.error ?? result.detail?.message ?? 'The model test failed.',
    });
  };

  const createProvider = async () => {
    if (selectedLlm === null) return;
    setBusy(true);
    setError(null);
    try {
      await saveTypeSettings();
      const body: { name: string; api_key?: string } = { name: accountName.trim() || 'Primary account' };
      if (selectedLlm.auth === 'api_key' && apiKey !== '') body.api_key = apiKey;
      const created = await apiCall<{ account: Account }>(
        'POST',
        `/api/admin/provider-types/${encodeURIComponent(selectedLlm.type)}/accounts`,
        body,
      );
      setProviderId(created.account.id);
      if (selectedLlm.auth === 'oauth') {
        setOauthDraftId(created.account.id);
        return;
      }
      await runSyncAndTest(created.account.id);
      const next = new Set(completed).add('provider');
      await saveProgress(next);
      setNote('Provider connected. Review the model test, then continue.');
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  const onOAuthApproved = async () => {
    setBusy(true);
    try {
      if (oauthDraftId !== null) {
        setProviderId(oauthDraftId);
        await runSyncAndTest(oauthDraftId);
      }
      const next = new Set(completed).add('provider');
      await saveProgress(next);
      setNote('Device sign-in complete — the tokens are stored encrypted on the server.');
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  const createWebAccount = async () => {
    if (selectedWeb === null) return;
    setBusy(true);
    setError(null);
    try {
      const body: { name: string; api_key?: string } = { name: webName.trim() || 'Web tools account' };
      if (webKey !== '') body.api_key = webKey;
      const created = await apiCall<{ account: Account }>(
        'POST',
        `/api/admin/provider-types/${encodeURIComponent(selectedWeb.type)}/accounts`,
        body,
      );
      setWebAccountId(created.account.id);
      const probe = await apiCall<{ ok: boolean; skipped?: boolean; retry_after?: number }>(
        'POST',
        `/api/admin/accounts/usage/refresh/${encodeURIComponent(created.account.id)}`,
        {},
      );
      setWebTest({
        ok: probe.ok,
        detail: probe.skipped
          ? 'Credential accepted — the cached usage reading is current.'
          : 'Credential accepted and usage read.',
      });
      const next = new Set(completed).add('web_tools');
      await saveProgress(next);
    } catch (err) {
      setWebTest({ ok: false, detail: errorMessage(err) });
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  const saveFallbacks = async () => {
    setBusy(true);
    setError(null);
    try {
      const current = await apiCall<CapabilityFallbacks>('GET', '/api/admin/capability-fallbacks');
      const webList = useWebFallback && webAccountId !== null ? [webAccountId] : current.web_search;
      await apiCall('PUT', '/api/admin/capability-fallbacks', {
        vision: visionModel !== '' ? [visionModel] : current.vision,
        web_search: webList,
        web_fetch: useWebFallback && webAccountId !== null ? [webAccountId] : current.web_fetch,
      });
      const next = new Set(completed).add('fallbacks');
      await saveProgress(next);
      setNote('Fallbacks saved and live.');
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  const saveAliases = async () => {
    setBusy(true);
    setError(null);
    try {
      const settings = await apiCall<{ entries: { key: string; value: string }[] }>('GET', '/api/admin/settings');
      const raw = settings.entries.find((e) => e.key === 'model_aliases')?.value ?? '{}';
      let aliases: Record<string, string>;
      try {
        aliases = JSON.parse(raw) as Record<string, string>;
      } catch {
        aliases = {};
      }
      for (const preset of aliasPresets) {
        if (aliasTarget !== '') aliases[preset] = aliasTarget;
      }
      await apiCall('PUT', '/api/admin/settings', {
        key: 'model_aliases',
        value: JSON.stringify(aliases),
      });
      const next = new Set(completed).add('routing');
      await saveProgress(next);
      setNote('Aliases saved.');
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  const createKey = async () => {
    setBusy(true);
    setError(null);
    try {
      const created = await apiCall<CreatedKey>('POST', '/api/admin/keys', { name: keyName.trim() || 'my-client' });
      setCreatedKey(created);
      const next = new Set(completed).add('key');
      await saveProgress(next);
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  const copyKey = async () => {
    if (createdKey === null) return;
    setCopied((await copyText(createdKey.key)) === 'copied');
  };

  const finish = async () => {
    setBusy(true);
    try {
      const next = new Set(completed).add('done');
      await saveProgress(next, { completedAt: Math.floor(Date.now() / 1000) });
      navigate('/', { replace: true });
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  const skip = async () => {
    setBusy(true);
    try {
      await saveProgress(completed, { dismissed: true });
      navigate('/', { replace: true });
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  const completeStep = async (id: StepId) => {
    if (id === 'provider') {
      if (modelTest === null) {
        setError('Run the connection test before continuing.');
        return;
      }
      setStep('web_tools');
      return;
    }
    if (id === 'web_tools') {
      setStep('fallbacks');
      return;
    }
    if (id === 'fallbacks') {
      await saveFallbacks();
      setStep('routing');
      return;
    }
    if (id === 'routing') {
      await saveAliases();
      setStep('key');
      return;
    }
    if (id === 'key') {
      setStep('done');
      return;
    }
    setStep(id);
  };

  if (error !== null && status === null) {
    return <ErrorNote message={error} />;
  }
  if (status === null) {
    return <EmptyState title="Loading setup" body="Reading the instance state." />;
  }

  return (
    <>
      <PageHead
        title="Set up PROxyLLM"
        sub="Connect a provider, add web tools, then point your clients at the proxy. Every step can be skipped."
        actions={
          <button type="button" className="btn btn-ghost btn-sm" onClick={() => void skip()} disabled={busy}>
            Skip for now
          </button>
        }
      />
      <div className="canvas">
        <div className="setup-layout">
          <nav className="widget setup-steps" aria-label="Setup steps">
            <div className="widget-body">
              <ol>
                {STEPS.map((s, i) => {
                  const done = completed.has(s.id) ||
                    (s.id === 'done' && (status.state.completed_at ?? 0) > 0);
                  return (
                    <li key={s.id} className={s.id === step ? 'is-current' : ''}>
                      <button
                        type="button"
                        className="setup-step"
                        aria-current={s.id === step ? 'step' : undefined}
                        onClick={() => setStep(s.id)}
                      >
                        <span className={`setup-step-num${done ? ' is-done' : ''}`}>
                          {done ? <IconCheck width="12" height="12" /> : i + 1}
                        </span>
                        <span>{s.label}</span>
                      </button>
                    </li>
                  );
                })}
              </ol>
              <p className="hint setup-counts">
                {status.counts.llm_providers} chat provider(s), {status.counts.web_tools} web-tool(s),{' '}
                {status.counts.models} model(s), {status.counts.client_keys} key(s).
              </p>
            </div>
          </nav>

          <section className="widget setup-card" aria-live="polite">
            <div className="widget-body">
              {error !== null && (
                <p role="alert" className="form-note">
                  {error}
                </p>
              )}
              {note !== null && (
                <p role="status" className="form-note">
                  {note}
                </p>
              )}

              {step === 'welcome' && (
                <div className="form">
                  <h2 className="title">Welcome</h2>
                  <p className="sub">
                    Four short steps: connect a provider, add web search/fetch, choose fallbacks, and
                    create a client key. The wizard records progress, so you can leave and come back.
                  </p>
                  <div className="form-actions">
                    <button type="button" className="btn btn-primary" onClick={() => setStep('provider')}>
                      Start
                    </button>
                  </div>
                </div>
              )}

              {step === 'provider' && (
                <div className="form">
                  <h2 className="title">First provider</h2>
                  <p className="sub">Add one chat provider and prove the connection with a real model test.</p>
                  <div className="form-row">
                    <label className="field">
                      <span>Provider type</span>
                      <select className="select" value={llmType} onChange={(e) => setLlmType(e.target.value)}>
                        {llmTypes.map((t) => (
                          <option key={t.type} value={t.type}>{t.label}</option>
                        ))}
                      </select>
                    </label>
                    <label className="field">
                      <span>Account name</span>
                      <input className="input" value={accountName} onChange={(e) => setAccountName(e.target.value)} />
                    </label>
                  </div>
                  {selectedLlm?.settings_fields.map((f) => (
                    <label className="field" key={f.key}>
                      <span>{f.label}{f.required ? ' *' : ''}</span>
                      <input
                        className="input"
                        type={f.kind === 'url' ? 'url' : 'text'}
                        value={settingsForm[f.key] ?? ''}
                        onChange={(e) => setSettingsForm((s) => ({ ...s, [f.key]: e.target.value }))}
                      />
                      <span className="hint">{f.hint}</span>
                    </label>
                  ))}
                  {selectedLlm?.auth === 'api_key' && (
                    <label className="field">
                      <span>API key</span>
                      <input className="input" type="password" value={apiKey} onChange={(e) => setApiKey(e.target.value)} />
                      <span className="hint">Stored encrypted on the server; never shown again.</span>
                    </label>
                  )}
                  {providerDetail !== null && providerDetail.accounts.length > 0 && (
                    <p className="hint">This type already has {providerDetail.accounts.length} account(s); the wizard adds another.</p>
                  )}
                  {oauthDraftId === null ? (
                    <div className="form-actions">
                      <button type="button" className="btn btn-primary" onClick={() => void createProvider()} disabled={busy}>
                        {busy ? 'Connecting…' : 'Connect and test'}
                      </button>
                    </div>
                  ) : (
                    <OAuthSignIn providerId={oauthDraftId} onApproved={() => void onOAuthApproved()} />
                  )}
                  {modelTest !== null && (
                    <p role={modelTest.ok ? 'status' : 'alert'} className="form-note">
                      {modelTest.ok ? 'Model test passed: ' : 'Model test failed: '}{modelTest.detail}
                    </p>
                  )}
                  <div className="form-actions">
                    <button type="button" className="btn btn-ghost" onClick={() => void completeStep('provider')} disabled={busy}>
                      {modelTest?.ok === true ? 'Continue' : 'Continue without a passing test'}
                    </button>
                  </div>
                </div>
              )}

              {step === 'web_tools' && (
                <div className="form">
                  <h2 className="title">Web search and fetch</h2>
                  <p className="sub">Optional: a web-tool account answers the emulated web_search and web_fetch calls.</p>
                  <div className="form-row">
                    <label className="field">
                      <span>Provider type</span>
                      <select className="select" value={webType} onChange={(e) => setWebType(e.target.value)}>
                        {webTypes.map((t) => (
                          <option key={t.type} value={t.type}>{t.label}</option>
                        ))}
                      </select>
                    </label>
                    <label className="field">
                      <span>Account name</span>
                      <input className="input" value={webName} onChange={(e) => setWebName(e.target.value)} />
                    </label>
                  </div>
                  <label className="field">
                    <span>API key</span>
                    <input className="input" type="password" value={webKey} onChange={(e) => setWebKey(e.target.value)} />
                    <span className="hint">The credential probe reads usage; it does not run a search.</span>
                  </label>
                  <div className="form-actions">
                    <button type="button" className="btn btn-primary" onClick={() => void createWebAccount()} disabled={busy}>
                      {busy ? 'Checking…' : 'Add and check'}
                    </button>
                    <button type="button" className="btn btn-ghost" onClick={() => setStep('fallbacks')} disabled={busy}>
                      Skip this step
                    </button>
                  </div>
                  {webTest !== null && (
                    <p role={webTest.ok ? 'status' : 'alert'} className="form-note">
                      {webTest.detail}
                    </p>
                  )}
                  <div className="form-actions">
                    <button type="button" className="btn btn-ghost" onClick={() => void completeStep('web_tools')} disabled={busy}>
                      Continue
                    </button>
                  </div>
                </div>
              )}

              {step === 'fallbacks' && (
                <div className="form">
                  <h2 className="title">Fallbacks</h2>
                  <p className="sub">Optional: choose a vision describer and point the web tools at the account from the previous step.</p>
                  <label className="field">
                    <span>Vision fallback model</span>
                    <select className="select" value={visionModel} onChange={(e) => setVisionModel(e.target.value)}>
                      <option value="">— none —</option>
                      {models.map((m) => (
                        <option key={m.id} value={m.id}>{m.id}</option>
                      ))}
                    </select>
                    <span className="hint">Blind models are captioned by this model before dispatch.</span>
                  </label>
                  <label className="check">
                    <input type="checkbox" checked={useWebFallback} onChange={(e) => setUseWebFallback(e.target.checked)} />
                    <span className="cbox" />
                    <span>Use the web-tool account for web_search and web_fetch</span>
                  </label>
                  <div className="form-actions">
                    <button type="button" className="btn btn-primary" onClick={() => void saveFallbacks()} disabled={busy}>
                      {busy ? 'Saving…' : 'Save fallbacks'}
                    </button>
                    <button type="button" className="btn btn-ghost" onClick={() => setStep('routing')} disabled={busy}>
                      Skip
                    </button>
                  </div>
                </div>
              )}

              {step === 'routing' && (
                <div className="form">
                  <h2 className="title">Aliases</h2>
                  <p className="sub">Point client model names at the provider model you tested. The wildcard covers versioned Claude Code IDs.</p>
                  <label className="field">
                    <span>Alias target</span>
                    <select className="select" value={aliasTarget} onChange={(e) => setAliasTarget(e.target.value)}>
                      <option value="">— choose a model —</option>
                      {models.map((m) => (
                        <option key={m.id} value={m.id}>{m.id}</option>
                      ))}
                    </select>
                  </label>
                  <div className="setup-presets">
                    {ALIAS_PRESETS.map((preset) => (
                      <label className="check" key={preset}>
                        <input
                          type="checkbox"
                          checked={aliasPresets.has(preset)}
                          onChange={(e) => {
                            setAliasPresets((prev) => {
                              const next = new Set(prev);
                              if (e.target.checked) next.add(preset);
                              else next.delete(preset);
                              return next;
                            });
                          }}
                        />
                        <span className="cbox" />
                        <span className="mono">{preset}</span>
                      </label>
                    ))}
                  </div>
                  <div className="form-actions">
                    <button type="button" className="btn btn-primary" onClick={() => void saveAliases()} disabled={busy || aliasTarget === ''}>
                      {busy ? 'Saving…' : 'Save aliases'}
                    </button>
                    <button type="button" className="btn btn-ghost" onClick={() => setStep('key')} disabled={busy}>
                      Skip
                    </button>
                  </div>
                </div>
              )}

              {step === 'key' && (
                <div className="form">
                  <h2 className="title">Client API key</h2>
                  <p className="sub">Clients authenticate with this key. It is shown once.</p>
                  <label className="field">
                    <span>Key name</span>
                    <input className="input" value={keyName} onChange={(e) => setKeyName(e.target.value)} />
                  </label>
                  <div className="form-actions">
                    <button type="button" className="btn btn-primary" onClick={() => void createKey()} disabled={busy || createdKey !== null}>
                      {busy ? 'Creating…' : 'Create key'}
                    </button>
                    <button type="button" className="btn btn-ghost" onClick={() => setStep('done')} disabled={busy}>
                      Skip
                    </button>
                  </div>
                  {createdKey !== null && (
                    <div className="field">
                      <span className="badge up">Created — copy it now</span>
                      <div className="setup-key">
                        <input className="input mono" readOnly value={createdKey.key} onFocus={(e) => e.target.select()} />
                        <button type="button" className="btn btn-secondary btn-sm" onClick={() => void copyKey()}>
                          {copied ? <IconCheck width="14" height="14" /> : <IconCopy width="14" height="14" />}
                          {copied ? 'Copied' : 'Copy'}
                        </button>
                      </div>
                      <span className="hint">Use it as ANTHROPIC_AUTH_TOKEN or Authorization: Bearer.</span>
                    </div>
                  )}
                </div>
              )}

              {step === 'done' && (
                <div className="form">
                  <h2 className="title">Ready</h2>
                  <p className="sub">
                    {providerId !== null || status.counts.llm_providers > 0 ? 'A chat provider is connected. ' : ''}
                    {models.length > 0 || status.counts.models > 0
                      ? `${Math.max(models.length, status.counts.models)} model(s) registered. `
                      : ''}
                    {createdKey !== null || status.counts.client_keys > 0
                      ? `${Math.max(createdKey !== null ? 1 : 0, status.counts.client_keys)} client key(s) exist.`
                      : ''}
                  </p>
                  <p className="hint">You can revisit any step, or finish and open the console.</p>
                  <div className="form-actions">
                    <button type="button" className="btn btn-primary" onClick={() => void finish()} disabled={busy}>
                      Finish setup
                    </button>
                  </div>
                </div>
              )}

              {step !== 'welcome' && step !== 'done' && canContinue && stepIndex < STEPS.length - 1 && (
                <div className="form-actions setup-next">
                  <button
                    type="button"
                    className="btn btn-secondary"
                    onClick={() => void completeStep(step)}
                    disabled={busy}
                  >
                    Continue to {STEPS[stepIndex + 1]?.label.toLowerCase()}
                  </button>
                </div>
              )}
            </div>
          </section>
        </div>
      </div>
    </>
  );
}
