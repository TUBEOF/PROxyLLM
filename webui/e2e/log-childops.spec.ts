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
// carries a sub-operation chip and nests one child row beneath it.
test('log rows nest their sub-operations', async ({ page }) => {
  await page.goto('/logs');
  await readyForCapture(page);

  await expect(page.locator('.chip-childops').first()).toBeVisible();

  // By content, not by position: more than one row has children.
  const child = page.locator('tr.log-row-child', { hasText: 'Failover' }).first();
  await expect(child).toBeVisible();
  await expect(child.locator('.chip')).toHaveClass(/child-failover/);
  // The child hangs under its parent: its link chip names the parent row id.
  await expect(child.locator('.link-chip')).toHaveText(/^#\d+$/);
});

// The parent row's chip counts its sub-operations.
test('the parent chip counts its sub-operations', async ({ page }) => {
  await page.goto('/logs');
  await readyForCapture(page);
  await expect(page.locator('.chip-childops').first()).toContainText('1 op');
});

// The seeded emulated-tool detail carries a token-shaped value; the child
// row must show it masked, never raw (the write-path redaction, end to end).
test('an emulated web-tool child masks its credential-shaped detail', async ({ page }) => {
  await page.goto('/logs');
  await readyForCapture(page);

  const emulated = page.locator('tr.log-row-child', { hasText: 'Emulated web_search' }).first();
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
