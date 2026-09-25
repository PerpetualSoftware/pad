import { test, expect } from './fixtures';
import type { Page, APIRequestContext } from '@playwright/test';
import { browserLogin } from './lib/collab-helpers';
import { createWorkspace, deleteCollection, deleteWorkspace } from './lib/attachment-viewer';

/**
 * BUG-3164 — the collection table's header row did not stick.
 *
 * `.table-row.table-header` is `position: sticky; top: 0`, but its nearest
 * scroll container is `.table-scroll`, whose `overflow-x: auto` (sideways
 * scrolling for wide tables) computes to auto in BOTH axes. That box never
 * scrolled vertically — the page did, in `.main-content` (or `.list-column`
 * with the pane open) — so `top: 0` never engaged and the header scrolled away
 * with the rows.
 *
 * Ruled option A: in table view the table fills the visible region and owns
 * both scroll axes, like the board.
 *
 * The legs find the element that ACTUALLY scrolls the rows vertically by
 * walking up from the header, rather than naming it, so the same spec runs on
 * unfixed main (where that element is the page) and on the fix (the table).
 */

const ROWS = 60;

async function setup(
	page: Page,
	fixture: import('./fixtures').SuiteFixture,
	request: APIRequestContext,
	opts: { wide?: boolean; width?: number; ws?: string } = {},
) {
	const ws = opts.ws ?? fixture.workspaceSlug;
	await page.setViewportSize({ width: opts.width ?? 1280, height: 800 });
	await browserLogin(page);
	const headers = { Authorization: `Bearer ${fixture.apiToken}`, 'Content-Type': 'application/json' };
	const stamp = Date.now();
	const fields = opts.wide
		? Array.from({ length: 20 }, (_, i) => ({ key: `f${i}`, label: `Field ${i}`, type: 'text' }))
		: [];
	const res = await request.post(`/api/v1/workspaces/${ws}/collections`, {
		headers,
		data: { name: `B3164 ${stamp}`, prefix: `BT${String(stamp).slice(-4)}`, schema: JSON.stringify({ fields }) },
	});
	expect(res.ok(), await res.text()).toBeTruthy();
	const coll = await res.json();
	for (let i = 0; i < ROWS; i++) {
		const r = await request.post(`/api/v1/workspaces/${ws}/collections/${coll.slug}/items`, {
			headers,
			data: { title: `B3164 row ${String(i).padStart(2, '0')}` },
		});
		expect(r.ok(), await r.text()).toBeTruthy();
	}
	const url = `/${fixture.adminUsername}/${ws}/${coll.slug}`;
	await page.goto(`${url}?view=list`);
	await expect(page.locator('.item-card').nth(ROWS - 1)).toBeAttached({ timeout: 15_000 });
	// ?view= accepts list|board only; the table is reached through the view menu.
	await page.getByRole('button', { name: 'Change view' }).click();
	await page.getByRole('menuitemradio', { name: /Table/ }).or(page.getByRole('menuitem', { name: /Table/ })).first().click();
	await expect(page.locator('.table-row .title-link').nth(ROWS - 1)).toBeAttached({ timeout: 10_000 });
	return { coll };
}

/**
 * Scroll the header's nearest VERTICALLY-overflowing ancestor by `by` pixels
 * and report where the header ended up relative to that scroller.
 */
async function scrollRowsAndMeasure(page: Page, by: number) {
	return page.evaluate((dy) => {
		const header = document.querySelector<HTMLElement>('.table-row.table-header')!;
		let el: HTMLElement | null = header.parentElement;
		while (el) {
			const oy = getComputedStyle(el).overflowY;
			if ((oy === 'auto' || oy === 'scroll') && el.scrollHeight > el.clientHeight + 1) break;
			el = el.parentElement;
		}
		const scroller = el!;
		scroller.scrollTop = scroller.scrollTop + dy;
		return {
			scroller: `${scroller.tagName.toLowerCase()}.${[...scroller.classList].join('.')}`,
			scrollTop: Math.round(scroller.scrollTop),
			scrollerTop: Math.round(scroller.getBoundingClientRect().top),
			headerTop: Math.round(header.getBoundingClientRect().top),
		};
	}, by);
}

test.describe('BUG-3164: the table header stays at the top of the table while it scrolls', () => {
	test.beforeEach(({}, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'one project is enough; the layout is asserted directly');
		test.setTimeout(120_000);
	});

	test('no pane: the header sticks at the scroller top after a vertical scroll', async ({ page, fixture, request }) => {
		const { coll } = await setup(page, fixture, request);
		try {
			const m = await scrollRowsAndMeasure(page, 600);
			expect(m.scrollTop, `the rows did not scroll (${m.scroller})`).toBeGreaterThan(300);
			expect(Math.abs(m.headerTop - m.scrollerTop), `header top ${m.headerTop} vs ${m.scroller} top ${m.scrollerTop}`).toBeLessThanOrEqual(2);
		} finally {
			await deleteCollection(fixture, request, coll.slug);
		}
	});

	test('pane open: the header sticks at the scroller top after a vertical scroll', async ({ page, fixture, request }) => {
		const { coll } = await setup(page, fixture, request);
		try {
			await page.locator('.table-row .title-link').first().click();
			await expect(page.locator('.item-pane')).toBeVisible({ timeout: 10_000 });
			await page.waitForTimeout(600);
			const m = await scrollRowsAndMeasure(page, 600);
			expect(m.scrollTop, `the rows did not scroll (${m.scroller})`).toBeGreaterThan(300);
			expect(Math.abs(m.headerTop - m.scrollerTop), `header top ${m.headerTop} vs ${m.scroller} top ${m.scrollerTop}`).toBeLessThanOrEqual(2);
		} finally {
			await deleteCollection(fixture, request, coll.slug);
		}
	});

	test('a wide table still scrolls sideways, the header moves with the body, and still sticks', async ({ page, fixture, request }) => {
		const { coll } = await setup(page, fixture, request, { wide: true });
		try {
			const before = await page.evaluate(() => {
				const scroll = document.querySelector<HTMLElement>('.table-scroll')!;
				const head = document.querySelector<HTMLElement>('.table-row.table-header .table-cell:last-child')!;
				const body = document.querySelector<HTMLElement>('.table-row:not(.table-header) .table-cell:last-child')!;
				return {
					overflow: scroll.scrollWidth - scroll.clientWidth,
					head: Math.round(head.getBoundingClientRect().left),
					body: Math.round(body.getBoundingClientRect().left),
				};
			});
			expect(before.overflow, 'the fixture must be wider than the table').toBeGreaterThan(300);
			expect(before.head).toBe(before.body);

			const after = await page.evaluate(() => {
				const scroll = document.querySelector<HTMLElement>('.table-scroll')!;
				scroll.scrollLeft = 250;
				const head = document.querySelector<HTMLElement>('.table-row.table-header .table-cell:last-child')!;
				const body = document.querySelector<HTMLElement>('.table-row:not(.table-header) .table-cell:last-child')!;
				return {
					scrollLeft: Math.round(scroll.scrollLeft),
					head: Math.round(head.getBoundingClientRect().left),
					body: Math.round(body.getBoundingClientRect().left),
				};
			});
			expect(after.scrollLeft, 'horizontal scroll must still work').toBe(250);
			expect(after.head, 'the header moves sideways with the body').toBe(after.body);
			expect(after.head).toBe(before.head - 250);

			const m = await scrollRowsAndMeasure(page, 600);
			expect(m.scrollTop, `the rows did not scroll (${m.scroller})`).toBeGreaterThan(300);
			expect(Math.abs(m.headerTop - m.scrollerTop), `header top ${m.headerTop} vs ${m.scroller} top ${m.scrollerTop}`).toBeLessThanOrEqual(2);
		} finally {
			await deleteCollection(fixture, request, coll.slug);
		}
	});
	test('keyboard navigation keeps the focused row visible, clear of the sticky header', async ({ page, fixture, request }) => {
		// Its OWN workspace: any change to the page's filtered item list resets
		// the keyboard cursor, and other specs write to the shared suite
		// workspace concurrently, which reset it mid-walk under full-suite load.
		const { slug: ws } = await createWorkspace(fixture, request, 'B3164 keys');
		await setup(page, fixture, request, { ws });
		try {
			// Where the focused row sits relative to the visible band of the table
			// (below the header, above the scroller's bottom edge).
			const focusedBand = () =>
				page.evaluate(() => {
					const row = document.querySelector<HTMLElement>('.table-row.focused');
					const table = document.querySelector<HTMLElement>('.table-scroll')!;
					const head = document.querySelector<HTMLElement>('.table-row.table-header')!.getBoundingClientRect();
					const box = table.getBoundingClientRect();
					if (!row) return null;
					const r = row.getBoundingClientRect();
					return {
						title: row.querySelector('.title-link')?.textContent?.trim(),
						belowHeader: Math.round(r.top - head.bottom),
						aboveBottom: Math.round(box.top + table.clientTop + table.clientHeight - r.bottom),
					};
				});
			// The list column is the page's tabindex=-1 focus landmark; a click at an
			// arbitrary point can land on a sidebar control and swallow the keys.
			await page.locator('.list-column').focus();
			// Step with j until focus is well below the first screenful (about 17
			// rows fit), pausing so each press is handled before the next. The
			// POSITION in the table is what counts, not the title: rows are listed
			// newest first, so the first focused row is titled "row 59".
			const position = () =>
				page.evaluate(() =>
					[...document.querySelectorAll('.table-row:not(.table-header)')].findIndex((r) => r.classList.contains('focused')),
				);
			for (let i = 0; i < 80 && (await position()) < 30; i++) {
				await page.keyboard.press('j');
				await page.waitForTimeout(40);
			}
			expect(await position(), 'j should reach a row far below the first screenful').toBeGreaterThanOrEqual(30);
			// NO_ROW can never satisfy the bound, so a lost focus fails the poll
			// instead of passing it (round 2 of the review caught a -1 sentinel
			// that met its own `>= -1`).
			const NO_ROW = -1e9;
			await expect.poll(async () => (await focusedBand())?.aboveBottom ?? NO_ROW, { message: 'j moved the focus below the visible table' })
				.toBeGreaterThanOrEqual(-1);

			const before = await position();
			for (let i = 0; i < 20; i++) {
				await page.keyboard.press('k');
				await page.waitForTimeout(40);
			}
			expect(await position(), 'k should move the focus back up').toBeLessThan(before - 10);
			await expect.poll(async () => (await focusedBand())?.belowHeader ?? NO_ROW, { message: 'k left the focused row under the sticky header' })
				.toBeGreaterThanOrEqual(-1);
		} finally {
			await deleteWorkspace(fixture, request, ws);
		}
	});
});
