import { expect, test } from '@playwright/test';

// The Quota page shows the account's OWN limits (ADR-012 amendment
// 2026-10-17): the scraped windows with a reset countdown, and nothing the
// proxy measured itself. An account the scrape could not serve renders the
// empty state — never a pair of empty bars under a "live" badge.
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

interface StubLive {
  session_pct?: number | null;
  session_reset?: number;
  weekly_pct?: number | null;
  weekly_reset?: number;
  credits?: { remaining?: number | null; limit?: number | null; reset?: number };
}

interface StubAccount {
  account_id: string;
  type: string;
  name: string;
  enabled: boolean;
  live: StubLive | null;
  refreshable?: boolean;
}

function account(id: string, name: string, live: StubLive | null): StubAccount {
  return { account_id: id, type: 'ollama-cloud', name, enabled: true, live, refreshable: true };
}

async function stubUsage(page: import('@playwright/test').Page, accounts: StubAccount[]) {
  await page.route('**/api/admin/accounts/usage', (route) =>
    route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({ accounts }),
    }),
  );
}

const card = (page: import('@playwright/test').Page, name: string) =>
  page.locator('.widget').filter({ hasText: name }).first();

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

  await expect(card(page, 'Two hours out')).toContainText('resets in 2h');
  await expect(card(page, 'Three twelve out')).toContainText('resets in 3h 12m');
  await expect(card(page, 'A day and an hour')).toContainText('resets in 1d 1h');
  await expect(card(page, 'Under a minute')).toContainText('resets in <1m');
});

test('the exact reset instant rides the countdown tooltip', async ({ page }) => {
  await login(page);
  const now = Math.floor(Date.now() / 1000);
  await stubUsage(page, [
    account('acc-tip', 'Tooltip account', { session_pct: 10, session_reset: now + 2 * 3600 + 30 }),
  ]);
  await page.goto('/quota');

  const trigger = card(page, 'Tooltip account').locator('.tooltip');
  await trigger.hover();
  const tip = page.getByRole('tooltip');
  await expect(tip).toBeVisible();
  await expect(tip).toContainText('Session window resets');
  // The value is the browser's locale and timezone — CI's zone is not a
  // contract — so the assertion pins the shape: a year, then a clock time
  // with seconds.
  await expect(tip).toContainText(/\d{4}.*\d{1,2}:\d{2}:\d{2}/);

  // Keyboard parity: the same content is reachable by focus, not hover only.
  await trigger.focus();
  await expect(tip).toBeVisible();
});

test('a window without a reset carries no countdown', async ({ page }) => {
  await login(page);
  await stubUsage(page, [account('acc-nr', 'No reset', { session_pct: 55, weekly_pct: 12 })]);
  await page.goto('/quota');

  const c = card(page, 'No reset');
  // The provider reports "55 % used" — the card shows what is left.
  await expect(c).toContainText('45% left');
  await expect(
    c.locator('.usage-window').filter({ hasText: 'Session' }).locator('.usage-ring-num'),
  ).toHaveText('45%');
  await expect(c).toContainText('Session');
  // Absence means something only against a rendered card: the ring is there,
  // the countdown is not, and no tooltip was invented for the unknown reset.
  await expect(c).not.toContainText('resets in');
  await expect(page.getByRole('tooltip')).toHaveCount(0);
});

test('the remaining percent is centered and the reset timer is its own column', async ({ page }) => {
  await login(page);
  const now = Math.floor(Date.now() / 1000);
  await stubUsage(page, [
    account('acc-ring', 'Ring account', { session_pct: 28.8, session_reset: now + 2 * 3600 + 30 }),
  ]);
  await page.goto('/quota');

  const c = card(page, 'Ring account');
  await expect(c.locator('.usage-ring-num')).toHaveText('71%');
  await expect(c).toContainText('71% left');
  const timer = c.locator('.usage-reset .tooltip');
  await expect(timer).toContainText('resets in 2h');
});

test('Firecrawl credits render a remaining ring and a reset from the billing period', async ({ page }) => {
  await login(page);
  const now = Math.floor(Date.now() / 1000);
  await stubUsage(page, [
    {
      account_id: 'acc-fc',
      type: 'firecrawl',
      name: 'Firecrawl key 1',
      enabled: true,
      refreshable: true,
      live: { credits: { remaining: 850, limit: 1000, reset: now + 24 * 3600 + 30 } },
    },
  ]);
  await page.goto('/quota');

  const c = card(page, 'Firecrawl key 1');
  await expect(c.locator('.usage-ring-num')).toHaveText('85%');
  await expect(c).toContainText('850 credits left');
  await expect(c).toContainText('resets in 1d');
});

test('a reset in the past says so instead of going negative', async ({ page }) => {
  await login(page);
  const now = Math.floor(Date.now() / 1000);
  await stubUsage(page, [
    account('acc-past', 'Stale window', { session_pct: 99, session_reset: now - 60 }),
  ]);
  await page.goto('/quota');

  await expect(card(page, 'Stale window')).toContainText('resetting…');
});

test('the card carries no locally measured numbers', async ({ page }) => {
  await login(page);
  const now = Math.floor(Date.now() / 1000);
  await stubUsage(page, [
    account('acc-local', 'Limit only', { session_pct: 28.8, session_reset: now + 2 * 3600 + 30 }),
  ]);
  await page.goto('/quota');

  const c = card(page, 'Limit only');
  // Positive anchors first — the rendered card — then the absence.
  await expect(c).toContainText('Limit only');
  await expect(c).toContainText('Session');
  await expect(c).not.toContainText('local month');
  await expect(c).not.toContainText('requests (');
  await expect(c).not.toContainText('in /');
});
