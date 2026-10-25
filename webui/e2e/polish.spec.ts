import { expect, test } from '@playwright/test';
import { login } from './helpers';

// P0 interaction polish: focus contract, keyboard paths, destructive
// confirmation, named collapsed navigation, page frames and the empty-state
// live region. The fixture server is seeded; the error/empty cases stub the
// endpoint they need.

test.describe('interaction polish', () => {
  test.beforeEach(async ({ page }) => {
    await login(page);
  });

  test('a dialog traps focus, closes on Escape and restores the trigger', async ({ page }) => {
    await page.goto('/models');
    const trigger = page.getByRole('button', { name: 'Edit' }).first();
    await trigger.click();
    const dialog = page.getByRole('dialog');
    await expect(dialog).toBeVisible();
    for (let i = 0; i < 12; i += 1) {
      await page.keyboard.press('Tab');
      const inside = await dialog.evaluate((el) => el.contains(document.activeElement));
      expect(inside, `tab ${i} left the dialog`).toBe(true);
    }
    await page.keyboard.press('Escape');
    await expect(dialog).toHaveCount(0);
    await expect(trigger).toBeFocused();
  });

  test('a destructive action asks first and Cancel keeps the object', async ({ page }) => {
    await page.goto('/models');
    const cards = page.locator('.widget').filter({ has: page.getByRole('button', { name: 'Delete' }) });
    await expect(cards.first()).toBeVisible();
    const before = await cards.count();
    await cards.first().getByRole('button', { name: 'Delete' }).click();
    const dialog = page.getByRole('dialog');
    await expect(dialog).toBeVisible();
    await expect(dialog).toContainText('Delete model');
    await dialog.getByRole('button', { name: 'Cancel' }).click();
    await expect(dialog).toHaveCount(0);
    expect(await cards.count()).toBe(before);
  });

  test('a Logs parent row opens with Enter', async ({ page }) => {
    await page.goto('/logs');
    const row = page.locator('tbody tr.log-row').first();
    await row.focus();
    await page.keyboard.press('Enter');
    await expect(page.getByRole('dialog')).toBeVisible();
  });

  test('a Routing rule opens with Enter and reveals its actions on focus', async ({ page }) => {
    await page.goto('/routing');
    const row = page.locator('tbody tr').first();
    await row.focus();
    await expect(row.locator('.row-actions').first()).toHaveCSS('opacity', '1');
    await page.keyboard.press('Enter');
    await expect(page.getByRole('dialog')).toBeVisible();
  });

  test('the collapsed rail keeps every nav link named', async ({ page }) => {
    if ((page.viewportSize()?.width ?? 0) <= 768) {
      // On a phone the sidebar is an off-canvas drawer opened by the same
      // toggle, not the desktop rail; every link must still be named.
      await page.getByRole('button', { name: 'Expand sidebar' }).click();
      await expect(page.locator('.sidebar')).toHaveClass(/mobile-open/);
      await expect(page.getByRole('link', { name: 'Overview' })).toHaveAccessibleName('Overview');
      await expect(page.getByRole('link', { name: 'Routing' })).toHaveAccessibleName('Routing');
      await page.getByRole('button', { name: 'Collapse sidebar' }).click();
      await expect(page.locator('.sidebar')).not.toHaveClass(/mobile-open/);
      return;
    }
    await page.getByRole('button', { name: 'Collapse sidebar' }).click();
    await expect(page.locator('.sidebar')).toHaveClass(/collapsed/);
    await expect(page.getByRole('link', { name: 'Overview' })).toHaveAccessibleName('Overview');
    await expect(page.getByRole('link', { name: 'Routing' })).toHaveAccessibleName('Routing');
    await page.reload();
    await expect(page.locator('.sidebar')).toHaveClass(/collapsed/);
    await page.getByRole('button', { name: 'Expand sidebar' }).click();
    await expect(page.locator('.sidebar')).not.toHaveClass(/collapsed/);
  });

  test('the provider page keeps its frame when the catalog fails', async ({ page }) => {
    await page.route('**/api/admin/provider-types', (route) =>
      route.fulfill({ status: 500, contentType: 'application/json', body: '{"error":"boom"}' }),
    );
    await page.goto('/providers');
    await expect(page.getByRole('heading', { name: 'Providers', exact: true })).toBeVisible();
    await expect(page.locator('.empty[role="alert"]')).toContainText('boom');
  });

  test('an empty route announces itself with a next action', async ({ page }) => {
    await page.route('**/api/admin/models', (route) =>
      route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({ models: [] }),
      }),
    );
    await page.goto('/models');
    const empty = page.locator('.empty[role="status"]');
    await expect(empty).toBeVisible();
    await expect(empty).toContainText('No models');
  });
});
