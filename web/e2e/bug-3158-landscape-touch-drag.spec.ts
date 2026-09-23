import { test, expect } from './fixtures';
import { browserLogin } from './lib/collab-helpers';
import { CdpTouch, deleteCollection } from './lib/attachment-viewer';
import type { APIRequestContext, Page } from '@playwright/test';
import type { SuiteFixture } from './fixtures';

/**
 * BUG-3158 — drag zones were gated on WIDTH (`viewport.isMobile`, max-width
 * 768px) on the board and not at all on the list. A phone in landscape is
 * ~915 CSS px wide, so board drag was live there, and the list's was live at
 * every width. A touch held for svelte-dnd-action's 500ms delay while the
 * finger moved picked the item up and dropped it in another lane or group —
 * the BUG-3157 report's "a tap moved my card", by a second path.
 *
 * Real touch through CDP (Playwright's touchscreen can only tap), on the
 * Pixel 7 project (hasTouch, isMobile). The assertion is the stored status,
 * read back from the server.
 *
 * EACH TEST OWNS ITS COLLECTION. The suite runs in parallel against one
 * workspace, so the shared `tasks` lanes and groups hold other specs' items:
 * in CI the list's `open` group was long enough to push the next group
 * ~1300px down, off screen. Because every leg here asserts that an item did
 * NOT move, a gesture that missed its target would pass vacuously — so each
 * leg also asserts, before the gesture, that both of its ends are on screen.
 */

const LANDSCAPE = { width: 915, height: 412 };
const PORTRAIT = { width: 412, height: 915 };
const STATUSES = ['open', 'in-progress', 'done'];

function authHeaders(fixture: SuiteFixture) {
	return { Authorization: `Bearer ${fixture.apiToken}`, 'Content-Type': 'application/json' };
}

async function scratchCollection(fixture: SuiteFixture, request: APIRequestContext) {
	const stamp = Date.now();
	const schema = JSON.stringify({
		fields: [{ key: 'status', label: 'Status', type: 'select', options: STATUSES, default: 'open' }],
	});
	const res = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/collections`, {
		headers: authHeaders(fixture),
		data: { name: `B3158 ${stamp}`, prefix: `BD${String(stamp).slice(-4)}`, schema },
	});
	expect(res.ok(), await res.text()).toBeTruthy();
	return (await res.json()) as { slug: string };
}

async function seed(fixture: SuiteFixture, request: APIRequestContext, coll: string, title: string, status: string) {
	const res = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/collections/${coll}/items`, {
		headers: authHeaders(fixture),
		data: { title, fields: JSON.stringify({ status }), content: '' },
	});
	expect(res.ok(), await res.text()).toBeTruthy();
	return (await res.json()) as { slug: string };
}

async function storedStatus(request: APIRequestContext, fixture: SuiteFixture, slug: string) {
	const res = await request.get(`/api/v1/workspaces/${fixture.workspaceSlug}/items/${slug}`, {
		headers: authHeaders(fixture),
	});
	expect(res.ok(), await res.text()).toBeTruthy();
	return JSON.parse((await res.json()).fields || '{}').status as string;
}

function cardIn(page: Page, scope: string, title: string) {
	return page.locator(`${scope} .item-card`).filter({ has: page.locator('.card-title', { hasText: title }) });
}

/** Both ends of the gesture must be on screen, or "did not move" proves nothing. */
function expectOnScreen(p: { x: number; y: number }, vp: { width: number; height: number }, what: string) {
	expect(p.x, `${what} is off screen horizontally`).toBeGreaterThan(0);
	expect(p.x, `${what} is off screen horizontally`).toBeLessThan(vp.width);
	expect(p.y, `${what} is off screen vertically`).toBeGreaterThan(0);
	expect(p.y, `${what} is off screen vertically`).toBeLessThan(vp.height);
}

async function touchDrag(page: Page, start: { x: number; y: number }, end: { x: number; y: number }) {
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
}

async function boardDrag(
	page: Page,
	fixture: SuiteFixture,
	request: APIRequestContext,
	viewport: { width: number; height: number },
	expectWidthMobile: boolean,
) {
	await page.setViewportSize(viewport);
	await browserLogin(page);
	const coll = await scratchCollection(fixture, request);
	try {
		const stamp = Date.now();
		const title = `B3158 board ${stamp}`;
		const item = await seed(fixture, request, coll.slug, title, 'open');
		// An anchor in the NEXT lane. An empty lane's drop zone is only a few
		// pixels tall, so a gesture aimed "near the top" of it can land outside
		// the zone and drop nowhere — on unfixed main that made this leg pass
		// with drag LIVE. Aiming at a real card inside the zone removes that.
		const anchorTitle = `B3158 board anchor ${stamp}`;
		await seed(fixture, request, coll.slug, anchorTitle, 'in-progress');
		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/${coll.slug}?view=board`);
		const card = cardIn(page, '.board-view', title);
		await expect(card).toBeVisible();

		// PRECONDITIONS, read from the page: the width rule and the pointer.
		expect(await page.evaluate(() => window.matchMedia('(max-width: 768px)').matches)).toBe(expectWidthMobile);
		expect(await page.evaluate(() => window.matchMedia('(pointer: coarse)').matches)).toBe(true);

		const lanes = page.locator('.board-view .kanban-column');
		await expect(lanes).toHaveCount(STATUSES.length);
		const zone = lanes.nth(1).locator('.column-cards');
		const anchor = cardIn(page, '.board-view', anchorTitle);
		await expect(zone.locator('.card-title', { hasText: anchorTitle }), 'anchor is not in the next lane').toHaveCount(1);
		await anchor.scrollIntoViewIfNeeded();
		const a = (await card.boundingBox())!;
		const b = (await anchor.boundingBox())!;
		const z = (await zone.boundingBox())!;
		// The card's MIDDLE (its title). The top row holds the ref and its copy
		// button, and a press that starts on a button never picks the card up —
		// in this collection the short ref put that button under a top-edge
		// press, and the leg passed on unfixed main without a drag happening.
		const start = { x: a.x + a.width / 2, y: a.y + a.height / 2 };
		const end = { x: b.x + b.width / 2, y: b.y + b.height / 2 };
		expectOnScreen(start, viewport, 'the card');
		expectOnScreen(end, viewport, 'the next lane');
		expect(end.y > z.y && end.y < z.y + z.height && end.x > z.x && end.x < z.x + z.width,
			'the gesture ends outside the next lane\u2019s drop zone').toBe(true);

		await touchDrag(page, start, end);
		return await storedStatus(request, fixture, item.slug);
	} finally {
		await deleteCollection(fixture, request, coll.slug);
	}
}

test('BUG-3158: a touch long-press and drag on a LANDSCAPE phone does not move a card', async ({
	page,
	fixture,
	request,
}, testInfo) => {
	test.skip(testInfo.project.name !== 'mobile-chromium', 'needs a touch device');
	expect(
		await boardDrag(page, fixture, request, LANDSCAPE, false),
		'a touch long-press and drag moved the card to another lane',
	).toBe('open');
});

// NO PORTRAIT-BOARD LEG, deliberately. It existed and passed, but measured
// nothing: at portrait width the board shows about one lane, so no single
// on-screen drag reaches the next lane, and "the card did not move" held
// whether or not drag was live. The on-screen preconditions above exposed it.
// The width half of the predicate is pinned by breakpoint.svelte.test.ts.

/**
 * The LIST half (lead-approved scope): ListView's item zone had NO drag gate at
 * all, so on a phone in any orientation a held finger could drag a row into
 * another group — on a status-grouped list, another status.
 */
test('BUG-3158: a touch long-press and drag on a PORTRAIT phone LIST does not move a row to another group', async ({
	page,
	fixture,
	request,
}, testInfo) => {
	test.skip(testInfo.project.name !== 'mobile-chromium', 'needs a touch device');
	await page.setViewportSize(PORTRAIT);
	await browserLogin(page);
	const coll = await scratchCollection(fixture, request);
	try {
		const stamp = Date.now();
		const title = `B3158 list ${stamp}`;
		const item = await seed(fixture, request, coll.slug, title, 'open');
		// Guarantees an in-progress group exists to drop into.
		const anchorTitle = `B3158 list anchor ${stamp}`;
		await seed(fixture, request, coll.slug, anchorTitle, 'in-progress');

		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/${coll.slug}?view=list`);
		const row = cardIn(page, '.list-view', title);
		const anchor = cardIn(page, '.list-view', anchorTitle);
		await expect(row).toBeVisible();
		await expect(anchor).toBeVisible();
		const a = (await row.boundingBox())!;
		const b = (await anchor.boundingBox())!;
		// The card's MIDDLE (its title). The top row holds the ref and its copy
		// button, and a press that starts on a button never picks the card up —
		// in this collection the short ref put that button under a top-edge
		// press, and the leg passed on unfixed main without a drag happening.
		const start = { x: a.x + a.width / 2, y: a.y + a.height / 2 };
		const end = { x: b.x + b.width / 2, y: b.y + b.height / 2 };
		expectOnScreen(start, PORTRAIT, 'the row');
		expectOnScreen(end, PORTRAIT, 'the in-progress group');

		await touchDrag(page, start, end);
		expect(
			await storedStatus(request, fixture, item.slug),
			'a touch long-press and drag moved the row into another status group',
		).toBe('open');
	} finally {
		await deleteCollection(fixture, request, coll.slug);
	}
});

test('BUG-3158 CONTROL: a MOUSE drag on desktop still moves a card between lanes', async ({
	page,
	fixture,
	request,
}, testInfo) => {
	// The counterfactual for the legs above: the gate withholds TOUCH drag, not
	// drag. Without this, disabling every zone everywhere would pass them all.
	test.skip(testInfo.project.name !== 'desktop-chromium', 'a mouse device');
	const vp = { width: 1400, height: 900 };
	await page.setViewportSize(vp);
	await browserLogin(page);
	const coll = await scratchCollection(fixture, request);
	try {
		const title = `B3158 mouse ${Date.now()}`;
		const item = await seed(fixture, request, coll.slug, title, 'open');
		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/${coll.slug}?view=board`);
		const card = cardIn(page, '.board-view', title);
		await expect(card).toBeVisible();
		expect(await page.evaluate(() => window.matchMedia('(pointer: coarse)').matches)).toBe(false);

		const a = (await card.boundingBox())!;
		const b = (await page.locator('.board-view .kanban-column').nth(1).locator('.column-cards').boundingBox())!;
		const start = { x: a.x + a.width / 2, y: a.y + 15 };
		const end = { x: b.x + b.width / 2, y: b.y + 30 };
		expectOnScreen(start, vp, 'the card');
		expectOnScreen(end, vp, 'the next lane');
		await page.mouse.move(start.x, start.y);
		await page.mouse.down();
		const steps = 15;
		for (let s = 1; s <= steps; s++) {
			await page.mouse.move(start.x + ((end.x - start.x) * s) / steps, start.y + ((end.y - start.y) * s) / steps);
			await page.waitForTimeout(20);
		}
		await page.mouse.up();
		await expect.poll(() => storedStatus(request, fixture, item.slug), { timeout: 5000 }).toBe('in-progress');
	} finally {
		await deleteCollection(fixture, request, coll.slug);
	}
});
