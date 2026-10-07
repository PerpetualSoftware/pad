import type { APIRequestContext, Page, Route } from '@playwright/test';
import { test, expect } from './fixtures';
import { browserLogin } from './lib/collab-helpers';

/**
 * BUG-3204 — with the split pane open, j moves the list cursor at once and the
 * pane follows ~140ms later (the pane-follow debounce). The route also snaps
 * the cursor to the pane's item whenever the list changes ("keep the row
 * highlight on the OPEN pane's item"). A list change inside that debounce
 * window therefore snapped the cursor BACK to the pane's old item, then the
 * pane followed and it snapped forward again: a visible flicker, and the
 * cause of attachment-viewer-owners' owner-2 flake (CI trace: alpha, alpha,
 * BRAVO, alpha within 600ms of one j). In CI the list change was another
 * worker writing to the shared workspace; for a user, anyone editing the list.
 *
 * A list change reaches the page through SSE -> a coalesced reconcile ->
 * `/items-changes`, which rarely lands inside 140ms on its own (an unheld run
 * reproduced 1 in 20). So the tests make the window as wide as they need:
 * the page's clock is FROZEN (`page.clock`) just before j, so the follow's 140ms
 * timer cannot fire until the test runs the clock on. The list-change tests
 * HOLD that fetch, having already read the server's answer, and hand it over
 * after j. The change then lands inside the debounce on any machine at any
 * load (a real-clock version missed the window 9 times in 30 on a loaded box).
 *
 * The page records every change of the focused row, `?item=`, the card count
 * and the card text, so a snap-back shows even when it lasts a frame. Rows are
 * tracked by ref (`data-item-key`), which a rename keeps.
 */

type Made = { id: string; slug: string; title: string };
type Tick = { focused: string; item: string; cards: number; text: string };
type Fixture = { apiToken: string; workspaceSlug: string; adminUsername: string };

async function setup(page: Page, request: APIRequestContext, fixture: Fixture, index: number) {
	await page.setViewportSize({ width: 1400, height: 900 });
	await browserLogin(page);
	const h = { Authorization: `Bearer ${fixture.apiToken}`, 'Content-Type': 'application/json' };
	const ws = fixture.workspaceSlug;
	const stamp = `${Date.now()}-${index}`;
	const res = await request.post(`/api/v1/workspaces/${ws}/collections`, {
		headers: h,
		data: { name: `B3204 ${stamp}`, prefix: `BS${String(Date.now()).slice(-5)}` }
	});
	expect(res.ok(), await res.text()).toBeTruthy();
	const coll = await res.json();
	const cleanup = () => request.delete(`/api/v1/workspaces/${ws}/collections/${coll.slug}`, { headers: h });
	const made: Made[] = [];
	for (const t of ['first', 'second', 'third']) {
		const r = await request.post(`/api/v1/workspaces/${ws}/collections/${coll.slug}/items`, {
			headers: h,
			data: { title: `B3204 ${t} ${stamp}` }
		});
		expect(r.ok(), await r.text()).toBeTruthy();
		made.push(await r.json());
	}
	await page.goto(`/${fixture.adminUsername}/${ws}/${coll.slug}?view=list`);
	const cards = page.locator('.item-card');
	await expect(cards).toHaveCount(3);
	const keys = await cards.evaluateAll((els) => els.map((e) => e.getAttribute('data-item-key') ?? ''));
	const slugs = await cards.evaluateAll((els) => els.map((e) => e.getAttribute('data-item-slug') ?? ''));
	const rows = slugs.map((s) => made.find((m) => m.slug === s)!);
	// Open the pane on the top row; the snap-back puts the cursor there.
	await cards.first().click();
	await expect(page.locator('.item-pane')).toBeVisible();
	await expect(page).toHaveURL(new RegExp(`[?&]item=${keys[0]}(&|$)`));
	await expect(page.locator('.item-card.focused')).toHaveAttribute('data-item-key', keys[0]);
	// Hand the keyboard back to the list, as owner 2 does after a click.
	await page.evaluate(() => (document.activeElement as HTMLElement | null)?.blur());
	await page.waitForTimeout(400);

	await page.evaluate(() => {
		const w = window as unknown as { __b3204: Tick[] };
		w.__b3204 = [];
		const read = (): Tick => ({
			focused: document.querySelector('.item-card.focused')?.getAttribute('data-item-key') ?? '',
			item: new URL(location.href).searchParams.get('item') ?? '',
			cards: document.querySelectorAll('.item-card').length,
			text: Array.from(document.querySelectorAll('.item-card'))
				.map((c) => c.textContent ?? '')
				.join('|')
		});
		let last = JSON.stringify(read());
		const tick = () => {
			const now = read();
			const s = JSON.stringify(now);
			if (s !== last) {
				w.__b3204.push(now);
				last = s;
			}
		};
		new MutationObserver(tick).observe(document.body, {
			subtree: true,
			attributes: true,
			characterData: true,
			childList: true
		});
		// replaceState moves the URL without touching the DOM.
		const poll = setInterval(tick, 5);
		setTimeout(() => clearInterval(poll), 5000);
	});

	// Hold every list answer that CARRIES the change (`carries`): the
	// `/items-changes` delta, and the two snapshots a reconcile or a reload can
	// fall back to (`/items-index`, the collection's own item list), where a
	// deleted row is simply absent. Each is read from the server at hold time,
	// so a release is a local fulfil that lands in milliseconds. Every other
	// answer passes straight through: the reconcile loop is single-flight, so holding a stale answer
	// from before the write would block the request that carries it.
	const held: { route: Route; response: Awaited<ReturnType<Route['fetch']>> }[] = [];
	let carries: ((url: string, body: string) => boolean) | null = null;
	await page.route(/\/api\/v1\/workspaces\/[^/]+\/(items-changes|items-index|collections\/[^/]+\/items)(\?|$)/, async (route) => {
		if (route.request().method() !== 'GET') return route.fallback();
		const response = await route.fetch();
		if (carries && carries(route.request().url(), await response.text())) held.push({ route, response });
		else await route.fulfill({ response });
	});
	const holdFor = (c: (url: string, body: string) => boolean) => {
		carries = c;
	};
	const release = async () => {
		carries = null;
		for (const { route, response } of held) await route.fulfill({ response });
	};
	const heldChange = () => expect.poll(() => held.length, { timeout: 10_000 }).toBeGreaterThan(0);
	const log = () => page.evaluate(() => (window as unknown as { __b3204: Tick[] }).__b3204);
	// Freeze the page's timers, right before j: the pane follow's debounce is
	// then armed by j and fires only when the test runs the clock past it.
	// Not earlier, because the reconcile that fetches a change is itself
	// coalesced on a timer. (`install` alone lets time run on; `pauseAt` stops
	// it. The recorder's poll was armed before the install, so it keeps real
	// time.)
	const freeze = async () => {
		await page.clock.install();
		await page.clock.pauseAt(Date.now() + 1000);
	};
	const fireFollow = () => page.clock.runFor(300);
	return { h, ws, keys, rows, holdFor, release, heldChange, freeze, fireFollow, log, cleanup };
}

test.describe('BUG-3204: the list cursor during a pending pane follow', () => {
	test.beforeEach(({}, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'the split pane is a desktop layout');
		test.setTimeout(60_000);
	});

	test('a list change while the pane follows j does not snap the cursor back', async ({
		page,
		fixture,
		request
	}, testInfo) => {
		const s = await setup(page, request, fixture, testInfo.repeatEachIndex);
		try {
			const [top, next] = s.keys;
			s.holdFor((_url, body) => body.includes(`${s.rows[0].title} renamed`));
			const r = await request.patch(`/api/v1/workspaces/${s.ws}/items/${s.rows[0].id}`, {
				headers: s.h,
				data: { title: `${s.rows[0].title} renamed` }
			});
			expect(r.ok(), await r.text()).toBeTruthy();
			await s.heldChange();
			await s.freeze();

			await page.keyboard.press('j');
			// j has moved the cursor before the change is handed over.
			await expect(page.locator('.item-card.focused')).toHaveAttribute('data-item-key', next);
			await s.release();
			await expect(page.locator(`.item-card[data-item-key="${top}"]`)).toContainText('renamed');
			await page.waitForTimeout(300);
			await s.fireFollow();
			await expect(page).toHaveURL(new RegExp(`[?&]item=${next}(&|$)`));
			await page.waitForTimeout(300);

			const log = await s.log();
			const trace = JSON.stringify(log.map((t) => [t.focused, t.item, t.text.includes('renamed')]));
			// The premise: the rename reached the list while `?item=` still named
			// the top row, i.e. inside the debounce. (The frozen clock makes this
			// hold; it is checked so a change to the mechanism cannot pass vacuously.)
			const renamedAt = log.findIndex((t) => t.text.includes('renamed'));
			expect(renamedAt, `the rename never reached the list: ${trace}`).toBeGreaterThanOrEqual(0);
			expect(log[renamedAt].item, `the rename landed after the follow, outside the window: ${trace}`).toBe(top);
			// The cursor left the top row and never came back, or went blank.
			const left = log.findIndex((t) => t.focused !== top);
			expect(left, `the cursor never left the top row: ${trace}`).toBeGreaterThanOrEqual(0);
			expect(
				log.slice(left).filter((t) => t.focused !== next),
				`the cursor snapped back (or vanished) after j: ${trace}`
			).toEqual([]);
			// It ends where j put it, and the pane followed it there.
			await expect(page.locator('.item-card.focused')).toHaveAttribute('data-item-key', next);
		} finally {
			await s.cleanup();
		}
	});

	test('a follow that opens nothing leaves the cursor on the open item', async ({
		page,
		fixture,
		request
	}, testInfo) => {
		// k on the top row schedules a follow whose target is the row already
		// open, so the follow opens nothing. A list change inside its debounce
		// used to blank the cursor (the list-change reset ran and nothing put it
		// back), and the pane was left with no highlighted row. (A target
		// DELETED during the debounce ends the follow the same way; a deletion
		// reaches the list by more than one door, so it cannot be held here.)
		const s = await setup(page, request, fixture, testInfo.repeatEachIndex);
		try {
			const [top] = s.keys;
			// Rename the OPEN row: the list sorts by last update, so renaming any
			// other row would move it to the top and change what k means.
			s.holdFor((_url, body) => body.includes(`${s.rows[0].title} renamed`));
			const r = await request.patch(`/api/v1/workspaces/${s.ws}/items/${s.rows[0].id}`, {
				headers: s.h,
				data: { title: `${s.rows[0].title} renamed` }
			});
			expect(r.ok(), await r.text()).toBeTruthy();
			await s.heldChange();
			await s.freeze();

			// k moves nothing on the top row, so nothing on screen says it was
			// handled; dispatching it in the page settles that before the release.
			await page.evaluate(() =>
				document.body.dispatchEvent(new KeyboardEvent('keydown', { key: 'k', code: 'KeyK', bubbles: true }))
			);
			await s.release();
			await expect(page.locator(`.item-card[data-item-key="${top}"]`)).toContainText('renamed');
			await page.waitForTimeout(300);
			await s.fireFollow();
			await page.waitForTimeout(300);

			const log = await s.log();
			const trace = JSON.stringify(log.map((t) => [t.focused, t.item, t.text.includes('renamed')]));
			// The premise: the rename reached the list inside the debounce.
			expect(log.some((t) => t.text.includes('renamed')), `the rename never reached the list: ${trace}`).toBe(true);
			// The pane stays on the top row, and so does the highlight, at every
			// point: never on no row.
			expect(new URL(page.url()).searchParams.get('item'), trace).toBe(top);
			expect(log.filter((t) => t.focused !== top), `the cursor left the open row: ${trace}`).toEqual([]);
			await expect(page.locator('.item-card.focused'), trace).toHaveAttribute('data-item-key', top);
		} finally {
			await s.cleanup();
		}
	});

	test('a row clicked while a j follow is pending wins over the keypress', async ({
		page,
		fixture,
		request
	}, testInfo) => {
		const s = await setup(page, request, fixture, testInfo.repeatEachIndex);
		try {
			const third = s.keys[2];
			await s.freeze();
			// j and a click on the third row in ONE task (and the clock frozen),
			// so the click is inside the debounce by construction.
			await page.evaluate((key) => {
				document.body.dispatchEvent(new KeyboardEvent('keydown', { key: 'j', code: 'KeyJ', bubbles: true }));
				(document.querySelector(`.item-card[data-item-key="${key}"]`) as HTMLElement).click();
			}, third);
			await expect(page).toHaveURL(new RegExp(`[?&]item=${third}(&|$)`));
			await s.fireFollow();
			await page.waitForTimeout(300);

			const log = await s.log();
			const trace = JSON.stringify(log.map((t) => [t.focused, t.item]));
			// The pending follow must not re-target the pane away from the click.
			const opened = log.findIndex((t) => t.item === third);
			expect(opened, trace).toBeGreaterThanOrEqual(0);
			expect(log.slice(opened).filter((t) => t.item !== third), trace).toEqual([]);
			await expect(page.locator('.item-card.focused'), trace).toHaveAttribute('data-item-key', third);
		} finally {
			await s.cleanup();
		}
	});
});
