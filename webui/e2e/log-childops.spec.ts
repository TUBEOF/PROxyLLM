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

// The row OPENS (2026-10-10). A sub-row used to be inert, so the tool exchange
// that answered a call inside the request was unreachable from the log — the
// operator could see that a web tool ran but not what it said.
test('a sub-row opens its own view with what the step sent and received', async ({ page }) => {
  await page.goto('/logs');
  await readyForCapture(page);

  await page.locator('tr.log-row-child', { hasText: 'Web tool: web_search' }).first().click();

  const drawer = page.locator('.drawer');
  await expect(drawer).toBeVisible();
  await expect(drawer.locator('legend', { hasText: 'Step' })).toBeVisible();
  // The step's own body, labelled as what it sent and received (never as the
  // parent's conversation).
  await expect(drawer.locator('legend', { hasText: 'Sent & received' })).toBeVisible();
  await expect(drawer.locator('pre', { hasText: 'sqlite wal busy_timeout' }).first()).toBeVisible();
  await expect(drawer.locator('pre', { hasText: 'sqlite.org/wal.html' }).first()).toBeVisible();
  // The parent request is one button away.
  await expect(drawer.locator('button', { hasText: 'Open request #' })).toBeVisible();
});

// The operator's report, pinned end to end: a vision-fallback sub-row must say
// WHAT was described, not how many characters it took.
test('a vision-fallback sub-row shows the description, not a character count', async ({ page }) => {
  await page.goto('/logs');
  await readyForCapture(page);

  const caption = page.locator('tr.log-row-child', { hasText: 'Image caption' }).first();
  await expect(caption).toBeVisible();
  await expect(caption).not.toContainText('character(s) described');
  await expect(caption).toContainText('failing Go test');

  await caption.click();
  const drawer = page.locator('.drawer');
  await expect(drawer.locator('pre', { hasText: 'terminal window' }).first()).toBeVisible();
  // The described image by media type and size — never its bytes.
  await expect(drawer.locator('pre', { hasText: 'image/png' }).first()).toBeVisible();
  await expect(drawer.locator('pre', { hasText: '48213' }).first()).toBeVisible();
});

// A step and its request are two views of the same request, linked both ways:
// the row opens the step, the step's button opens the request.
test('a sub-op view links back to its request', async ({ page }) => {
  await page.goto('/logs');
  await readyForCapture(page);

  await page.locator('tr.log-row-child', { hasText: 'Image caption' }).first().click();
  const drawer = page.locator('.drawer');
  await drawer.locator('button', { hasText: 'Open request #' }).click();

  // The request view: its own fields and the structured trace.
  await expect(drawer.locator('legend', { hasText: 'Request' })).toBeVisible();
  await expect(drawer).toContainText('Route trace');
  // And the step is one click away again, from the drawer's own list.
  await expect(drawer.locator('.child-op-open').first()).toBeVisible();
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
