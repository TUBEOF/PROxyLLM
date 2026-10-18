import { expect, test } from '@playwright/test';

// API keys page: create (display-once), regenerate (new secret, old one
// dies), delete (hard delete — the row disappears and the secret stops
// authenticating). The fixture server runs the real admin API and store, so
// these are end-to-end assertions, not stubs.

async function login(page: import('@playwright/test').Page) {
  await page.goto('/login');
  await page.getByLabel('Username').fill('admin');
  await page.getByLabel('Password').fill('proxyllm-e2e');
  await page.getByRole('button', { name: /sign in/i }).click();
  await expect(page.getByRole('link', { name: 'API keys' })).toBeVisible();
  await page.goto('/keys');
  await expect(page.getByRole('heading', { name: 'API keys' })).toBeVisible();
}

async function createKey(page: import('@playwright/test').Page, name: string): Promise<string> {
  await page.getByRole('button', { name: /^create key$/i }).click();
  const dialog = page.getByRole('dialog');
  await expect(dialog).toContainText('Create API key');
  await dialog.getByLabel('Name').fill(name);
  await dialog.getByRole('button', { name: /^create$/i }).click();
  await expect(dialog).toContainText('Key created');
  const secret = await dialog.locator('input.mono').inputValue();
  expect(secret.startsWith('pk_')).toBe(true);
  await dialog.getByRole('button', { name: /^done$/i }).click();
  return secret;
}

test('create shows the secret once and lists only the current key', async ({ page }) => {
  await login(page);
  const name = `e2e-create-${Date.now()}`;
  const secret = await createKey(page, name);

  const row = page.locator('tr').filter({ hasText: name });
  await expect(row).toBeVisible();
  await expect(row).toContainText('never'); // last used

  // The one-time secret authenticates against the OpenAI surface.
  const ok = await page.request.get('/v1/models', { headers: { 'x-api-key': secret } });
  expect(ok.status()).toBe(200);
});

test('regenerate replaces the secret and the old one stops working', async ({ page }) => {
  await login(page);
  const name = `e2e-regen-${Date.now()}`;
  const oldSecret = await createKey(page, name);

  const row = page.locator('tr').filter({ hasText: name });
  await row.getByRole('button', { name: /^regenerate$/i }).click();
  const dialog = page.getByRole('dialog');
  await expect(dialog).toContainText('Regenerate API key');
  await dialog.getByRole('button', { name: /^regenerate$/i }).click();
  await expect(dialog).toContainText('Key regenerated');
  const newSecret = await dialog.locator('input.mono').inputValue();
  expect(newSecret.startsWith('pk_')).toBe(true);
  expect(newSecret).not.toBe(oldSecret);
  await dialog.getByRole('button', { name: /^done$/i }).click();

  const oldStatus = await page.request.get('/v1/models', { headers: { 'x-api-key': oldSecret } });
  expect(oldStatus.status()).toBe(401);
  const newStatus = await page.request.get('/v1/models', { headers: { 'x-api-key': newSecret } });
  expect(newStatus.status()).toBe(200);
});

test('delete removes the row and the secret stops working', async ({ page }) => {
  await login(page);
  const name = `e2e-delete-${Date.now()}`;
  const secret = await createKey(page, name);

  const row = page.locator('tr').filter({ hasText: name });
  await expect(row).toBeVisible();
  await row.getByRole('button', { name: /^delete$/i }).click();
  const dialog = page.getByRole('dialog');
  await expect(dialog).toContainText('Delete API key');
  await dialog.getByRole('button', { name: /^delete key$/i }).click();

  await expect(page.locator('tr').filter({ hasText: name })).toHaveCount(0);
  const status = await page.request.get('/v1/models', { headers: { 'x-api-key': secret } });
  expect(status.status()).toBe(401);
});
