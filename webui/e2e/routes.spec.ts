// Route screenshots: one per route, against the seeded fixture server.
// One test per route so a diff names its
// route precisely; the login page is captured signed-out, everything else
// signed-in through the real form.

import { test, expect } from '@playwright/test';
import { freezeTime, login, readyForCapture, kpiMasks } from './helpers';

test.beforeEach(async ({ page }) => {
  await freezeTime(page);
});

test.describe('signed out', () => {
  test('login', async ({ page }) => {
    await page.goto('/login');
    await readyForCapture(page);
    await expect(page).toHaveScreenshot('login.png');
  });
});

test.describe('signed in', () => {
  test.beforeEach(async ({ page }) => {
    await login(page);
  });

  test('overview', async ({ page }) => {
    await page.goto('/');
    await readyForCapture(page);
    await expect(page).toHaveScreenshot('overview.png', { mask: kpiMasks(page) });
  });

  // ADR-012 Quota page: per-account usage cards. The reset timestamps
  // render from the seeded blob (frozen clock) — masked along with the
  // percent values to stay robust against seed drift.
  test('quota', async ({ page }) => {
    await page.goto('/quota');
    await readyForCapture(page);
    await expect(page).toHaveScreenshot('quota.png', {
      mask: [...kpiMasks(page), page.locator('.usage-meter-fill')],
    });
  });

  test('providers', async ({ page }) => {
    await page.goto('/providers');
    await readyForCapture(page);
    await expect(page).toHaveScreenshot('providers.png');
  });

  // ADR-009: the per-type settings page — accounts list, credential
  // badges, connection settings form. Captured for the oauth type (the
  // codex card's destination) so both badge states render.
  test('provider-type', async ({ page }) => {
    await page.goto('/providers/codex-oauth');
    await readyForCapture(page);
    await expect(page).toHaveScreenshot('provider-type.png');
  });

  test('models', async ({ page }) => {
    await page.goto('/models');
    await readyForCapture(page);
    await expect(page).toHaveScreenshot('models.png');
  });

  // 2026-09-22: the per-model Test button against the scripted prv-test
  // account — a deterministic "OK" answer (fixtureTestProvider), no live
  // upstream.
  //
  // 2026-10-17: the card is addressed by its MODEL ID. The page groups by
  // provider TYPE, two fixture accounts share the custom-openai type, and the
  // account no longer rides the card at all (a model is a property of the
  // type; which account serves it is routing's business). The group starts
  // collapsed (several fixture groups), so open it first.
  test('models test button', async ({ page }) => {
    await page.goto('/models');
    await readyForCapture(page);
    const group = page.locator('details', { hasText: 'Custom OpenAI-compatible' });
    await group.locator('summary').first().click();
    const card = group.locator('.widget', { hasText: 'mdl-test' });
    await card.getByRole('button', { name: 'Test', exact: true }).click();
    // The outcome stays on the card: a check, not a transient note.
    await expect(card.getByRole('button', { name: 'OK' })).toBeVisible();
    await expect(page).toHaveScreenshot('models-test.png');
  });

  test('routing', async ({ page }) => {
    await page.goto('/routing');
    await readyForCapture(page);
    await expect(page).toHaveScreenshot('routing.png');
  });

  test('logs', async ({ page }) => {
    await page.goto('/logs');
    await readyForCapture(page);
    // Timestamps render from the frozen clock + seeded relative offsets —
    // deterministic in CI, but masked to stay robust against seed drift.
    // The gutter (first-child) keeps the chain lines out of the baseline.
    await expect(page).toHaveScreenshot('logs.png', {
      mask: [page.locator('td:first-child'), page.locator('td.col-time'), ...kpiMasks(page)],
    });
  });

  test('keys', async ({ page }) => {
    await page.goto('/keys');
    await readyForCapture(page);
    // The fixture seeds keys through the real store, whose timestamps come
    // from the wall clock — mask the Created/Last-used columns so the
    // baseline pins the layout and labels, not the seed's clock.
    await expect(page).toHaveScreenshot('keys.png', {
      mask: [page.locator('.table tbody td:nth-child(3)'), page.locator('.table tbody td:nth-child(4)')],
    });
  });

});