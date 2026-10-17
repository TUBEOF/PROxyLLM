import { expect, test } from '@playwright/test';
import { login } from './helpers';

// The Models page is the CHAT-model surface. A web-tool backend (Firecrawl,
// Ollama Cloud Web Tools) is an emulator for web_search/web_fetch, owns no
// models, and must not appear here at all — the seeded fixture carries a
// Firecrawl account with no models, which is exactly the shape that used to
// render an empty group and put Firecrawl next to the model providers.
//
// The server refuses a model row on a web-tool account for the same reason
// (internal/server/webui/adminapi.go, putModel), so the page and the write
// boundary agree on what a model's provider can be.

test('the models page lists chat providers only', async ({ page }) => {
  await login(page);
  await page.goto('/models');

  const groups = page.locator('details.fieldset > summary');
  // The chat groups first: an absence assertion is satisfied by a page that
  // has not rendered its list yet, so it would pass for the wrong reason.
  // Only against a rendered list does it mean what it says. 'Ollama Cloud'
  // is a substring of the web-tools label, so that check covers both.
  await expect(groups.filter({ hasText: 'ChatGPT / Codex' })).toHaveCount(1);
  await expect(groups.filter({ hasText: 'Ollama Cloud' })).toHaveCount(1);
  await expect(groups.filter({ hasText: 'Firecrawl' })).toHaveCount(0);
  await expect(groups.filter({ hasText: 'Ollama Cloud (Web Tools)' })).toHaveCount(0);
});

test('the model editor offers chat providers only', async ({ page }) => {
  await login(page);
  await page.goto('/models');
  await page.getByRole('button', { name: 'Add model' }).click();

  const provider = page.locator('#m-provider');
  await expect(provider).toBeVisible();
  await expect(page.locator('#m-provider option')).not.toHaveCount(0);
  // The web-tool account is not a legal target for a model row.
  await expect(page.locator('#m-provider option[value="prv-firecrawl"]')).toHaveCount(0);
});
