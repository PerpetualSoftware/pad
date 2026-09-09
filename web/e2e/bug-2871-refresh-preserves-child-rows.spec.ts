import { test, expect } from './fixtures';
import { browserLogin, seedDoc } from './lib/collab-helpers';
import type { APIRequestContext, Page } from '@playwright/test';
import type { SuiteFixture } from './fixtures';

/**
 * BUG-2871 — a child row's DOM node must survive a background refresh.
 *
 * `loadChildren()` set `loading = true` before every fetch, and the template
 * swaps the whole list for a spinner while loading. Any `item_created` in the
 * workspace triggers that refresh (ChildItems' SSE subscription), so with other
 * people working it fired constantly — and each time, every row node was
 * destroyed and rebuilt even when the data came back identical.
 *
 * A click needs mousedown and mouseup on the SAME node. When a refresh landed
 * between them the click event never fired at all: no navigation, no in-pane
 * drill, and no error anywhere. In e2e that presented as the ctrl-click popup
 * timing out; for a user it silently drops a click on a child row.
 *
 * This asserts the property the fix establishes — an unchanged row keeps its
 * node across a refresh — rather than the symptom, because the symptom is
 * timing-dependent and the property is not.
 */

const DESKTOP = { width: 1200, height: 900 };

function authHeaders(fixture: SuiteFixture) {
	return { Authorization: `Bearer ${fixture.apiToken}` };
}

function itemCard(page: Page, title: string) {
	return page.locator('.item-card').filter({ has: page.locator('.card-title', { hasText: title }) });
}

test('BUG-2871: a same-data refresh keeps the child row node', async ({
	page,
	fixture,
	request,
}, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'the pane is a desktop-split concern');

	await page.setViewportSize(DESKTOP);
	await browserLogin(page);

	const parent = await seedDoc(fixture, request, 'B2871 refresh parent');
	const kidTitle = `B2871 refresh kid ${Date.now()}`;
	const kidResp = await request.post(
		`/api/v1/workspaces/${fixture.workspaceSlug}/collections/docs/items`,
		{
			headers: authHeaders(fixture),
			data: { title: kidTitle, fields: JSON.stringify({ parent: parent.id }), content: '' },
		},
	);
	expect(kidResp.ok(), await kidResp.text()).toBeTruthy();

	// Count children fetches, so "the node survived" can't pass because no
	// refresh ever happened — which is the way this test would go vacuous.
	// Count RESPONSES, not requests (codex round 1). A request event can fire
	// before the component has processed the state change that replaces the
	// DOM, so polling on requests and asserting immediately could observe the
	// node still connected on a BROKEN build — a race-dependent false pass.
	// The replacement happens when the refresh settles, so that is what to wait
	// for, plus a render turn.
	let childrenResponses = 0;
	page.on('response', (res) => {
		const req = res.request();
		if (req.method() === 'GET' && /\/items\/[^/]+\/children/.test(res.url())) childrenResponses++;
	});

	await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/docs`);
	await itemCard(page, 'B2871 refresh parent').first().click();
	const pane = page.locator('.item-pane');
	await expect(pane).toBeVisible();
	await pane.getByRole('tab', { name: 'Relationships' }).click();
	const childRow = pane.locator('.child-row', { hasText: kidTitle });
	await expect(childRow).toBeVisible();

	// Hold a reference to the actual node, then trigger a refresh whose result
	// is identical by creating an UNRELATED item in the same workspace.
	await page.evaluate((title) => {
		const row = [...document.querySelectorAll('.item-pane .child-row')].find((el) =>
			(el.textContent ?? '').includes(title),
		);
		(window as unknown as { __b2871row?: Element | undefined }).__b2871row = row;
	}, kidTitle);

	const responsesBefore = childrenResponses;
	const unrelated = await request.post(
		`/api/v1/workspaces/${fixture.workspaceSlug}/collections/docs/items`,
		{ headers: authHeaders(fixture), data: { title: `B2871 unrelated ${Date.now()}`, content: '' } },
	);
	expect(unrelated.ok(), await unrelated.text()).toBeTruthy();

	// Non-vacuity: a refresh must actually have COMPLETED — otherwise "the node
	// survived" is a statement about a refresh that never happened.
	await expect
		.poll(() => childrenResponses, { timeout: 10_000 })
		.toBeGreaterThan(responsesBefore);
	// ...and give the component its render turn, so a broken build has actually
	// had the chance to replace the node before this asserts that it did not.
	await page.evaluate(() => new Promise((r) => requestAnimationFrame(() => requestAnimationFrame(() => r(null)))));
	await page.waitForTimeout(500);

	const result = await page.evaluate((title) => {
		const w = window as unknown as { __b2871row?: Element };
		const rendered = [...document.querySelectorAll('.item-pane .child-row')].some((el) =>
			(el.textContent ?? '').includes(title),
		);
		return { sameNodeStillConnected: !!w.__b2871row?.isConnected, rendered };
	}, kidTitle);

	// The row is still on screen either way — the point is that it is the SAME
	// node, because a replaced node is what swallows an in-flight click.
	expect(result.rendered, 'the child row disappeared entirely').toBe(true);
	expect(result.sameNodeStillConnected, 'the child row node was replaced by a refresh').toBe(true);
});
