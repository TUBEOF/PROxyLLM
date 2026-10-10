// Logs page sub-operations (2026-10-07): assertion-only specs — no
// baselines involved, so this suite is green locally as well as in CI.
// Fixturesrv derives the child rows from the seeded traces with the server's
// own mapping, so these assertions exercise the real content.

import { test, expect } from '@playwright/test';
import { freezeTime, login, readyForCapture } from './helpers';

test.beforeEach(async ({ page }) => {
  await freezeTime(page);
  await login(page);
});

// The seeded failover row (llama3.3-70b) recorded a failover event, so it
// nests one child row beneath it, drawn as a tree so the branch reads as
// belonging to the parent above it. The parent is marked by the tree stub in
// its gutter, not by a badge: the ops count repeated nothing the nesting does
// not already say.
test('log rows nest their sub-operations', async ({ page }) => {
  await page.goto('/logs');
  await readyForCapture(page);

  // By content, not by position: more than one row has children. The parent
  // drops a tree stub into the gutter; the child continues the line.
  await expect(page.locator('tr.log-row[data-has-children]').first()).toBeVisible();
  const child = page.locator('tr.log-row-child', { hasText: 'Fallback' }).first();
  await expect(child).toBeVisible();
  await expect(child.locator('.chip')).toHaveClass(/child-failover/);
  await expect(child.locator('.child-elbow').first()).toBeAttached();
  // The last child terminates the branch rather than trailing a dangling line.
  await expect(page.locator('tr.log-row-child[data-child-last]').first()).toBeAttached();
});

// A sub-row owns only its own facts. The parent's endpoint, credential and
// token columns have no meaning on a fallback op, so they render empty rather
// than echoing the parent's values or padding with a placeholder dash.
test('a sub-row leaves the request-scoped columns empty', async ({ page }) => {
  await page.goto('/logs');
  await readyForCapture(page);

  const child = page.locator('tr.log-row-child', { hasText: 'Fallback' }).first();
  const cells = child.locator('td');
  // Columns: gutter | status | model | provider | protocol | account | key |
  // tokens | tps | duration | time. Indices 3..9 are the request-scoped ones.
  for (const i of [3, 4, 5, 6, 7, 8, 9]) {
    await expect(cells.nth(i)).toHaveText('');
  }
  // The op's own columns still carry content.
  await expect(cells.nth(1)).not.toHaveText('');
  await expect(cells.nth(10)).not.toHaveText('');
});

// The seeded emulated-tool detail carries a token-shaped value; the child
// row must show it masked, never raw (the write-path redaction, end to end).
test('an emulated web-tool child masks its credential-shaped detail', async ({ page }) => {
  await page.goto('/logs');
  await readyForCapture(page);

  const emulated = page.locator('tr.log-row-child', { hasText: 'Web tool: web_search' }).first();
  await expect(emulated).toBeVisible();
  await expect(emulated.locator('.chip')).toHaveClass(/child-emulated-tool/);
  await expect(emulated).toContainText('[REDACTED');
  await expect(emulated).not.toContainText('sk-live-e2efixture1234');
});

// The detail drawer replaces the raw JSON dump with a chain summary and a
// routing timeline, and keeps the raw trace available as a disclosure.
test('detail drawer renders the structured trace with a raw fallback', async ({ page }) => {
  await page.goto('/logs');
  await readyForCapture(page);

  // Open the seeded row that carries a capability_fallback event.
  await page.locator('tr.log-row', { hasText: 'glm-5.3-flash' }).first().click();

  const drawer = page.locator('.drawer');
  await expect(drawer).toBeVisible();
  await expect(drawer).toContainText('Route trace');
  // Structured: a chain summary pill row + a timeline feed item.
  await expect(drawer.locator('.chain-summary .chain-pill').first()).toBeVisible();
  await expect(drawer.locator('.feed-item').first()).toBeVisible();
  await expect(drawer.locator('.feed-item', { hasText: 'Capability fallback' }).first()).toBeVisible();
  // Raw JSON stays available for debugging / unknown shapes.
  await expect(drawer.locator('.raw-trace > summary')).toContainText('Raw JSON');
  await drawer.locator('.raw-trace > summary').click();
  await expect(drawer.locator('.raw-trace pre')).toContainText('"version"');
});
