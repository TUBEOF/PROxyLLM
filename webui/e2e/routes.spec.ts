// Route screenshots: every §9 route against the seeded fixture server
// (research §7 V-1/V-2/V-3/V-7). One test per route so a diff names its
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

  test('providers', async ({ page }) => {
    await page.goto('/providers');
    await readyForCapture(page);
    await expect(page).toHaveScreenshot('providers.png');
  });

  test('models', async ({ page }) => {
    await page.goto('/models');
    await readyForCapture(page);
    await expect(page).toHaveScreenshot('models.png');
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
    await expect(page).toHaveScreenshot('logs.png', {
      mask: [page.locator('td:first-child'), ...kpiMasks(page)],
    });
  });

  test('keys', async ({ page }) => {
    await page.goto('/keys');
    await readyForCapture(page);
    await expect(page).toHaveScreenshot('keys.png');
  });

  test('settings', async ({ page }) => {
    await page.goto('/settings');
    await readyForCapture(page);
    await expect(page).toHaveScreenshot('settings.png');
  });
});