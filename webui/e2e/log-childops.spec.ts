// Logs page sub-operations: assertion-only specs — no
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

// The elbow into a row is positioned as a percentage of the row, so the rail
// element MUST keep the row's full height. It used to be shortened to 50% on
// the last child, which dragged that child's elbow up to a quarter of the row —
// the connector read as misaligned because it was.
test('the tree rail spans its whole row, so the elbow lands on the row centre', async ({ page }) => {
  await page.goto('/logs');
  await readyForCapture(page);

  for (const row of await page.locator('tr.log-row-child').all()) {
    const rowBox = await row.boundingBox();
    const elbowBox = await row.locator('.child-elbow').first().boundingBox();
    expect(rowBox).not.toBeNull();
    expect(elbowBox).not.toBeNull();
    // Within a pixel: the rail covers the row, top to bottom, on every child,
    // including the last one that terminates the branch.
    expect(Math.abs((elbowBox?.height ?? 0) - (rowBox?.height ?? 0))).toBeLessThanOrEqual(1);
  }
});

// A sub-row owns only its own facts. The parent's protocol, credential and
// token columns have no meaning on a fallback op, so they render empty rather
// than echoing the parent's values or padding with a placeholder dash. Provider
// and account DO carry the endpoint this op ran on — that is the whole point of
// a fallback row, and the parent's values would name where the request started
// instead of where the failover landed.
// A sub-row shows what the step owns — where it ran, what it spent — and
// inherits what the request owns: the surface it arrived on and the key that
// opened it. Nothing is left blank: a value an op has none of reads as a dash,
// so an empty cell can never be confused with a column that failed to load.
//
// This contract replaced an earlier one that kept protocol/key/tokens/TPS/
// duration empty on sub-rows. The operator asked for the opposite: a describer
// call and a web-tool call both take time, the describer also spends tokens, and
// the request's own facts belong to every step of it.
test('a sub-row shows its own endpoint, the request it belongs to, and a dash where it owns nothing', async ({ page }) => {
  await page.goto('/logs');
  await readyForCapture(page);

  const child = page.locator('tr.log-row-child', { hasText: 'Fallback' }).first();
  const cells = child.locator('td');
  // Columns: gutter | status | model | provider | protocol | account | key |
  // tokens | tps | duration | time.
  // A failover is a transition, not an operation: it took no time and spent
  // nothing, and says so.
  await expect(cells.nth(7)).toHaveText(/—/);
  await expect(cells.nth(8)).toHaveText('—');
  await expect(cells.nth(9)).toHaveText('—');
  // Inherited from the request, so both carry content.
  await expect(cells.nth(4)).not.toHaveText('');
  await expect(cells.nth(6)).not.toHaveText('');
  // The op's own columns: status, model, provider, account, time.
  for (const i of [1, 2, 3, 5, 10]) {
    await expect(cells.nth(i)).not.toHaveText('');
  }
});

// Every row is one line tall. The old two-line model cell (the resolved model
// with the requested id, the op's reason or caption excerpt beneath it) made a
// sub-row taller than its parent and left the tree elbow floating between two
// baselines. The detail is not lost: it is the first thing the row's drawer
// shows.
test('a model cell is a single line on both the request and the sub-row', async ({ page }) => {
  await page.goto('/logs');
  await readyForCapture(page);

  // A Range yields one client rect per LINE BOX, so this counts rendered lines
  // directly. The targets name short models, so a second box would be a
  // sub-line rather than a wrapped model id.
  const captionCell = page
    .locator('tr.log-row-child', { hasText: 'Image caption' })
    .first()
    .locator('.child-model');
  await expect(captionCell).toHaveText('glm-5.3-vl');
  const lines = await captionCell.evaluate((el) => {
    const range = document.createRange();
    range.selectNodeContents(el);
    return range.getClientRects().length;
  });
  expect(lines, 'the sub-row model cell must render one line').toBe(1);

  // And the second line is gone as a STRUCTURE, not just on one row: the class
  // that drew it no longer exists in the table.
  await expect(page.locator('td.cell-sub')).toHaveCount(0);
});

// The seeded emulated-tool detail carries a token-shaped value. The row no
// longer prints the op's detail (that is what made it two lines tall), so the
// property is now pinned where the detail IS rendered: the request drawer's
// sub-operation list. It must show the value masked and never raw — the
// write-path redaction, end to end.
test('an emulated web-tool child masks its credential-shaped detail', async ({ page }) => {
  await page.goto('/logs');
  await readyForCapture(page);

  const emulated = page.locator('tr.log-row-child', { hasText: 'Web tool: web_search' }).first();
  await expect(emulated).toBeVisible();
  await expect(emulated.locator('.chip')).toHaveClass(/child-emulated-tool/);
  // Nowhere in the list may the raw secret appear.
  await expect(page.locator('tr.log-row-child').filter({ hasText: 'Web tool: web_search' })).toHaveCount(
    1,
  );
  await expect(page.locator('table')).not.toContainText('sk-live-e2efixture1234');

  // The detail lives in the request drawer's sub-operation list, masked.
  // Scoped to that list on purpose: the drawer also renders the raw route_trace
  // (its own documented surface, not redacted on write — ADR-015), and this
  // test is about the child operation's own rendering, not the trace view.
  await emulated.click();
  await page.locator('.drawer').locator('button', { hasText: 'Open request #' }).click();
  const list = page.locator('.drawer .child-op-detail');
  await expect(list.first()).toBeVisible();
  await expect(list).toContainText('[REDACTED');
  await expect(list).not.toContainText('sk-live-e2efixture1234');
});

// The row OPENS. A sub-row used to be inert, so the tool exchange
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

// The operator's report, pinned end to end: for a vision-fallback sub-row the
// description is what matters, never the character count. The row itself is now
// one line and names only the model that described the image, so the
// description is asserted where it is rendered — the step's own view.
test('a vision-fallback sub-row shows the description, not a character count', async ({ page }) => {
  await page.goto('/logs');
  await readyForCapture(page);

  const caption = page.locator('tr.log-row-child', { hasText: 'Image caption' }).first();
  await expect(caption).toBeVisible();
  await expect(caption.locator('.child-model')).toHaveText('glm-5.3-vl');
  // The count is not evidence of anything; it must not stand in for the text.
  await expect(caption).not.toContainText('character(s) described');

  // A describer call is an operation: it took time and it spent tokens, so the
  // row reports both where it knows them.
  const cells = caption.locator('td');
  await expect(cells.nth(4)).not.toHaveText(''); // protocol, inherited
  await expect(cells.nth(6)).not.toHaveText(''); // API key, inherited
  await expect(cells.nth(7)).toContainText('TI:');
  await expect(cells.nth(9)).toContainText('ms');

  await caption.click();
  const drawer = page.locator('.drawer');
  await expect(drawer.locator('pre', { hasText: 'failing Go test' }).first()).toBeVisible();
  await expect(drawer.locator('pre', { hasText: 'terminal window' }).first()).toBeVisible();
  // The described image by media type and size — never its bytes.
  await expect(drawer.locator('pre', { hasText: 'image/png' }).first()).toBeVisible();
  await expect(drawer.locator('pre', { hasText: '48213' }).first()).toBeVisible();
});

// A web-tool sub-row answers "which web provider served this". It names the
// emulating account, not the model whose round asked for the call, and it owns a
// duration but no tokens — the model's own round is accounted on the request.
test('a web-tool sub-row names the web provider, with no model and no tokens', async ({ page }) => {
  await page.goto('/logs');
  await readyForCapture(page);

  const row = page.locator('tr.log-row-child', { hasText: 'Web tool: web_search' }).first();
  await expect(row).toBeVisible();
  const cells = row.locator('td');
  await expect(cells.nth(2)).toHaveText('—'); // no model: the chat model is not where the call went
  await expect(cells.nth(3)).not.toHaveText(''); // provider: the emulating account
  await expect(cells.nth(5)).not.toHaveText(''); // account name
  await expect(cells.nth(7)).toHaveText(/—/); // nothing was billed here
  await expect(cells.nth(8)).toHaveText('—');
  await expect(cells.nth(9)).toContainText('ms');
});

// A description served from the cache is the absence of an operation, so it
// produces no row of its own. The substitution is still on the record — in the
// request's own trace — and the request itself has no sub-row at all.
test('a cached description adds no sub-row', async ({ page }) => {
  await page.goto('/logs');
  await readyForCapture(page);

  const row = page.locator('tr.log-row[title*="ca9e0f21"]').first();
  await expect(row).toBeVisible();
  await expect(row).not.toHaveAttribute('data-has-children', /.+/);

  await row.click();
  const drawer = page.locator('.drawer');
  await expect(drawer.locator('.child-op-list')).toHaveCount(0);
  await expect(drawer).toContainText('reused from the caption cache');
});

// Nothing in the log is blank: a value a row does not have reads as a dash, so
// an empty cell can only ever mean "not loaded".
test('no cell in the log table is empty', async ({ page }) => {
  await page.goto('/logs');
  await readyForCapture(page);

  // The gutter column is the tree rail: a graphic, not a value, and correctly
  // empty on a row without children.
  const empties = await page
    .locator('tr.log-row td:not(.td-gutter), tr.log-row-child td:not(.td-gutter)')
    .evaluateAll((cells) =>
      cells.map((c) => (c.textContent ?? '').trim()).filter((t) => t === ''),
    );
  expect(empties).toEqual([]);
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
