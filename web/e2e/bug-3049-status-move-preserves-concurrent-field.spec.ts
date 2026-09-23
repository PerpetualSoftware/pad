import { test, expect } from './fixtures';
import { browserLogin } from './lib/collab-helpers';
import type { Page } from '@playwright/test';
import type { SuiteFixture } from './fixtures';

/**
 * BUG-3049 — a board/list status move must not revert a field written after the
 * page loaded.
 *
 * `handleStatusChange` in the collection page read the item's fields from the
 * page's own (load-time) copy, set the group field on it, and PATCHed the WHOLE
 * blob. So a status click reverted every field written since the page loaded —
 * by another user, another tab, an agent, or the item pane. It now sends a
 * `fields_patch` naming only the group field.
 *
 * This is the browser half of the unit's evidence, and the only leg that runs
 * the real door in a real browser against the real server: the interleaving is
 * genuine (an API write lands between the page load and the click), and the
 * assertion is the OUTCOME on the row, read back from the server — not the shape
 * of the request. The request shape is checked too, because it is the mechanism
 * and a future regression is likelier to change it than to change the outcome
 * in some other way.
 */

const DESKTOP = { width: 1200, height: 900 };

function authHeaders(fixture: SuiteFixture) {
	return { Authorization: `Bearer ${fixture.apiToken}`, 'Content-Type': 'application/json' };
}

function itemCard(page: Page, title: string) {
	return page
		.locator('.item-card')
		.filter({ has: page.locator('.card-title', { hasText: title }) });
}

test('BUG-3049: a status click does not revert a field written after page load', async ({
	page,
	fixture,
	request,
}, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'one browser is enough for a write shape');

	await page.setViewportSize(DESKTOP);
	await browserLogin(page);

	// HOLD THE WINDOW OPEN, and why this is the honest setup rather than a
	// contrivance. With SSE connected, the concurrent write below reaches this
	// tab within milliseconds and refreshes its copy of the item, so a
	// full-blob write would send the FRESH value and the revert would not
	// happen. MEASURED, not assumed: with the door reverted to a full blob and
	// SSE connected, this test's readback still passed — the defect was
	// invisible. That is a statement about the RACE being narrow on a warm local
	// instance, not about the write being safe: a tab whose stream is
	// disconnected (offline, a proxy that buffers, a backgrounded tab, a dropped
	// reconnect) holds a stale copy indefinitely, and a click inside the stream's
	// latency does the same on a healthy one. Blocking the stream is how the
	// window is held open deterministically.
	await page.route('**/api/v1/events*', (route) => route.abort());

	const title = `B3049 status move ${Date.now()}`;
	const created = await request.post(
		`/api/v1/workspaces/${fixture.workspaceSlug}/collections/tasks/items`,
		{
			headers: authHeaders(fixture),
			data: { title, fields: JSON.stringify({ status: 'open', priority: 'high' }), content: '' },
		},
	);
	expect(created.ok(), await created.text()).toBeTruthy();
	const item = await created.json();

	await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/tasks`);
	const card = itemCard(page, title);
	await expect(card).toBeVisible();
	// BUG-3157: the chip opens a status picker; the move is choosing a row.
	const statusChip = card.locator('[aria-haspopup="menu"][title="Change status"]');
	await expect(statusChip).toBeVisible();
	const statusBefore = (await statusChip.innerText()).trim();

	// THE CONCURRENT WRITE. It lands after the page has its copy of the item,
	// which is the window the defect lived in. Sent as a field patch, i.e. the
	// shape every other writer now uses — so if the door under test still sent a
	// blob, this value is what it would revert.
	const concurrent = await request.patch(
		`/api/v1/workspaces/${fixture.workspaceSlug}/items/${item.id}`,
		{ headers: authHeaders(fixture), data: { fields_patch: { priority: 'critical' } } },
	);
	expect(concurrent.ok(), await concurrent.text()).toBeTruthy();

	// PREMISE, asserted before the click: the concurrent write really landed.
	// Without this leg the survival assertion below passes when nothing happened.
	const midRead = await (
		await request.get(`/api/v1/workspaces/${fixture.workspaceSlug}/items/${item.id}`, {
			headers: authHeaders(fixture),
		})
	).json();
	expect(JSON.parse(midRead.fields).priority, 'the concurrent write did not land').toBe('critical');

	// waitForResponse, not waitForRequest: the write must have COMMITTED before
	// the readback below, or that read can race an in-flight PATCH.
	const patched = page.waitForResponse(
		(r) => r.url().includes(`/items/${item.id}`) && r.request().method() === 'PATCH',
	);
	await statusChip.click();
	await page.getByRole('menu', { name: 'Status' }).getByRole('menuitemradio', { name: /done/i }).click();
	const patchRes = await patched;
	expect(patchRes.ok(), await patchRes.text()).toBeTruthy();

	const after = await (
		await request.get(`/api/v1/workspaces/${fixture.workspaceSlug}/items/${item.id}`, {
			headers: authHeaders(fixture),
		})
	).json();
	const fields = JSON.parse(after.fields);

	// The door did its own job — the other half of the premise. A click that
	// wrote nothing would satisfy the survival assertion.
	expect(fields.status, 'the status move did not apply').not.toBe('open');
	expect(statusBefore.toLowerCase()).toContain('open');
	// The defect itself.
	expect(fields.priority, 'BUG-3049: the status move reverted a field it did not name').toBe(
		'critical',
	);

	// The MECHANISM, asserted after the outcome so that a regression is reported
	// as the consequence first and the shape second. (Ordering matters: with the
	// shape checked first, a mutant reverting the door fails here and never
	// reaches the readback, so the outcome leg would never be shown able to go
	// red — CONVE-34 applied to the test's own legs.)
	const body = JSON.parse(patchRes.request().postData() ?? '{}');
	expect(body.fields_patch, 'the status move must send a field patch').toBeTruthy();
	expect(
		body.fields,
		'the status move must not send a full fields blob (that is the defect)',
	).toBeUndefined();
	expect(
		Object.keys(body.fields_patch),
		'the patch must name only the group field',
	).toEqual(['status']);
});
