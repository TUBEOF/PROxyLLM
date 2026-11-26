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

// The account controls (Accounts table): the pool switch and the ollama.com
// session cookie. Both are stored state, so the assertions read them back
// after a reload — a control that only changed its own checkbox would pass a
// weaker test and fail the operator.

test('an account can be switched out of the pool and back', async ({ page }) => {
  await login(page);
  await page.goto('/providers/ollama-cloud');

  // The kit toggle hides its input and shows a .tswitch, so the STATE is read
  // from the input and the CLICK goes to the label that wraps it — what an
  // operator actually hits.
  const sw = page.getByLabel('Ollama key 1 in the pool');
  const control = page.locator('label.toggle').filter({ has: sw });
  await expect(control).toBeVisible();
  await expect(sw).toBeChecked();

  await control.click();
  await expect(sw).not.toBeChecked();
  await page.reload();
  await expect(page.getByLabel('Ollama key 1 in the pool')).not.toBeChecked();

  // Back on, so the fixture is left as it was found.
  await page.locator('label.toggle').filter({ has: page.getByLabel('Ollama key 1 in the pool') }).click();
  await expect(page.getByLabel('Ollama key 1 in the pool')).toBeChecked();
});

test('the session cookie is set and cleared on the account', async ({ page }) => {
  await login(page);
  await page.goto('/providers/ollama-cloud');
  await expect(page.getByRole('columnheader', { name: 'Session cookie' })).toBeVisible();

  const row = () => page.locator('tr', { hasText: 'prv-ollama-2' }).first();
  await expect(row().getByLabel('No session cookie')).toBeVisible();

  await row().getByRole('button', { name: 'Edit' }).click();
  await page.getByLabel('Set or replace it').fill('fixture-session-cookie');
  await page.getByRole('button', { name: 'Save', exact: true }).click();
  await expect(row().getByLabel('Session cookie set')).toBeVisible();

  // Clearing it removes the stored value: the presence flag is the truth.
  await row().getByRole('button', { name: 'Edit' }).click();
  // Same kit control: the checkbox is hidden, its label is the target.
  await page.locator('label.check').filter({ has: page.getByLabel('Remove the stored cookie') }).click();
  await page.getByRole('button', { name: 'Save', exact: true }).click();
  await expect(row().getByLabel('No session cookie')).toBeVisible();

  // The add-account form offers the same field for this type.
  await page.getByRole('button', { name: 'Add account' }).click();
  await expect(page.getByLabel('Session cookie (optional)')).toBeVisible();
  await expect(page.getByRole('link', { name: 'How to get it' })).toHaveAttribute('href', /docs\/operating\.md/);
});

// The Quota page is where an operator watches an account; switching it out of
// the pool belongs there too, and the card must say what the switch did.
test('the quota card carries the same pool switch', async ({ page }) => {
  await login(page);
  await page.goto('/quota');

  const card = page.locator('.usage-card').filter({ hasText: 'Ollama key 1' }).first();
  await card.scrollIntoViewIfNeeded();
  const sw = card.getByLabel('Ollama key 1 in the pool');
  await expect(sw).toBeChecked();

  // WebKit never reports the label "stable": the quota page's clock re-renders
  // the card while Playwright samples its box. Force the click on the visible
  // switch span — the same activation an operator performs — and assert the
  // state change below.
  await card.locator('label.toggle .tswitch').click({ force: true });
  await expect(sw).not.toBeChecked();
  // A switched-off account serves nothing, so the card says so rather than
  // showing a health state it cannot be in.
  await expect(card).toContainText('out of the pool');

  await card.locator('label.toggle .tswitch').click({ force: true });
  await expect(sw).toBeChecked();
});

// The device sign-in copy button. navigator.clipboard exists only in a secure
// context, so on the operator's plain-HTTP LAN origin the old code swallowed
// the TypeError and copied nothing — silently, with no way to tell. The
// fallback path is made visible here by taking the clipboard API away, which
// is exactly what an insecure origin does.
test('the device sign-in code copies without the clipboard API', async ({ page }) => {
  await login(page);
  await page.addInitScript(() => {
    Object.defineProperty(navigator, 'clipboard', { value: undefined, configurable: true });
  });
  // The issuer stays out of it: the code and the polling are canned.
  await page.route('**/api/admin/providers/*/oauth/start', (route) =>
    route.fulfill({
      json: { user_code: 'ABCD-EFGH', verification_url: 'https://example.test/device', interval_seconds: 30 },
    }),
  );
  await page.route('**/api/admin/providers/*/oauth/poll', (route) => route.fulfill({ json: { status: 'pending' } }));

  await page.goto('/providers/codex-oauth');
  await page.getByRole('button', { name: 'Add account' }).click();
  await page.getByLabel('Account name').fill('copy-fallback-probe');
  await page.getByRole('button', { name: 'Create & sign in' }).click();
  await page.getByRole('button', { name: 'Start device sign-in' }).click();

  await expect(page.getByRole('textbox', { name: 'User code' })).toHaveValue('ABCD-EFGH');
  // The copy button's accessible name is its aria-label; "Copied" is the
  // visible text the operator reads.
  const copyBtn = page.getByRole('button', { name: 'Copy user code to clipboard' });
  await copyBtn.click();
  await expect(copyBtn).toContainText('Copied');

  // Leave the fixture as it was found: the probe account goes again. The
  // drawer is transient UI — a reload drops it and shows the table behind it.
  await page.reload();
  await page.locator('tr', { hasText: 'copy-fallback-probe' }).getByRole('button', { name: 'Delete' }).click();
  await page.getByRole('dialog').getByRole('button', { name: 'Delete' }).click();
  await expect(page.locator('tr', { hasText: 'copy-fallback-probe' })).toHaveCount(0);
});

// The secure-context path stays the primary one: with the clipboard API
// present the code lands in the system clipboard, not in the fallback.
test('the device sign-in code copies through the clipboard API', async ({ page }) => {
  await login(page);
  // Only clipboard-read is granted: CI's Chromium rejects 'clipboard-write'
  // as an unknown permission, and writeText succeeds without it on a focused
  // document anyway (the helper's fallback covers a denial).
  await page.context().grantPermissions(['clipboard-read']);
  await page.route('**/api/admin/providers/*/oauth/start', (route) =>
    route.fulfill({
      json: { user_code: 'WXYZ-1234', verification_url: 'https://example.test/device', interval_seconds: 30 },
    }),
  );
  await page.route('**/api/admin/providers/*/oauth/poll', (route) => route.fulfill({ json: { status: 'pending' } }));

  await page.goto('/providers/codex-oauth');
  await page.getByRole('button', { name: 'Add account' }).click();
  await page.getByLabel('Account name').fill('copy-api-probe');
  await page.getByRole('button', { name: 'Create & sign in' }).click();
  await page.getByRole('button', { name: 'Start device sign-in' }).click();

  await expect(page.getByRole('textbox', { name: 'User code' })).toHaveValue('WXYZ-1234');
  // The copy button's accessible name is its aria-label; "Copied" is the
  // visible text the operator reads.
  const copyBtn = page.getByRole('button', { name: 'Copy user code to clipboard' });
  await copyBtn.click();
  await expect(copyBtn).toContainText('Copied');
  expect(await page.evaluate(() => navigator.clipboard.readText())).toBe('WXYZ-1234');

  await page.reload();
  await page.locator('tr', { hasText: 'copy-api-probe' }).getByRole('button', { name: 'Delete' }).click();
  await page.getByRole('dialog').getByRole('button', { name: 'Delete' }).click();
  await expect(page.locator('tr', { hasText: 'copy-api-probe' })).toHaveCount(0);
});

// ADR-022: the versioned risk notice gates the first account creation.
test('the risk gate blocks Add account until the notice is acknowledged', async ({ page }) => {
  await login(page);
  await page.goto('/providers/anthropic-oauth');
  await expect(page.getByRole('note')).toContainText('Teams or Enterprise');

  await page.getByRole('button', { name: 'Add account' }).click();
  const dialog = page.getByRole('dialog');
  await expect(dialog).toContainText('Before continuing');
  await expect(dialog).toContainText('Consumer subscriptions (Free/Pro/Max)');
  await dialog.getByRole('button', { name: 'Cancel' }).click();
  await expect(dialog).toHaveCount(0);
  await expect(page.locator('#account-form')).toHaveCount(0);

  // Continuing without "Don't show this again" does not persist anything; the
  // account form opens and can be closed again.
  await page.getByRole('button', { name: 'Add account' }).click();
  await dialog.getByRole('button', { name: 'I understand, continue' }).click();
  await expect(page.locator('#account-form')).toBeVisible();
});

// The OAuth flow is type-driven: codex gets the device UI, anthropic-oauth the
// browser + paste UI.
test('the sign-in UI follows the provider flow', async ({ page }) => {
  await login(page);

  await page.goto('/providers/codex-oauth');
  await page.getByRole('button', { name: 'Add account' }).click();
  const dialog = page.getByRole('dialog');
  await dialog.getByLabel('Account name').fill(`device-${Date.now()}`);
  await dialog.getByRole('button', { name: /create & sign in/i }).click();
  await expect(dialog).toContainText('Device sign-in');
  await dialog.locator('.drawer-foot button').filter({ hasText: 'Close' }).click();

  await page.goto('/providers/anthropic-oauth');
  await page.getByRole('button', { name: 'Add account' }).click();
  // Acknowledge once for this page load (not persisted).
  if (await page.getByRole('dialog').getByText('Before continuing').isVisible()) {
    await page.getByRole('dialog').getByRole('button', { name: 'I understand, continue' }).click();
  }
  const add = page.getByRole('dialog');
  await add.getByLabel('Account name').fill(`paste-${Date.now()}`);
  await add.getByRole('button', { name: /create & sign in/i }).click();
  await expect(add).toContainText('Anthropic has no device flow');
  await expect(add).toContainText('localhost not found');
});

// The paste completion path surfaces a state mismatch from the server and
// never sends the input anywhere else.
test('a pasted callback with a wrong state shows the server error', async ({ page }) => {
  await login(page);
  await page.route('**/api/admin/providers/*/oauth/start', (route) =>
    route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({
        flow: 'paste',
        status: 'pending',
        flow_id: 'flow-1',
        authorize_url: 'https://claude.ai/oauth/authorize?client_id=x',
        redirect_uri: 'http://localhost:53692/callback',
        expires_at: 1,
      }),
    }),
  );
  await page.route('**/api/admin/providers/*/oauth/complete', (route) =>
    route.fulfill({
      status: 400,
      contentType: 'application/json',
      body: JSON.stringify({ error: 'state mismatch — the pasted callback does not belong to this sign-in' }),
    }),
  );

  await page.goto('/providers/anthropic-oauth');
  await page.getByRole('button', { name: 'Add account' }).click();
  if (await page.getByRole('dialog').getByText('Before continuing').isVisible()) {
    await page.getByRole('dialog').getByRole('button', { name: 'I understand, continue' }).click();
  }
  const dialog = page.getByRole('dialog');
  await dialog.getByLabel('Account name').fill(`paste-error-${Date.now()}`);
  await dialog.getByRole('button', { name: /create & sign in/i }).click();
  await dialog.getByRole('button', { name: /open anthropic sign-in/i }).click();
  await expect(dialog.getByRole('link', { name: /open anthropic sign-in/i })).toHaveAttribute('href', /claude\.ai/);
  await dialog.getByLabel('Callback URL or code').fill('the-code#wrong-state');
  await dialog.getByRole('button', { name: /complete sign-in/i }).click();
  await expect(dialog.getByRole('alert')).toContainText('state mismatch');
});
