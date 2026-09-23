import { test, expect } from './fixtures';
import { browserLogin } from './lib/collab-helpers';
import { CdpTouch } from './lib/attachment-viewer';
import type { APIRequestContext, Page } from '@playwright/test';
import type { SuiteFixture } from './fixtures';

/**
 * BUG-3158 — board drag was gated on WIDTH (`viewport.isMobile`, max-width
 * 768px), not on the input. A phone in landscape is ~915 CSS px wide, so drag
 * was live there, and a touch held for svelte-dnd-action's 500ms delay while
 * the finger moved picked the card up and dropped it in another lane — the
 * BUG-3157 report's "a tap moved my card", by a second path.
 *
 * Real touch through CDP (Playwright's touchscreen can only tap), on the
 * Pixel 7 project (hasTouch, isMobile) at a landscape viewport. The assertion
 * is the stored status, read back from the server.
 */

const LANDSCAPE = { width: 915, height: 412 };

function authHeaders(fixture: SuiteFixture) {
	return { Authorization: `Bearer ${fixture.apiToken}`, 'Content-Type': 'application/json' };
}

async function storedStatus(request: APIRequestContext, fixture: SuiteFixture, slug: string) {
	const res = await request.get(`/api/v1/workspaces/${fixture.workspaceSlug}/items/${slug}`, {
		headers: authHeaders(fixture),
	});
	expect(res.ok(), await res.text()).toBeTruthy();
	return JSON.parse((await res.json()).fields || '{}').status as string;
}

function itemCard(page: Page, title: string) {
	return page
		.locator('.board-view .item-card')
		.filter({ has: page.locator('.card-title', { hasText: title }) });
}

async function longPressDragToNextLane(
	page: Page,
	fixture: SuiteFixture,
	request: APIRequestContext,
	viewport: { width: number; height: number },
	expectWidthMobile: boolean,
) {
	await page.setViewportSize(viewport);
	await browserLogin(page);
	const title = `B3158 landscape drag ${Date.now()}`;
	const created = await request.post(
		`/api/v1/workspaces/${fixture.workspaceSlug}/collections/tasks/items`,
		{ headers: authHeaders(fixture), data: { title, fields: JSON.stringify({ status: 'open' }), content: '' } },
	);
	expect(created.ok(), await created.text()).toBeTruthy();
	const item = await created.json();

	await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/tasks`);
	const card = itemCard(page, title);
	await expect(card).toBeVisible();

	// PRECONDITION: this viewport is NOT "mobile" by the app's width rule, which
	// is the configuration under test. Read from the page, not assumed.
	expect(await page.evaluate(() => window.matchMedia('(max-width: 768px)').matches)).toBe(expectWidthMobile);
	expect(await page.evaluate(() => window.matchMedia('(pointer: coarse)').matches)).toBe(true);

	// The card's lane and the next one.
	const lanes = page.locator('.board-view .kanban-column');
	const laneCount = await lanes.count();
	let from = -1;
	for (let i = 0; i < laneCount; i++) {
		if ((await lanes.nth(i).locator('.card-title', { hasText: title }).count()) > 0) from = i;
	}
	expect(from, 'card is in no lane').toBeGreaterThanOrEqual(0);
	const target = lanes.nth(from + 1).locator('.column-cards');
	await target.scrollIntoViewIfNeeded();
	await card.scrollIntoViewIfNeeded();

	const a = (await card.boundingBox())!;
	const b = (await target.boundingBox())!;
	const start = { x: a.x + a.width / 2, y: a.y + Math.min(20, a.height / 2) };
	const end = { x: b.x + b.width / 2, y: b.y + 30 };

	const touch = await CdpTouch.attach(page);
	await touch.down(1, start.x, start.y);
	await page.waitForTimeout(700); // past svelte-dnd-action's 500ms delayTouchStart
	const steps = 12;
	for (let s = 1; s <= steps; s++) {
		await touch.move(1, start.x + ((end.x - start.x) * s) / steps, start.y + ((end.y - start.y) * s) / steps);
		await page.waitForTimeout(30);
	}
	await page.waitForTimeout(200);
	await touch.lift(1);

	await page.waitForTimeout(1500);
	return storedStatus(request, fixture, item.slug);
}

test('BUG-3158: a touch long-press and drag on a LANDSCAPE phone does not move a card', async ({
	page,
	fixture,
	request,
}, testInfo) => {
	test.skip(testInfo.project.name !== 'mobile-chromium', 'needs a touch device');
	expect(
		await longPressDragToNextLane(page, fixture, request, LANDSCAPE, false),
		'a touch long-press and drag moved the card to another lane',
	).toBe('open');
});

test('BUG-3158 CONTROL: the same gesture in PORTRAIT does not move a card either', async ({
	page,
	fixture,
	request,
}, testInfo) => {
	// The width gate already covered portrait; this pins that the fix keeps it.
	test.skip(testInfo.project.name !== 'mobile-chromium', 'needs a touch device');
	expect(await longPressDragToNextLane(page, fixture, request, { width: 412, height: 915 }, true)).toBe('open');
});

/**
 * The LIST half (lead-approved scope): ListView's item zone had NO drag gate at
 * all, so on a phone in any orientation a held finger could drag a row into
 * another group — on a status-grouped list, another status.
 */
async function longPressDragToNextGroup(
	page: Page,
	fixture: SuiteFixture,
	request: APIRequestContext,
	viewport: { width: number; height: number },
) {
	await page.setViewportSize(viewport);
	await browserLogin(page);
	const stamp = Date.now();
	const title = `B3158 list drag ${stamp}`;
	const mk = async (t: string, status: string) => {
		const r = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/collections/tasks/items`, {
			headers: authHeaders(fixture),
			data: { title: t, fields: JSON.stringify({ status }), content: '' },
		});
		expect(r.ok(), await r.text()).toBeTruthy();
		return r.json();
	};
	const item = await mk(title, 'open');
	// Guarantees an in-progress group exists to drop into.
	const anchorTitle = `B3158 list anchor ${stamp}`;
	await mk(anchorTitle, 'in-progress');

	await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/tasks?view=list`);
	const row = page.locator('.list-view .item-card').filter({ has: page.locator('.card-title', { hasText: title }) });
	await expect(row).toBeVisible();
	const anchor = page.locator('.list-view .item-card').filter({ has: page.locator('.card-title', { hasText: anchorTitle }) });
	await anchor.scrollIntoViewIfNeeded();
	await row.scrollIntoViewIfNeeded();

	const a = (await row.boundingBox())!;
	const b = (await anchor.boundingBox())!;
	const start = { x: a.x + a.width / 2, y: a.y + Math.min(20, a.height / 2) };
	const end = { x: b.x + b.width / 2, y: b.y + b.height / 2 };
	expect(Math.abs(end.y - start.y), 'the two groups are not both on screen').toBeLessThan(viewport.height);

	const touch = await CdpTouch.attach(page);
	await touch.down(1, start.x, start.y);
	await page.waitForTimeout(700);
	const steps = 12;
	for (let s = 1; s <= steps; s++) {
		await touch.move(1, start.x + ((end.x - start.x) * s) / steps, start.y + ((end.y - start.y) * s) / steps);
		await page.waitForTimeout(30);
	}
	await page.waitForTimeout(200);
	await touch.lift(1);
	await page.waitForTimeout(1500);
	return storedStatus(request, fixture, item.slug);
}

test('BUG-3158: a touch long-press and drag on a PORTRAIT phone LIST does not move a row to another group', async ({
	page,
	fixture,
	request,
}, testInfo) => {
	test.skip(testInfo.project.name !== 'mobile-chromium', 'needs a touch device');
	expect(
		await longPressDragToNextGroup(page, fixture, request, { width: 412, height: 915 }),
		'a touch long-press and drag moved the row into another status group',
	).toBe('open');
});

test('BUG-3158 CONTROL: a MOUSE drag on desktop still moves a card between lanes', async ({
	page,
	fixture,
	request,
}, testInfo) => {
	// The counterfactual for the legs above: the gate withholds TOUCH drag, not
	// drag. Without this, disabling every zone everywhere would pass them all.
	test.skip(testInfo.project.name !== 'desktop-chromium', 'a mouse device');
	await page.setViewportSize({ width: 1400, height: 900 });
	await browserLogin(page);
	const title = `B3158 mouse drag ${Date.now()}`;
	const created = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/collections/tasks/items`, {
		headers: authHeaders(fixture),
		data: { title, fields: JSON.stringify({ status: 'open' }), content: '' },
	});
	expect(created.ok(), await created.text()).toBeTruthy();
	const item = await created.json();
	await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/tasks`);
	const card = itemCard(page, title);
	await expect(card).toBeVisible();
	expect(await page.evaluate(() => window.matchMedia('(pointer: coarse)').matches)).toBe(false);

	const lanes = page.locator('.board-view .kanban-column');
	let from = -1;
	for (let i = 0; i < (await lanes.count()); i++) {
		if ((await lanes.nth(i).locator('.card-title', { hasText: title }).count()) > 0) from = i;
	}
	const a = (await card.boundingBox())!;
	const b = (await lanes.nth(from + 1).locator('.column-cards').boundingBox())!;
	await page.mouse.move(a.x + a.width / 2, a.y + 15);
	await page.mouse.down();
	const steps = 15;
	for (let s = 1; s <= steps; s++) {
		await page.mouse.move(
			a.x + a.width / 2 + ((b.x + b.width / 2 - (a.x + a.width / 2)) * s) / steps,
			a.y + 15 + ((b.y + 30 - (a.y + 15)) * s) / steps,
		);
		await page.waitForTimeout(20);
	}
	await page.mouse.up();
	await expect.poll(() => storedStatus(request, fixture, item.slug), { timeout: 5000 }).not.toBe('open');
});
