import { test, expect } from './fixtures';
import { browserLogin } from './lib/collab-helpers';
import type { APIRequestContext } from '@playwright/test';
import type { SuiteFixture } from './fixtures';

/**
 * BUG-3043 — a lane draft whose lane stops existing.
 *
 * A draft typed in a lane lives in the collection PAGE's map, keyed by that
 * lane's value. Deleting the option took the lane, and the draft card with it,
 * off the board while the text stayed in the map, and "Save all" could then
 * neither complete nor show why. Lead ruling, RE-HOME: the draft appears in the
 * Uncategorized lane, marked with the lane it came from, and saves there.
 *
 * The unit legs (laneDrafts.test.ts, BoardView.laneDrafts.svelte.test.ts) hand
 * BoardView its placement directly. This is the leg that runs the real WIRING:
 * the page derives the placement from its own draft map and the live schema,
 * a schema edit arriving over SSE re-derives it, and the create goes through
 * the page's real create path to the real server.
 *
 * Its own collection, because deleting an option from the shared `docs`
 * collection would change the lanes every other spec in the run sees.
 */

const DESKTOP = { width: 1280, height: 900 };

function headers(fixture: SuiteFixture) {
	return { Authorization: `Bearer ${fixture.apiToken}`, 'Content-Type': 'application/json' };
}

function schema(options: string[]) {
	return JSON.stringify({ fields: [{ key: 'status', label: 'Status', type: 'select', options }] });
}

async function seed(fixture: SuiteFixture, request: APIRequestContext) {
	const ws = `/api/v1/workspaces/${fixture.workspaceSlug}`;
	const coll = await request.post(`${ws}/collections`, {
		headers: headers(fixture),
		data: { name: `B3043 rehome ${Date.now()}`, prefix: 'BRH', schema: schema(['alpha', 'beta']) },
	});
	expect(coll.ok(), await coll.text()).toBeTruthy();
	const { slug } = (await coll.json()) as { slug: string };
	// One item in a lane that SURVIVES, so the board renders (an empty
	// collection shows an empty state) and Uncategorized starts with no item.
	const anchor = await request.post(`${ws}/collections/${slug}/items`, {
		headers: headers(fixture),
		data: { title: `B3043 anchor ${Date.now()}`, fields: JSON.stringify({ status: 'alpha' }), content: '' },
	});
	expect(anchor.ok(), await anchor.text()).toBeTruthy();
	return { ws, slug };
}

test('BUG-3043: a draft whose lane is deleted moves to Uncategorized, marked, and saves there', async ({
	page,
	fixture,
	request,
}, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'viewport is driven explicitly; one project is enough');
	await page.setViewportSize(DESKTOP);
	await browserLogin(page);
	const { ws, slug } = await seed(fixture, request);

	await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/${slug}?view=board`);
	await expect(page.locator('.column-name', { hasText: 'Beta' })).toBeVisible();

	const title = `Kept draft ${Date.now()}`;
	await page.getByRole('button', { name: 'Add item to Beta' }).click();
	await page.locator('.lane-draft-input').fill(title);
	// PREMISE: no Uncategorized lane yet — so the one that appears below can only
	// be the re-home's, not an item's.
	await expect(page.locator('.uncategorized-column')).toHaveCount(0);

	// Delete the option the draft's lane stands for.
	const patched = await request.patch(`${ws}/collections/${slug}`, {
		headers: headers(fixture),
		data: { schema: schema(['alpha']) },
	});
	expect(patched.ok(), await patched.text()).toBeTruthy();

	await expect(page.locator('.column-name', { hasText: 'Beta' })).toHaveCount(0);
	const card = page.locator('.uncategorized-column .lane-draft-rehomed');
	await expect(card).toBeVisible();
	await expect(card.locator('.lane-draft-moved')).toContainText('Moved from Beta');
	await expect(card.locator('textarea')).toHaveValue(title);

	await card.getByRole('button', { name: 'Add card' }).click();
	await expect(page.locator('.uncategorized-column .item-card', { hasText: title })).toBeVisible();

	// The real server stored it in Uncategorized: status '' (a select stores
	// the blank even though the client pre-fills the first option).
	const list = await request.get(`${ws}/collections/${slug}/items`, { headers: headers(fixture) });
	expect(list.ok()).toBeTruthy();
	const rows = (await list.json()) as Array<{ title: string; fields: string | Record<string, unknown> }>;
	const saved = rows.find((r) => r.title === title);
	expect(saved, 'the draft was created').toBeTruthy();
	const fields = typeof saved!.fields === 'string' ? JSON.parse(saved!.fields) : saved!.fields;
	expect(fields.status).toBe('');
});
