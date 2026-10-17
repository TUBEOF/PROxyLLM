import { expect, test, type Page } from '@playwright/test';
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

/** Opens the provider-type group a model card lives in and returns that card. */
async function modelCard(page: Page, groupLabel: string, modelId: string) {
  const group = page.locator('details.fieldset').filter({ hasText: groupLabel });
  await group.locator('summary').click();
  const card = group.locator('.widget').filter({ hasText: modelId });
  await expect(card).toHaveCount(1);
  return card;
}

test('a model card lists the capabilities it has, and nothing else', async ({ page }) => {
  await login(page);
  await page.goto('/models');
  const card = await modelCard(page, 'ChatGPT / Codex', 'mdl-gpt5');

  // The seeded row declares streaming, tools, vision, web_search, thinking.
  for (const flag of ['streaming', 'tools', 'vision', 'web_search', 'thinking']) {
    await expect(card.locator('.badge').filter({ hasText: flag })).toHaveCount(1);
  }
  // A capability the model does NOT have is absent, not struck through: the
  // card is a statement of what the model can do.
  await expect(card).not.toContainText('code_execution');
  await expect(card).not.toContainText('decision');
  // Models are a property of the provider type — the account serving them is
  // routing's business and no longer rides the card.
  await expect(card).not.toContainText('account:');
});

test('the test outcome stays on the card', async ({ page }) => {
  await login(page);
  await page.goto('/models');
  // The fixture's scripted provider (prv-test) answers "OK" deterministically.
  const card = await modelCard(page, 'Custom OpenAI-compatible', 'mdl-test');

  await card.getByRole('button', { name: 'Test', exact: true }).click();
  const ok = card.getByRole('button', { name: 'OK' });
  await expect(ok).toBeVisible();
  // It stays: the point of testing several models is to see afterwards which
  // ones answered. The list keeps polling (10 s), so a state that lived only
  // in one response cycle would be gone by now.
  await page.waitForTimeout(11_000);
  await expect(ok).toBeVisible();
});

test('a failed test opens its request and the upstream error', async ({ page }) => {
  await login(page);
  await page.route('**/api/admin/models/test/**', (route) =>
    route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({
        ok: false,
        latency_ms: 123,
        error: 'upstream rejected the test request (HTTP 400): model overloaded',
        request: '{"model":"gpt-5","stream":false,"max_completion_tokens":16}',
        detail: {
          http_status: 400,
          type: 'invalid_request_error',
          code: 'model_overloaded',
          message: 'model overloaded',
        },
      }),
    }),
  );
  await page.goto('/models');
  const card = await modelCard(page, 'ChatGPT / Codex', 'mdl-gpt5');

  await card.getByRole('button', { name: 'Test', exact: true }).click();
  const failed = card.getByRole('button', { name: 'Failed' });
  await expect(failed).toBeVisible();

  // The cross is the door to the diagnosis: what was sent, and what came back.
  await failed.click();
  const panel = page.getByRole('dialog');
  await expect(panel).toContainText('Test failed');
  await expect(panel).toContainText('HTTP 400');
  await expect(panel).toContainText('model overloaded');
  await expect(panel).toContainText('invalid_request_error');
  // The request is the wire body the provider encoded, pretty-printed.
  await expect(panel).toContainText('"model": "gpt-5"');
});

test('the sync card owns the cadence of the model sync', async ({ page }) => {
  await login(page);
  await page.goto('/models');
  const sync = page.locator('.widget').filter({ hasText: 'Auto-sync' });
  await expect(sync).toHaveCount(1);
  await expect(sync.getByLabel('Sync interval')).toHaveValue('24');
  await expect(sync.getByRole('button', { name: 'Sync now' })).toBeVisible();
  // The last run reports what it did, per account.
  await expect(sync).toContainText('Last run');
});
