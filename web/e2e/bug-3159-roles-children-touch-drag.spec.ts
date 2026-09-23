import { test, expect } from './fixtures';
import { browserLogin } from './lib/collab-helpers';
import { CdpTouch, deleteCollection } from './lib/attachment-viewer';
import type { APIRequestContext, Page } from '@playwright/test';
import type { SuiteFixture } from './fixtures';

/**
 * BUG-3159 — the two drag zones BUG-3158 left out, same class: a finger held
 * past svelte-dnd-action's 500ms delayTouchStart picks an item up.
 *
 * - Roles board: a cross-lane drop writes `agent_role_id`, and when the item
 *   had no assignee, ALSO `assigned_user_id` = the current user.
 * - ChildItems: a drop writes only `sort_order` (never status) — a stray
 *   reorder of the parent's children.
 *
 * Built on #1461's lessons: every fixture is scratch (own collection, own
 * roles), the gesture starts in an item's middle and ends on an anchor item
 * inside the target zone, and both ends are asserted on screen BEFORE the
 * gesture, so "nothing changed" cannot pass because the gesture missed.
 */

const LANDSCAPE = { width: 915, height: 412 };

// Scratch fixtures are removed after each test, once its assertions have read them.
const cleanups: Array<() => Promise<void>> = [];
test.afterEach(async () => {
	while (cleanups.length) await cleanups.pop()!();
});
const PORTRAIT = { width: 412, height: 915 };

function authHeaders(fixture: SuiteFixture) {
	return { Authorization: `Bearer ${fixture.apiToken}`, 'Content-Type': 'application/json' };
}

async function api(request: APIRequestContext, fixture: SuiteFixture, method: 'get' | 'post' | 'delete', path: string, data?: unknown) {
	const res = await request[method](`/api/v1/workspaces/${fixture.workspaceSlug}${path}`, {
		headers: authHeaders(fixture),
		...(data !== undefined ? { data } : {}),
	});
	expect(res.ok(), `${method} ${path}: ${await res.text()}`).toBeTruthy();
	return method === 'delete' ? null : res.json();
}

async function scratchCollection(fixture: SuiteFixture, request: APIRequestContext) {
	const stamp = Date.now();
	const schema = JSON.stringify({
		fields: [{ key: 'status', label: 'Status', type: 'select', options: ['open', 'done'], default: 'open' }],
	});
	return api(request, fixture, 'post', '/collections', {
		name: `B3159 ${stamp}`,
		prefix: `BE${String(stamp).slice(-4)}`,
		schema,
	}) as Promise<{ slug: string }>;
}

function expectOnScreen(p: { x: number; y: number }, vp: { width: number; height: number }, what: string) {
	expect(p.x, `${what} is off screen horizontally`).toBeGreaterThan(0);
	expect(p.x, `${what} is off screen horizontally`).toBeLessThan(vp.width);
	expect(p.y, `${what} is off screen vertically`).toBeGreaterThan(0);
	expect(p.y, `${what} is off screen vertically`).toBeLessThan(vp.height);
}

/**
 * The point must land ON `target`, not merely inside the viewport: a card under
 * the sidebar is within the viewport's bounds and still unreachable.
 */
async function expectReachable(page: Page, target: import('@playwright/test').Locator, p: { x: number; y: number }, what: string) {
	const hit = await target.evaluate((el, pt) => {
		const at = document.elementFromPoint(pt.x, pt.y);
		return !!at && (el === at || el.contains(at));
	}, p);
	expect(hit, `${what} is covered or off screen at the gesture point`).toBe(true);
}

function middle(b: { x: number; y: number; width: number; height: number }) {
	return { x: b.x + b.width / 2, y: b.y + b.height / 2 };
}

async function touchDrag(page: Page, start: { x: number; y: number }, end: { x: number; y: number }) {
	const touch = await CdpTouch.attach(page);
	await touch.down(1, start.x, start.y);
	await page.waitForTimeout(700); // past delayTouchStart (500ms)
	const steps = 12;
	for (let s = 1; s <= steps; s++) {
		await touch.move(1, start.x + ((end.x - start.x) * s) / steps, start.y + ((end.y - start.y) * s) / steps);
		await page.waitForTimeout(30);
	}
	await page.waitForTimeout(200);
	await touch.lift(1);
	await page.waitForTimeout(1500);
}

async function mouseDrag(page: Page, start: { x: number; y: number }, end: { x: number; y: number }) {
	await page.mouse.move(start.x, start.y);
	await page.mouse.down();
	const steps = 15;
	for (let s = 1; s <= steps; s++) {
		await page.mouse.move(start.x + ((end.x - start.x) * s) / steps, start.y + ((end.y - start.y) * s) / steps);
		await page.waitForTimeout(20);
	}
	await page.mouse.up();
}

/**
 * Two scratch roles, an UNASSIGNED item in the first and an anchor in the
 * second; the gesture drags the item onto the anchor. Returns the item as
 * stored afterwards.
 */
async function rolesDrag(
	page: Page,
	fixture: SuiteFixture,
	request: APIRequestContext,
	viewport: { width: number; height: number },
	drag: typeof touchDrag,
) {
	await page.setViewportSize(viewport);
	await browserLogin(page);
	const stamp = Date.now();
	// Each scratch resource registers its cleanup the moment it exists, so a
	// failure partway through setup leaks nothing (codex round 1).
	const coll = await scratchCollection(fixture, request);
	cleanups.push(() => deleteCollection(fixture, request, coll.slug));
	// Lanes sort by NAME, so the stamp goes first: the pair sorts together and
	// its two lanes are adjacent whatever other roles exist.
	const roleA = await api(request, fixture, 'post', '/agent-roles', { name: `B3159 ${stamp} A` });
	cleanups.push(() => deleteRole(fixture, request, roleA));
	const roleB = await api(request, fixture, 'post', '/agent-roles', { name: `B3159 ${stamp} B` });
	cleanups.push(() => deleteRole(fixture, request, roleB));
	{
		const title = `B3159 role item ${stamp}`;
		const anchorTitle = `B3159 role anchor ${stamp}`;
		const item = await api(request, fixture, 'post', `/collections/${coll.slug}/items`, {
			title, fields: JSON.stringify({ status: 'open' }), agent_role_id: roleA.id,
		});
		expect(item.assigned_user_id ?? null, 'precondition: the item has no assignee').toBeNull();
		await api(request, fixture, 'post', `/collections/${coll.slug}/items`, {
			title: anchorTitle, fields: JSON.stringify({ status: 'open' }), agent_role_id: roleB.id,
		});

		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/roles`);
		const laneA = page.locator('.lane').filter({ has: page.locator('.lane-name', { hasText: roleA.name }) });
		const laneB = page.locator('.lane').filter({ has: page.locator('.lane-name', { hasText: roleB.name }) });
		const card = laneA.locator('.item-card').filter({ hasText: title });
		const anchor = laneB.locator('.item-card').filter({ hasText: anchorTitle });
		await expect(card).toBeVisible();
		// Put lane A at the left edge, so it and the lane after it (created
		// together, so adjacent) are on screen at once. Scrolling to B alone
		// pushed A off screen whenever other roles sat before them.
		await laneA.evaluate((el) => el.scrollIntoView({ inline: 'start', block: 'nearest' }));
		await expect(anchor).toBeVisible();

		const start = middle((await card.boundingBox())!);
		const end = middle((await anchor.boundingBox())!);
		const zone = (await laneB.locator('.lane-items').boundingBox())!;
		expectOnScreen(start, viewport, 'the item');
		expectOnScreen(end, viewport, 'the other role’s lane');
		await expectReachable(page, card, start, 'the item');
		await expectReachable(page, anchor, end, 'the anchor in the other role’s lane');
		expect(end.y > zone.y && end.y < zone.y + zone.height && end.x > zone.x && end.x < zone.x + zone.width,
			'the gesture ends outside the target lane').toBe(true);

		await drag(page, start, end);
		const read = () => api(request, fixture, 'get', `/items/${item.slug}`);
		return { read, roleA, roleB };
	}
}

async function deleteRole(fixture: SuiteFixture, request: APIRequestContext, r: { id: string; name: string }) {
	const res = await request.delete(`/api/v1/workspaces/${fixture.workspaceSlug}/agent-roles/${r.id}`, { headers: authHeaders(fixture) });
	// Warn, never throw: a throw here would replace the test's real failure.
	if (!res.ok()) console.warn(`[bug-3159] leaked role ${r.name}: ${res.status()} ${await res.text()}`);
}

test('BUG-3159: a touch drag on the ROLES board in landscape changes neither role nor assignee', async ({
	page,
	fixture,
	request,
}, testInfo) => {
	test.skip(testInfo.project.name !== 'mobile-chromium', 'needs a touch device');
	const { read, roleA } = await rolesDrag(page, fixture, request, LANDSCAPE, touchDrag);
	const after = await read(); // touchDrag already waits 1.5s after the lift
	expect(after.agent_role_id, 'a touch drag moved the item to another role').toBe(roleA.id);
	expect(after.assigned_user_id ?? null, 'a touch drag assigned the item to the current user').toBeNull();
});

test('BUG-3159 CONTROL: a MOUSE drag on the desktop roles board still moves the role', async ({
	page,
	fixture,
	request,
}, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'a mouse device');
	const { read, roleB } = await rolesDrag(page, fixture, request, { width: 1400, height: 900 }, mouseDrag);
	// POLLED: the write lands after the drop, and reading at once saw the old role.
	await expect.poll(async () => (await read()).agent_role_id, {
		message: 'the mouse drag did not move the role — drag was disabled too widely',
		timeout: 5000,
	}).toBe(roleB.id);
});

/**
 * A scratch parent with two open children; the gesture drags the first child
 * onto the second. Returns both children's sort_order before and after.
 */
async function childrenDrag(
	page: Page,
	fixture: SuiteFixture,
	request: APIRequestContext,
	viewport: { width: number; height: number },
	drag: typeof touchDrag,
) {
	await page.setViewportSize(viewport);
	await browserLogin(page);
	const stamp = Date.now();
	const coll = await scratchCollection(fixture, request);
	cleanups.push(() => deleteCollection(fixture, request, coll.slug));
	{
		const parent = await api(request, fixture, 'post', `/collections/${coll.slug}/items`, {
			title: `B3159 parent ${stamp}`, fields: JSON.stringify({ status: 'open' }),
		});
		const first = `B3159 child one ${stamp}`;
		const second = `B3159 child two ${stamp}`;
		const c1 = await api(request, fixture, 'post', `/collections/${coll.slug}/items`, {
			title: first, fields: JSON.stringify({ status: 'open', parent: parent.id }),
		});
		const c2 = await api(request, fixture, 'post', `/collections/${coll.slug}/items`, {
			title: second, fields: JSON.stringify({ status: 'open', parent: parent.id }),
		});
		const order = async () =>
			(await api(request, fixture, 'get', `/items/${parent.slug}/children`) as Array<{ id: string; sort_order: number }>)
				.map((c) => `${c.id === c1.id ? 'one' : c.id === c2.id ? 'two' : '?'}:${c.sort_order}`)
				.join(',');
		const before = await order();

		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/${coll.slug}/${parent.slug}`);
		// The child list lives on the Relationships tab, not Details.
		await page.getByRole('tab', { name: 'Relationships' }).or(page.getByRole('button', { name: 'Relationships' })).first().click();
		const row1 = page.locator('.child-row').filter({ hasText: first });
		const row2 = page.locator('.child-row').filter({ hasText: second });
		await row2.scrollIntoViewIfNeeded();
		await expect(row1).toBeVisible();
		await expect(row2).toBeVisible();
		const b1 = (await row1.boundingBox())!;
		const b2 = (await row2.boundingBox())!;
		// Onto the LOWER half of the second row, so a live drag reorders.
		const start = middle(b1);
		const end = { x: b2.x + b2.width / 2, y: b2.y + b2.height * 0.8 };
		expectOnScreen(start, viewport, 'the first child');
		expectOnScreen(end, viewport, 'the second child');
		await expectReachable(page, row1, start, 'the first child');
		await expectReachable(page, row2, end, 'the second child');

		await drag(page, start, end);
		return { before, read: order };
	}
}

test('BUG-3159: a touch drag on a PORTRAIT phone does not reorder an item’s children', async ({
	page,
	fixture,
	request,
}, testInfo) => {
	test.skip(testInfo.project.name !== 'mobile-chromium', 'needs a touch device');
	const { before, read } = await childrenDrag(page, fixture, request, PORTRAIT, touchDrag);
	expect(await read(), 'a touch drag reordered the children').toBe(before);
});

test('BUG-3159 CONTROL: a MOUSE drag on desktop still reorders children', async ({
	page,
	fixture,
	request,
}, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'a mouse device');
	const { before, read } = await childrenDrag(page, fixture, request, { width: 1400, height: 900 }, mouseDrag);
	await expect.poll(read, { message: 'the mouse drag did not reorder — drag was disabled too widely', timeout: 5000 }).not.toBe(before);
});
