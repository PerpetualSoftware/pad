import { test, expect } from './fixtures';
import type { Page, APIRequestContext } from '@playwright/test';
import { browserLogin } from './lib/collab-helpers';
import { deleteCollection } from './lib/attachment-viewer';

/**
 * BUG-3165 — the collection list lost its position across the split pane.
 *
 * The list's scroll container changes with the pane: `.main-content` with no
 * pane, `.list-column` with `?item=` open. Nothing carried the position across,
 * so opening an item from a scrolled list dropped the list to its top (the
 * clicked row off screen), Expand to full page + Back came back to that, and
 * closing after scrolling the column jumped the list somewhere else.
 */

const ROWS = 80;

async function setup(page: Page, fixture: import('./fixtures').SuiteFixture, request: APIRequestContext, view: 'list' | 'table', titleOf: (i: number) => string = (i) => `B3165 row ${String(i).padStart(2, '0')}`) {
	await page.setViewportSize({ width: 1400, height: 800 });
	await browserLogin(page);
	const headers = { Authorization: `Bearer ${fixture.apiToken}`, 'Content-Type': 'application/json' };
	const stamp = Date.now();
	const res = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/collections`, {
		headers,
		data: { name: `B3165 ${stamp}`, prefix: `BL${String(stamp).slice(-4)}` },
	});
	expect(res.ok(), await res.text()).toBeTruthy();
	const coll = await res.json();
	for (let i = 0; i < ROWS; i++) {
		const r = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/collections/${coll.slug}/items`, {
			headers, data: { title: titleOf(i) },
		});
		expect(r.ok(), await r.text()).toBeTruthy();
	}
	const url = `/${fixture.adminUsername}/${fixture.workspaceSlug}/${coll.slug}`;
	await page.goto(`${url}?view=list`);
	await expect(page.locator('.item-card').nth(ROWS - 1)).toBeAttached({ timeout: 15_000 });
	if (view === 'table') {
		// ?view= accepts list|board only; the table is reached through the view menu.
		await page.getByRole('button', { name: 'Change view' }).click();
		await page.getByRole('menuitemradio', { name: /Table/ }).or(page.getByRole('menuitem', { name: /Table/ })).first().click();
		await expect(page.locator('.table-row .title-link').nth(ROWS - 1)).toBeAttached({ timeout: 10_000 });
	}
	return { coll, url };
}

// Rows are found by their title link / card title, NOT by `data-item-key` —
// the fix adds that attribute, and the counterfactual run on unfixed main has
// to find the same rows.
const ROW_SEL = '.item-card, .table-row:not(.table-header)';
const TITLE_SEL = '.card-title, .title-link';

/** Viewport top of the row with this title. */
function rowTop(page: Page, title: string) {
	return page.evaluate(([rowSel, titleSel, t]) => {
		const row = Array.from(document.querySelectorAll<HTMLElement>(rowSel))
			.find((el) => el.querySelector(titleSel)?.textContent?.trim() === t);
		return row ? Math.round(row.getBoundingClientRect().top) : null;
	}, [ROW_SEL, TITLE_SEL, title] as const);
}

/** Rendered height of the row with this title. */
function rowHeight(page: Page, title: string) {
	return page.evaluate(([rowSel, titleSel, t]) => {
		const row = Array.from(document.querySelectorAll<HTMLElement>(rowSel))
			.find((el) => el.querySelector(titleSel)?.textContent?.trim() === t);
		return row ? Math.round(row.getBoundingClientRect().height) : 0;
	}, [ROW_SEL, TITLE_SEL, title] as const);
}

/** The first row showing at the top of the list's current scroller, and its viewport top. */
function topRow(page: Page) {
	return page.evaluate(([rowSel, titleSel]) => {
		const col = document.querySelector('.collection-page.pane-open .list-column') ?? document.querySelector('.main-content')!;
		const top = col.getBoundingClientRect().top;
		const row = Array.from(document.querySelectorAll<HTMLElement>(rowSel))
			.find((el) => el.getBoundingClientRect().bottom > top + 1)!;
		return { title: row.querySelector(titleSel)!.textContent!.trim(), top: Math.round(row.getBoundingClientRect().top) };
	}, [ROW_SEL, TITLE_SEL] as const);
}

/** Scroll the page to 600 and pick a row showing mid-screen (clicking it needs no auto-scroll). */
async function scrollAndPick(page: Page) {
	await page.locator('.main-content').evaluate((el) => el.scrollTo({ top: 600 }));
	await page.waitForTimeout(300);
	const title = await page.evaluate(([rowSel, titleSel]) => {
		const row = Array.from(document.querySelectorAll<HTMLElement>(rowSel))
			.find((el) => { const t = el.getBoundingClientRect().top; return t > 300 && t < 500; })!;
		return row.querySelector(titleSel)!.textContent!.trim();
	}, [ROW_SEL, TITLE_SEL] as const);
	const before = (await rowTop(page, title))!;
	return { title, before };
}

async function openRow(page: Page, title: string) {
	await page.locator(TITLE_SEL).filter({ hasText: title }).first().click();
	await expect(page.locator('.item-pane')).toBeVisible({ timeout: 10_000 });
	await page.waitForTimeout(600);
}

async function scrollColumnAndClose(page: Page) {
	await page.locator('.list-column').evaluate((el) => el.scrollTo({ top: 1500 }));
	await page.waitForTimeout(300);
	const before = await topRow(page);
	await page.keyboard.press('Escape');
	await page.waitForURL((u) => !u.search.includes('item='), { timeout: 10_000 });
	await page.waitForTimeout(2500); // past the restore helper's 2s loop
	return before;
}

test.describe('BUG-3165: the list keeps its position across the split pane', () => {
	test.beforeEach(({}, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'the split pane is a desktop layout');
		test.setTimeout(120_000);
	});

	test('opening an item keeps its row in place; Expand to full page + Back returns to it', async ({ page, fixture, request }) => {
		const { coll } = await setup(page, fixture, request, 'list');
		try {
			const { title, before } = await scrollAndPick(page);
			await openRow(page, title);
			const opened = await rowTop(page, title);
			expect(opened, 'opening the pane moved the clicked row').toBeGreaterThan(before - 40);
			expect(opened).toBeLessThan(before + 40);

			await page.locator('.item-pane button.pane-header-btn[aria-label="Expand to full page"]').click();
			await page.waitForURL((u) => !u.search.includes('item='), { timeout: 10_000 });
			await expect(page.locator('.item-pane')).toHaveCount(0);
			await page.goBack();
			await expect(page.locator('.item-pane')).toBeVisible({ timeout: 10_000 });
			await expect.poll(() => rowTop(page, title), { timeout: 4000, message: 'Back from the full page lost the list position' })
				.toBeGreaterThan(before - 40);
			expect(await rowTop(page, title)).toBeLessThan(before + 40);
		} finally {
			await deleteCollection(fixture, request, coll.slug);
		}
	});

	test('rows that reflow taller in the column: the CLICKED row stays put, not merely the top one', async ({ page, fixture, request }) => {
		// Long titles wrap onto more lines in the narrower column than at full
		// width, so every row above the clicked one grows: holding the TOP row in
		// place would push the clicked row down by that growth. 250 characters
		// (titles cap at 255), so the line count differs whatever the font's
		// glyph width: a ~130 character title wrapped to two lines in BOTH
		// layouts on CI's fonts.
		const { coll } = await setup(page, fixture, request, 'list', (i) =>
			`B3165 row ${String(i).padStart(2, '0')} ${'with a long title that wraps once the pane narrows the list '.repeat(5)}`.slice(0, 250).trim());
		try {
			const { title, before } = await scrollAndPick(page);
			const heightBefore = await rowHeight(page, title);
			await openRow(page, title);
			// Precondition: the rows really grew, or this leg cannot tell the two
			// anchors apart.
			expect(await rowHeight(page, title), 'precondition: rows reflow taller in the column').toBeGreaterThan(heightBefore + 10);
			const opened = await rowTop(page, title);
			expect(opened, 'opening the pane moved the clicked row').toBeGreaterThan(before - 40);
			expect(opened).toBeLessThan(before + 40);
		} finally {
			await deleteCollection(fixture, request, coll.slug);
		}
	});

	test('closing the pane keeps the row the column was scrolled to (pane opened from the list)', async ({ page, fixture, request }) => {
		const { coll } = await setup(page, fixture, request, 'list');
		try {
			const { title } = await scrollAndPick(page);
			await openRow(page, title);
			const before = await scrollColumnAndClose(page);
			const after = await topRow(page);
			expect(after.title, 'closing jumped the list to a different row').toBe(before.title);
			expect(Math.abs(after.top - before.top)).toBeLessThan(40);
		} finally {
			await deleteCollection(fixture, request, coll.slug);
		}
	});

	test('closing a cold-loaded pane keeps the row the column was scrolled to', async ({ page, fixture, request }) => {
		const { coll, url } = await setup(page, fixture, request, 'list');
		try {
			const href = await page.locator('.item-card').first().getAttribute('href');
			const ref = href!.split('/').pop()!;
			// A fresh load of a `?item=` URL: the pane does not own the entry, so
			// close drops `?item=` in place rather than going back.
			await page.goto(`${url}?view=list&item=${ref}`);
			await expect(page.locator('.item-pane')).toBeVisible({ timeout: 10_000 });
			await expect(page.locator('.item-card').nth(ROWS - 1)).toBeAttached({ timeout: 15_000 });
			await page.waitForTimeout(600);
			const before = await scrollColumnAndClose(page);
			const after = await topRow(page);
			expect(after.title, 'closing jumped the list to a different row').toBe(before.title);
			expect(Math.abs(after.top - before.top)).toBeLessThan(40);
		} finally {
			await deleteCollection(fixture, request, coll.slug);
		}
	});

	test('Back to the pre-pane entry while the page is still LOADING restores its saved position (codex r1)', async ({ page, fixture, request }) => {
		// No handoff can run while the list is loading (there is no row to
		// anchor on), so the entry's saved offset is the only position there is:
		// the popstate must not skip it.
		const { coll } = await setup(page, fixture, request, 'list');
		try {
			const { title } = await scrollAndPick(page);
			await openRow(page, title);
			// Hold the collection's metadata read across a reload of the pane
			// entry, so the page is still in its initial load when Back lands.
			let release!: () => void;
			const gate = new Promise<void>((r) => (release = r));
			const collPath = `/collections/${coll.slug}`;
			await page.route((u) => u.pathname.endsWith(collPath), async (route) => {
				await gate;
				await route.continue();
			});
			await page.reload();
			await expect(page.locator('.list-column > .loading')).toBeVisible({ timeout: 10_000 });
			await page.goBack();
			await page.waitForURL((u) => !u.search.includes('item='), { timeout: 10_000 });
			await page.waitForTimeout(300);
			const loadingAtBack = await page.locator('.list-column > .loading').count();
			release();
			await expect(page.locator('.item-card').nth(ROWS - 1)).toBeAttached({ timeout: 15_000 });
			await page.waitForTimeout(2500);
			expect(loadingAtBack, 'precondition: the page was loading when Back landed').toBe(1);
			const y = await page.locator('.main-content').evaluate((el) => el.scrollTop);
			expect(Math.abs(y - 600), `the saved position was dropped (scrollTop ${y})`).toBeLessThan(40);
		} finally {
			await deleteCollection(fixture, request, coll.slug);
		}
	});

	test('table view: opening an item keeps its row in place', async ({ page, fixture, request }) => {
		const { coll } = await setup(page, fixture, request, 'table');
		try {
			const { title, before } = await scrollAndPick(page);
			await openRow(page, title);
			const opened = await rowTop(page, title);
			expect(opened, 'opening the pane moved the clicked row').toBeGreaterThan(before - 40);
			expect(opened).toBeLessThan(before + 40);
		} finally {
			await deleteCollection(fixture, request, coll.slug);
		}
	});
});
