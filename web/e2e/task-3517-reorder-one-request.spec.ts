import { test, expect } from './fixtures';
import { browserLogin } from './lib/collab-helpers';
import type { APIRequestContext, Page } from '@playwright/test';
import type { SuiteFixture } from './fixtures';

/**
 * TASK-3517: a lane reorder is ONE request. The web used to send one
 * `PATCH /items/{slug} {sort_order}` per row: N rate-limit tokens, and a refusal
 * in the middle left the server holding a half-applied order. It now sends one
 * `PUT /items/sort-order`, which the server applies all or nothing.
 */

function authHeaders(fixture: SuiteFixture) {
	return { Authorization: `Bearer ${fixture.apiToken}`, 'Content-Type': 'application/json' };
}

/** Open items in a collection of the test's own, ordered a, b, c, ... (four by default). */
async function seedLane(request: APIRequestContext, fixture: SuiteFixture, count = 4) {
	const stamp = `${Date.now()}${Math.floor(Math.random() * 1000)}`;
	const schema = JSON.stringify({
		fields: [{ key: 'status', label: 'Status', type: 'select', options: ['open', 'done'], default: 'open', terminal_options: ['done'] }]
	});
	const coll = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/collections`, {
		headers: authHeaders(fixture),
		data: { name: `T3517 ${stamp}`, prefix: `TR${stamp.slice(-5)}`, schema }
	});
	expect(coll.ok(), await coll.text()).toBeTruthy();
	const collSlug = (await coll.json()).slug as string;
	const titles = ['a', 'b', 'c', 'd', 'e', 'f'].slice(0, count).map((x) => `T3517 ${x} ${stamp}`);
	for (const [i, title] of titles.entries()) {
		const res = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/collections/${collSlug}/items`, {
			headers: authHeaders(fixture),
			data: { title, fields: JSON.stringify({ status: 'open' }), content: '' }
		});
		expect(res.ok(), await res.text()).toBeTruthy();
		const slug = (await res.json()).slug as string;
		// Fresh items tie on sort_order; set the order explicitly.
		const set = await request.patch(`/api/v1/workspaces/${fixture.workspaceSlug}/items/${slug}`, {
			headers: authHeaders(fixture),
			data: { sort_order: i }
		});
		expect(set.ok(), await set.text()).toBeTruthy();
	}
	return { titles, collSlug };
}

function recordWrites(page: Page) {
	const writes: { method: string; path: string; status: number; body: string }[] = [];
	page.on('response', (r) => {
		const method = r.request().method();
		const path = new URL(r.url()).pathname;
		if (['PATCH', 'PUT'].includes(method) && path.startsWith('/api/v1/')) {
			writes.push({ method, path, status: r.status(), body: r.request().postData() ?? '' });
		}
	});
	return writes;
}

test('TASK-3517: moving a card to the top of its lane sends one sort-order request', async ({ page, fixture, request }, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'one browser is enough');
	const { titles, collSlug } = await seedLane(request, fixture);
	await browserLogin(page);
	await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/${collSlug}?view=board`);
	const order = async () =>
		(await page.locator('.board-view .item-card .card-title').allInnerTexts()).map((t) => titles.indexOf(t.trim())).filter((i) => i >= 0);
	await expect.poll(order).toEqual([0, 1, 2, 3]);

	const writes = recordWrites(page);
	const card = page.locator('.item-card').filter({ has: page.locator('.card-title', { hasText: titles[3] }) });
	await card.hover();
	await card.locator('.iam-trigger').click();
	await page.getByRole('menuitem', { name: 'Move to top' }).click();
	await expect.poll(order, 'on screen').toEqual([3, 0, 1, 2]);

	// The old loop sent one PATCH per renumbered card. Now: one PUT. And since
	// TASK-2230 a move to the top writes the moved card alone, below the rest
	// (each written card is also a 'reordered' activity row), so the plan is one
	// update. A plan of several still goes in one request: reorderPlan.test.ts
	// 'persists every write in ONE request'.
	await expect.poll(() => writes.filter((w) => w.path.endsWith('/items/sort-order')).length).toBe(1);
	const put = writes.find((w) => w.path.endsWith('/items/sort-order'))!;
	expect(put.method).toBe('PUT');
	expect(put.status).toBe(200);
	const updates = JSON.parse(put.body).updates as { id: string; sort_order: number }[];
	expect(updates, 'a move to the top writes the moved card alone').toHaveLength(1);
	expect(updates[0].sort_order, 'below every card in the lane (min - 1)').toBe(-1);
	expect(
		writes.filter((w) => w.method === 'PATCH' && w.body.includes('sort_order')),
		'no per-row sort_order PATCH'
	).toEqual([]);

	// The server holds the new order.
	await page.reload();
	await expect.poll(order, 'after a reload').toEqual([3, 0, 1, 2]);
});

// A move that must renumber SEVERAL cards still goes in one request (the
// subject of TASK-3517), end to end. In a dense lane of five (0..4), moving the
// fourth card up gives a, b, d, c, e: pushing the left side down would write
// three cards, pushing the right side up writes two (c -> 4, e -> 5), so the
// plan is exactly those two, in one PUT (TASK-2230 writes the fewest).
test('TASK-3517: a middle move that renumbers several cards sends them in one request', async ({ page, fixture, request }, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'one browser is enough');
	const { titles, collSlug } = await seedLane(request, fixture, 5);
	await browserLogin(page);
	await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/${collSlug}?view=board`);
	const order = async () =>
		(await page.locator('.board-view .item-card .card-title').allInnerTexts()).map((t) => titles.indexOf(t.trim())).filter((i) => i >= 0);
	await expect.poll(order).toEqual([0, 1, 2, 3, 4]);

	const writes = recordWrites(page);
	const card = page.locator('.item-card').filter({ has: page.locator('.card-title', { hasText: titles[3] }) });
	await card.hover();
	await card.locator('.iam-trigger').click();
	await page.getByRole('menuitem', { name: 'Move up' }).click();
	await expect.poll(order, 'on screen').toEqual([0, 1, 3, 2, 4]);

	await expect.poll(() => writes.filter((w) => w.path.endsWith('/items/sort-order')).length).toBe(1);
	const put = writes.find((w) => w.path.endsWith('/items/sort-order'))!;
	expect(put.method).toBe('PUT');
	expect(put.status).toBe(200);
	const updates = JSON.parse(put.body).updates as { id: string; sort_order: number }[];
	expect(updates.length, 'several cards renumbered, in this one request').toBe(2);
	expect(updates.map((u) => u.sort_order).sort((x, y) => x - y)).toEqual([4, 5]);
	expect(
		writes.filter((w) => w.method === 'PATCH' && w.body.includes('sort_order')),
		'no per-row sort_order PATCH'
	).toEqual([]);

	await page.reload();
	await expect.poll(order, 'after a reload').toEqual([0, 1, 3, 2, 4]);
});
