import { expect, test } from '@playwright/test';

// The Quota page shows the account's OWN limits (ADR-012): the provider-
// neutral windows as shrinking remaining bars, grouped by provider type,
// with the cooldown deadline and an honest "as of" reading. An account the
// scrape could not serve renders the empty state — never a pair of empty
// bars under a "live" badge.
//
// The endpoint is stubbed for every case: the fixture's own blob is a frozen
// snapshot, and these readings need exact inputs. Reset timestamps are placed
// mid-bucket (a 45 s pad inside the minute) so the countdown cannot cross a
// bucket boundary between the stub and the assertion.

async function login(page: import('@playwright/test').Page) {
  await page.goto('/login');
  await page.getByLabel('Username').fill('admin');
  await page.getByLabel('Password').fill('proxyllm-e2e');
  await page.getByRole('button', { name: /sign in/i }).click();
  await expect(page.getByRole('link', { name: 'Quota' })).toBeVisible();
}

interface StubWindow {
  key: string;
  label?: string;
  kind?: 'percent' | 'credits' | 'usd' | string;
  remaining_pct?: number | null;
  used_pct?: number | null;
  remaining?: number | null;
  limit?: number | null;
  unit?: string;
  unlimited?: boolean;
  reset_at?: number;
  source?: string;
  note?: string;
}

interface StubLive {
  windows?: StubWindow[];
  session_pct?: number | null;
  session_reset?: number;
  weekly_pct?: number | null;
  weekly_reset?: number;
  credits?: { remaining?: number | null; limit?: number | null; reset?: number };
}

interface StubAccount {
  account_id: string;
  type: string;
  type_label?: string;
  kind?: 'llm' | 'web_tool' | string;
  type_order?: number;
  name: string;
  enabled: boolean;
  live: StubLive | null;
  error?: string;
  health?: string;
  health_until?: number;
  cooldown?: { reason: string; until?: number; quota_until?: number; breaker_until?: number };
  fetched_at?: number;
  refreshable?: boolean;
}

function account(id: string, name: string, live: StubLive | null): StubAccount {
  return {
    account_id: id,
    type: 'ollama-cloud',
    type_label: 'Ollama Cloud',
    kind: 'llm',
    type_order: 1,
    name,
    enabled: true,
    live,
    refreshable: true,
  };
}

function windowAccount(
  id: string,
  name: string,
  windows: StubWindow[],
  extra: Partial<StubAccount> = {},
): StubAccount {
  return {
    ...account(id, name, { windows }),
    ...extra,
  };
}

async function stubUsage(page: import('@playwright/test').Page, accounts: StubAccount[]) {
  await page.route('**/api/admin/accounts/usage', (route) =>
    route.fulfill({
      status: 200,
      contentType: 'application/json',
      headers: { 'X-CSRF-Token': 'quota-e2e-csrf' },
      body: JSON.stringify({ accounts }),
    }),
  );
}

async function stubRefresh(page: import('@playwright/test').Page, body: unknown) {
  await page.route('**/api/admin/accounts/usage/refresh/**', (route) =>
    route.fulfill({
      status: 200,
      contentType: 'application/json',
      headers: { 'X-CSRF-Token': 'quota-e2e-csrf' },
      body: JSON.stringify(body),
    }),
  );
}

const card = (page: import('@playwright/test').Page, id: string) =>
  page.locator(`[data-account-id="${id}"]`);

test('the reset countdown reads in the terms an operator counts in', async ({ page }) => {
  await login(page);
  const now = Math.floor(Date.now() / 1000);
  await stubUsage(page, [
    account('acc-2h', 'Two hours out', { session_pct: 10, session_reset: now + 2 * 3600 + 30 }),
    account('acc-3h12', 'Three twelve out', {
      session_pct: 20,
      session_reset: now + 3 * 3600 + 12 * 60 + 45,
    }),
    account('acc-25h', 'A day and an hour', { session_pct: 30, session_reset: now + 25 * 3600 + 30 }),
    account('acc-30s', 'Under a minute', { session_pct: 40, session_reset: now + 45 }),
  ]);
  await page.goto('/quota');

  await expect(card(page, 'acc-2h')).toContainText('resets in 2h');
  await expect(card(page, 'acc-3h12')).toContainText('resets in 3h 12m');
  await expect(card(page, 'acc-25h')).toContainText('resets in 1d 1h');
  await expect(card(page, 'acc-30s')).toContainText('resets in <1m');
});

test('the exact reset instant rides the countdown tooltip', async ({ page }) => {
  await login(page);
  const now = Math.floor(Date.now() / 1000);
  await stubUsage(page, [
    account('acc-tip', 'Tooltip account', { session_pct: 10, session_reset: now + 2 * 3600 + 30 }),
  ]);
  await page.goto('/quota');

  const trigger = card(page, 'acc-tip').locator('.usage-meter-reset-copy');
  await trigger.hover();
  const tip = page.getByRole('tooltip');
  await expect(tip).toBeVisible();
  await expect(tip).toContainText('Session window resets');
  await expect(tip).toContainText(/\d{4}.*\d{1,2}:\d{2}:\d{2}/);

  // Keyboard parity: the same content is reachable by focus, not hover only.
  await trigger.focus();
  await expect(tip).toBeVisible();
});

test('a window without a reset carries no countdown', async ({ page }) => {
  await login(page);
  await stubUsage(page, [account('acc-nr', 'No reset', { session_pct: 55, weekly_pct: 12 })]);
  await page.goto('/quota');

  const c = card(page, 'acc-nr');
  // The provider reports "55 % used" — the card shows what is left.
  const meter = c.getByRole('meter', { name: 'Session remaining' });
  await expect(meter).toHaveAttribute('aria-valuenow', '45');
  await expect(meter).toHaveAttribute('aria-valuetext', '45% left');
  await expect(c).toContainText('Session');
  await expect(c).not.toContainText('resets in');
  await expect(page.getByRole('tooltip')).toHaveCount(0);
});

test('the remaining bar shrinks and the reading is accessible', async ({ page }) => {
  await login(page);
  const now = Math.floor(Date.now() / 1000);
  await stubUsage(page, [
    account('acc-bar', 'Bar account', { session_pct: 28.8, session_reset: now + 2 * 3600 + 30 }),
  ]);
  await page.goto('/quota');

  const c = card(page, 'acc-bar');
  const meter = c.getByRole('meter', { name: 'Session remaining' });
  await expect(meter).toHaveAttribute('aria-valuenow', '71');
  await expect(meter).toHaveAttribute('aria-valuetext', '71% left');
  await expect(c).toContainText('71% left');
  const fill = c.locator('.usage-meter-fill');
  await expect(fill).toHaveAttribute('style', /width: 71%/);
  await expect(c.locator('.usage-meter-reset-copy')).toContainText('resets in 2h');
});

test('Firecrawl credits render a remaining bar and a reset from the billing period', async ({ page }) => {
  await login(page);
  const now = Math.floor(Date.now() / 1000);
  await stubUsage(page, [
    {
      account_id: 'acc-fc',
      type: 'firecrawl',
      type_label: 'Firecrawl',
      kind: 'web_tool',
      type_order: 4,
      name: 'Firecrawl key 1',
      enabled: true,
      refreshable: true,
      live: {
        windows: [{
          key: 'credits', label: 'Credits', kind: 'credits', unit: 'credits',
          remaining: 850, limit: 1000, reset_at: now + 24 * 3600 + 30,
          source: 'firecrawl_credit_usage',
        }],
      },
      fetched_at: now - 180,
    },
  ]);
  await page.goto('/quota');

  const c = card(page, 'acc-fc');
  const meter = c.getByRole('meter', { name: 'Credits remaining' });
  await expect(meter).toHaveAttribute('aria-valuenow', '85');
  await expect(c).toContainText('850 credits left');
  await expect(c).toContainText('resets in 1d');
  await expect(c).toContainText('Updated 3m ago');
  await expect(c).toContainText('Firecrawl credits');
});

test('a reset in the past says so instead of going negative', async ({ page }) => {
  await login(page);
  const now = Math.floor(Date.now() / 1000);
  await stubUsage(page, [
    account('acc-past', 'Stale window', { session_pct: 99, session_reset: now - 60 }),
  ]);
  await page.goto('/quota');

  await expect(card(page, 'acc-past')).toContainText('resetting…');
});

test('the page groups by provider type and keeps live groups first', async ({ page }) => {
  await login(page);
  await stubUsage(page, [
    {
      account_id: 'acc-custom', type: 'custom-openai', type_label: 'Custom OpenAI-compatible',
      kind: 'llm', type_order: 0, name: 'Custom', enabled: true, live: null, refreshable: false,
    },
    windowAccount('acc-ollama', 'Ollama', [{
      key: 'session', label: 'Session', kind: 'percent', remaining_pct: 70,
      reset_at: 0, source: 'ollama_balance',
    }], { type: 'ollama-cloud', type_label: 'Ollama Cloud', kind: 'llm', type_order: 1 }),
    {
      account_id: 'acc-fc', type: 'firecrawl', type_label: 'Firecrawl', kind: 'web_tool',
      type_order: 4, name: 'Firecrawl', enabled: true, live: {
        windows: [{ key: 'credits', label: 'Credits', kind: 'credits', remaining: 10, limit: 100 }],
      },
    },
  ]);
  await page.goto('/quota');

  const titles = page.locator('.usage-group-title');
  await expect(titles).toHaveCount(3);
  // Chat groups before web tools, and a live group before an empty one.
  await expect(titles.nth(0)).toHaveText('Ollama Cloud');
  await expect(titles.nth(1)).toHaveText('Custom OpenAI-compatible');
  await expect(titles.nth(2)).toHaveText('Firecrawl');
});

test('the summary counts the pool, reporting and cooling honestly', async ({ page }) => {
  await login(page);
  const now = Math.floor(Date.now() / 1000);
  await stubUsage(page, [
    windowAccount('acc-live', 'Live', [{
      key: 'session', label: 'Session', kind: 'percent', remaining_pct: 70,
    }]),
    {
      ...account('acc-cool', 'Cooling', null),
      health: 'quota_exhausted',
      health_until: now + 2 * 3600 + 30,
      cooldown: { reason: 'quota_exhausted', quota_until: now + 2 * 3600 + 30 },
    },
    { ...account('acc-off', 'Disabled', null), enabled: false, health: 'breaker' },
  ]);
  await page.goto('/quota');

  const accountsTile = page.locator('.usage-summary-tile').nth(0);
  const reportingTile = page.locator('.usage-summary-tile').nth(1);
  const coolingTile = page.locator('.usage-summary-tile').nth(2);
  await expect(accountsTile).toContainText('3');
  await expect(accountsTile).toContainText('2 in pool');
  await expect(reportingTile).toContainText('1');
  await expect(reportingTile).toContainText('of 3 accounts');
  await expect(coolingTile).toContainText('1');
  // A disabled account is not counted as cooling.
  await expect(card(page, 'acc-cool')).toContainText('quota · resets in 2h');
  await expect(card(page, 'acc-off')).toContainText('out of the pool');
});

test('refresh reports a skipped probe as a cached reading', async ({ page }) => {
  await login(page);
  const now = Math.floor(Date.now() / 1000);
  await stubUsage(page, [
    account('acc-refresh', 'Refresh me', { session_pct: 10, session_reset: now + 2 * 3600 + 30 }),
  ]);
  await stubRefresh(page, { ok: true, skipped: true, retry_after: 42 });
  await page.goto('/quota');

  await card(page, 'acc-refresh').getByRole('button', { name: 'Refresh now' }).click();
  await expect(page.locator('.usage-page-note')).toContainText('Refreshed recently');
  await expect(page.locator('.usage-page-note')).toContainText('retry in <1m');
});

test('a provider with no usage source says so', async ({ page }) => {
  await login(page);
  await stubUsage(page, [{
    account_id: 'acc-none', type: 'custom-openai', type_label: 'Custom OpenAI-compatible',
    kind: 'llm', type_order: 0, name: 'No source', enabled: true, live: null, refreshable: false,
  }]);
  await page.goto('/quota');

  const c = card(page, 'acc-none');
  await expect(c).toContainText('does not report usage windows');
  await expect(c.getByRole('button', { name: 'Refresh now' })).toHaveCount(0);
});

test('the card carries no locally measured numbers', async ({ page }) => {
  await login(page);
  const now = Math.floor(Date.now() / 1000);
  await stubUsage(page, [
    account('acc-local', 'Limit only', { session_pct: 28.8, session_reset: now + 2 * 3600 + 30 }),
  ]);
  await page.goto('/quota');

  const c = card(page, 'acc-local');
  await expect(c).toContainText('Limit only');
  await expect(c).toContainText('Session');
  await expect(c).not.toContainText('local month');
  await expect(c).not.toContainText('requests (');
  await expect(c).not.toContainText('in /');
});
