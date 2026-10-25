import { expect, test } from '@playwright/test';

// The first-run wizard (ADR-020). The server status is recomputed from the
// live lists; the spec stubs it and every write so each step can be driven
// deterministically. The real server's CSRF re-issue is mirrored on the
// stubbed GETs (a full-page navigation loses the in-memory token).

async function login(page: import('@playwright/test').Page) {
  await page.goto('/login');
  await page.getByLabel('Username').fill('admin');
  await page.getByLabel('Password').fill('proxyllm-e2e');
  await page.getByRole('button', { name: /sign in/i }).click();
  await expect(page.getByRole('link', { name: 'Quota' })).toBeVisible();
}

const catalog = {
  types: [
    {
      type: 'ollama-cloud', label: 'Ollama Cloud', description: 'Hosted Ollama models.',
      kind: 'llm', auth: 'api_key', settings_fields: [], account_fields: [],
      usage_cookie: true, default_capabilities: { streaming: true, tools: true },
    },
    {
      type: 'ollama-cloud-web', label: 'Ollama Cloud (Web Tools)', description: 'Hosted search and fetch.',
      kind: 'web_tool', auth: 'api_key', settings_fields: [], account_fields: [],
      usage_cookie: true, default_capabilities: null,
    },
    {
      type: 'codex-oauth', label: 'ChatGPT / Codex', description: 'Device sign-in.',
      kind: 'llm', auth: 'oauth',
      settings_fields: [{ key: 'issuer', label: 'Issuer', kind: 'url', required: true, hint: 'OAuth issuer' }],
      account_fields: [], usage_cookie: false, default_capabilities: { streaming: true, tools: true },
    },
  ],
};

const model = {
  id: 'ollama-cloud/test-model', provider_id: 'acc-1', name: 'test-model',
  capabilities: { vision: true }, compat: {}, priority: 0, origin: 'synced',
};

type Captured = { method: string; path: string; body: unknown };

async function stubWizard(page: import('@playwright/test').Page) {
  const captured: Captured[] = [];
  let needsSetup = true;
  let completed: string[] = [];
  let dismissed = false;

  await page.route('**/api/admin/**', async (route) => {
    const req = route.request();
    const url = new URL(req.url());
    const path = url.pathname;
    const method = req.method();
    const body = req.postData() !== null ? JSON.parse(req.postData() as string) : null;
    const ok = (payload: unknown) =>
      route.fulfill({
        status: 200,
        contentType: 'application/json',
        headers: { 'X-CSRF-Token': 'setup-e2e-csrf' },
        body: JSON.stringify(payload),
      });

    if (path === '/api/admin/onboarding' && method === 'GET') {
      return ok({
        needs_setup: needsSetup,
        state: { version: 1, completed, dismissed, completed_at: dismissed ? 1 : 0 },
        counts: {
          providers: completed.includes('provider') ? 1 : 0,
          llm_providers: completed.includes('provider') ? 1 : 0,
          web_tools: completed.includes('web_tools') ? 1 : 0,
          models: completed.includes('provider') ? 1 : 0,
          routing_rules: 0,
          client_keys: completed.includes('key') ? 1 : 0,
        },
      });
    }
    if (path === '/api/admin/onboarding' && method === 'PUT') {
      captured.push({ method, path, body });
      const payload = body as { completed: string[]; dismissed: boolean; completed_at: number };
      completed = payload.completed;
      dismissed = payload.dismissed;
      if (dismissed || (payload.completed_at ?? 0) > 0) needsSetup = false;
      return ok({ state: { version: 1, completed, dismissed, completed_at: payload.completed_at } });
    }
    if (path === '/api/admin/provider-types' && method === 'GET') return ok(catalog);
    if (path === '/api/admin/provider-types/ollama-cloud' && method === 'GET') {
      return ok({ settings: {}, accounts: [] });
    }
    if (path === '/api/admin/provider-types/codex-oauth' && method === 'GET') {
      return ok({ settings: {}, accounts: [] });
    }
    if (path.startsWith('/api/admin/provider-types/') && path.endsWith('/settings') && method === 'PUT') {
      captured.push({ method, path, body });
      return ok({ ok: true });
    }
    if (path === '/api/admin/provider-types/ollama-cloud/accounts' && method === 'POST') {
      captured.push({ method, path, body });
      return ok({ account: { id: 'acc-1', type: 'ollama-cloud', name: 'Primary', has_credential: true, enabled: true } });
    }
    if (path === '/api/admin/provider-types/ollama-cloud-web/accounts' && method === 'POST') {
      captured.push({ method, path, body });
      return ok({ account: { id: 'web-1', type: 'ollama-cloud-web', name: 'Web', has_credential: true, enabled: true } });
    }
    if (path === '/api/admin/provider-types/codex-oauth/accounts' && method === 'POST') {
      captured.push({ method, path, body });
      return ok({ account: { id: 'codex-1', type: 'codex-oauth', name: 'Codex', has_credential: false, enabled: true } });
    }
    if (path === '/api/admin/model-sync/run' && method === 'POST') {
      return ok({ last_run: { ran_at: 1, providers: [{ provider_id: 'acc-1', added: 1 }] } });
    }
    if (path === '/api/admin/models' && method === 'GET') return ok({ models: [model] });
    if (path.startsWith('/api/admin/models/test/') && method === 'POST') {
      return ok({ ok: true, latency_ms: 12, reply: 'OK' });
    }
    if (path === '/api/admin/accounts/usage/refresh/web-1' && method === 'POST') return ok({ ok: true });
    if (path === '/api/admin/capability-fallbacks' && method === 'GET') {
      return ok({ vision: [], web_search: [], web_fetch: [] });
    }
    if (path === '/api/admin/capability-fallbacks' && method === 'PUT') {
      captured.push({ method, path, body });
      return ok({ ok: true });
    }
    if (path === '/api/admin/settings' && method === 'GET') return ok({ entries: [] });
    if (path === '/api/admin/settings' && method === 'PUT') {
      captured.push({ method, path, body });
      return ok({ ok: true });
    }
    if (path === '/api/admin/keys' && method === 'POST') {
      captured.push({ method, path, body });
      return ok({ id: 'k1', key: 'pk_setup_test_secret' });
    }
    return route.continue();
  });

  return captured;
}

test('the wizard walks a fresh instance through provider, web tools, fallbacks, aliases and key', async ({ page }) => {
  await login(page);
  const captured = await stubWizard(page);
  await page.goto('/setup');

  await expect(page.getByRole('heading', { name: 'Welcome' })).toBeVisible();
  await page.getByRole('button', { name: 'Start' }).click();

  // Provider + real model test.
  await page.getByLabel('Account name').fill('Primary');
  await page.getByLabel('API key').fill('ollama-key');
  await page.getByRole('button', { name: 'Connect and test' }).click();
  await expect(page.getByText(/Model test passed/)).toBeVisible();
  await page.getByRole('button', { name: 'Continue', exact: true }).click();

  // Web-tool account + credential probe.
  await page.getByLabel('Account name').fill('Web');
  await page.getByLabel('API key').fill('ollama-key');
  await page.getByRole('button', { name: 'Add and check' }).click();
  await expect(page.getByText(/Credential accepted/)).toBeVisible();
  await page.getByRole('button', { name: 'Continue', exact: true }).click();

  // Optional fallbacks.
  await page.getByLabel('Vision fallback model').selectOption(model.id);
  await page.getByRole('button', { name: 'Save fallbacks' }).click();
  await expect(page.getByText(/Fallbacks saved/)).toBeVisible();
  await page.getByRole('button', { name: /Continue to aliases/i }).click();

  // Aliases.
  await page.getByRole('button', { name: 'Save aliases' }).click();
  await expect(page.getByText(/Aliases saved/)).toBeVisible();
  await page.getByRole('button', { name: /Continue to api key/i }).click();

  // Key, shown once.
  await page.getByRole('button', { name: 'Create key' }).click();
  await expect(page.locator('.setup-key input')).toHaveValue('pk_setup_test_secret');
  await page.getByRole('button', { name: /Continue to done/i }).click();
  await page.getByRole('button', { name: 'Finish setup' }).click();
  await expect(page).toHaveURL('/');
  await expect(page.getByRole('heading', { name: 'Finish setup' })).toHaveCount(0);

  const fallbackPut = captured.find((c) => c.path === '/api/admin/capability-fallbacks');
  expect(JSON.stringify(fallbackPut?.body)).toContain('web-1');
  expect(JSON.stringify(fallbackPut?.body)).toContain(model.id);
  const aliasPut = captured.find((c) => c.path === '/api/admin/settings');
  expect(JSON.stringify(aliasPut?.body)).toContain('claude-*');
  const progress = captured.filter((c) => c.path === '/api/admin/onboarding');
  expect(JSON.stringify(progress.at(-1)?.body)).toContain('"done"');
});

test('Skip for now dismisses the wizard without claiming completion', async ({ page }) => {
  await login(page);
  const captured = await stubWizard(page);
  await page.goto('/setup');
  await page.getByRole('button', { name: 'Skip for now' }).click();
  await expect(page).toHaveURL('/');
  const put = captured.filter((c) => c.path === '/api/admin/onboarding').at(-1);
  expect(JSON.stringify(put?.body)).toContain('"dismissed":true');
});

test('the OAuth provider path creates a draft and offers device sign-in', async ({ page }) => {
  await login(page);
  await stubWizard(page);
  await page.goto('/setup');
  await page.getByRole('button', { name: 'Start' }).click();
  await page.getByLabel('Provider type').selectOption('codex-oauth');
  await page.getByLabel('Account name').fill('Codex');
  await page.getByRole('button', { name: 'Connect and test' }).click();
  await expect(page.getByRole('button', { name: 'Start device sign-in' })).toBeVisible();
});
