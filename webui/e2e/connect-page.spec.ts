import { expect, test } from '@playwright/test';

// /connect (ADR-026): the generated configurations must use the console's
// own origin, the selected model and the right discovery mechanism per CLI.

async function login(page: import('@playwright/test').Page) {
  await page.goto('/login');
  await page.getByLabel('Username').fill('admin');
  await page.getByLabel('Password').fill('proxyllm-e2e');
  await page.getByRole('button', { name: /sign in/i }).click();
  await expect(page.getByRole('link', { name: 'Connect a CLI' })).toBeVisible();
}

test('connect page renders per-client configuration from the console origin', async ({ page }) => {
  await login(page);
  await page.goto('/connect');
  await expect(page.getByRole('heading', { name: 'Connect a CLI', exact: true })).toBeVisible();

  // Claude Code is the default tab and carries the discovery flag.
  const snippet = page.locator('.connect-snippet pre').first();
  await expect(snippet).toContainText('ANTHROPIC_BASE_URL');
  await expect(snippet).toContainText('CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY');
  await expect(snippet).toContainText(await page.evaluate(() => window.location.origin));

  // Model picker drives the snippet.
  await page.locator('.connect-block select').selectOption('mdl-gpt5');
  await expect(snippet).toContainText('"ANTHROPIC_MODEL": "mdl-gpt5"');

  await page.getByRole('tab', { name: 'Codex' }).click();
  await expect(page.locator('.connect-snippet pre').first()).toContainText('model_catalog_url');
  await expect(page.locator('.connect-snippet pre').first()).toContainText('wire_api = "responses"');
  await expect(page.locator('.connect-snippet pre').first()).toContainText('api_key_model_discovery = true');

  await page.getByRole('tab', { name: 'OpenCode' }).click();
  await expect(page.locator('.connect-snippet pre').first()).toContainText('"proxyllm"');
  await expect(page.locator('.connect-snippet pre').first()).toContainText('"model": "proxyllm/mdl-gpt5"');

  await page.getByRole('tab', { name: 'Generic OpenAI' }).click();
  await expect(page.locator('.connect-snippet pre').first()).toContainText('/v1');
});

test('the sidebar links to the connect page from the access section', async ({ page }) => {
  await login(page);
  const link = page.getByRole('link', { name: 'Connect a CLI' });
  await expect(link).toBeVisible();
  // The mobile sidebar is an overlay; force the activation so the overlay's
  // settle animation cannot swallow the click.
  await link.click({ force: true });
  await expect(page.getByRole('heading', { name: 'Connect a CLI', exact: true })).toBeVisible();
});
