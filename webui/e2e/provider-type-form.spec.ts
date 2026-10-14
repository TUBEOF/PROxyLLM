import { expect, test } from '@playwright/test';

// The provider-type settings form is filled from two independent requests: the
// catalog names the fields, the detail carries their values. Filling it as soon
// as the catalog arrives leaves every field empty for a type that has saved
// settings — and saving that form writes the emptiness back over them, because
// the submit path only sends the fields it finds non-empty.
//
// The race is made deterministic here rather than left to luck: the detail
// response is delayed past the catalog's, which is the order that emptied the
// form. Without the delay this passes either way on a fast local server.

async function login(page: import('@playwright/test').Page) {
  await page.goto('/login');
  await page.getByLabel('Username').fill('admin');
  await page.getByLabel('Password').fill('proxyllm-e2e');
  await page.getByRole('button', { name: /sign in/i }).click();
  await expect(page.getByRole('link', { name: 'Logs' })).toBeVisible();
}

test('the provider-type settings form shows the saved settings', async ({ page }) => {
  await login(page);
  await page.route('**/api/admin/provider-types/*', async (route) => {
    // Only the detail — the value-carrying request — is held back. The catalog
    // list must answer first, or there is no race to lose.
    if (route.request().method() === 'GET' && !route.request().url().endsWith('/provider-types')) {
      await new Promise((resolve) => setTimeout(resolve, 500));
    }
    await route.continue();
  });
  await page.goto('/providers/codex-oauth');

  const issuer = page.locator('#set-issuer');
  await expect(issuer).toBeVisible();
  await expect(issuer).toHaveValue('https://auth.openai.com');
});
