import { test, expect } from './fixtures';
import { browserLogin } from './lib/collab-helpers';
import { deleteCollection } from './lib/attachment-viewer';
import type { Page } from '@playwright/test';

/**
 * BUG-2837 — in the collection TABLE view, a long field label ran into the next
 * column's label ("Next Batching / Ranking" over "Cost Share" in Dave's
 * screenshot). Field columns are `minmax(90px, 0.55fr)`, and header labels were
 * `white-space: nowrap` with nothing clipping them, so a label wider than its
 * column painted over its neighbour. Labels now wrap to two lines, then
 * ellipsis, clipped to their column, with the full label as a title.
 *
 * Measured, not eyeballed: each header label's PAINTED right edge (its text,
 * clipped by any overflow-hiding ancestor) against the next cell's left edge.
 * Dave's six labels are the fixture.
 */
const LABELS = [
	'Sign-up Deadline',
	'Next Batching / Ranking',
	'Cost Share',
	'Rates Last Verified',
	'Agency Contact',
	'Contract Term',
];

async function overlaps(page: Page): Promise<string[]> {
	return page.evaluate(() => {
		const cells = [...document.querySelectorAll<HTMLElement>('.table-header [role="columnheader"]')];
		const out: string[] = [];
		for (let i = 0; i + 1 < cells.length; i++) {
			const label =
				cells[i].querySelector<HTMLElement>('.header-label') ??
				cells[i].querySelector<HTMLElement>('.sort-btn') ??
				cells[i];
			const range = document.createRange();
			range.selectNodeContents(label);
			let right = range.getBoundingClientRect().right;
			for (let el: HTMLElement | null = label; el && el !== cells[i].parentElement; el = el.parentElement) {
				if (getComputedStyle(el).overflowX !== 'visible') right = Math.min(right, el.getBoundingClientRect().right);
			}
			const nextLeft = cells[i + 1].getBoundingClientRect().left;
			if (right > nextLeft + 0.5) {
				out.push(`${label.textContent?.trim()} paints ${Math.round(right - nextLeft)}px into ${cells[i + 1].textContent?.trim()}`);
			}
		}
		return out;
	});
}

test('BUG-2837: long table header labels do not overlap, wrap on one baseline, and keep sort usable', async ({
	page,
	fixture,
	request,
}, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'a layout check at desktop widths');
	test.setTimeout(90_000);
	await page.setViewportSize({ width: 1280, height: 800 });
	await browserLogin(page);
	const headers = { Authorization: `Bearer ${fixture.apiToken}`, 'Content-Type': 'application/json' };
	const stamp = Date.now();
	const schema = JSON.stringify({ fields: LABELS.map((label, i) => ({ key: `f${i}`, label, type: 'text' })) });
	const res = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/collections`, {
		headers,
		data: { name: `B2837 ${stamp}`, prefix: `BG${String(stamp).slice(-4)}`, schema },
	});
	expect(res.ok(), await res.text()).toBeTruthy();
	const coll = await res.json();
	try {
		for (let r = 0; r < 3; r++) {
			const fields = Object.fromEntries(LABELS.map((_, i) => [`f${i}`, `v${r}-${i}`]));
			const item = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/collections/${coll.slug}/items`, {
				headers,
				data: { title: `B2837 row ${String(r).padStart(2, '0')}`, fields: JSON.stringify(fields) },
			});
			expect(item.ok(), await item.text()).toBeTruthy();
		}

		// ?view= accepts list|board only; the table is reached through the view menu.
		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/${coll.slug}`);
		await page.getByRole('button', { name: 'Change view' }).click();
		await page.getByRole('menuitemradio', { name: /Table/ }).or(page.getByRole('menuitem', { name: /Table/ })).first().click();
		const headerCells = page.locator('.table-header [role="columnheader"]');
		const longCell = headerCells.filter({ hasText: LABELS[1] }).first();
		await expect(longCell).toBeVisible();

		// (c) at 1280px.
		expect(await overlaps(page), 'header labels overlap at 1280px').toEqual([]);

		// The full label stays available while the cell shows a clamped one.
		await expect(longCell.locator('button')).toHaveAttribute('title', LABELS[1]);

		// (a) one baseline: every field label's bottom edge within 1px of the others.
		const bottoms = await page.evaluate(() =>
			[...document.querySelectorAll<HTMLElement>('.table-header .header-label')].map((l) => l.getBoundingClientRect().bottom),
		);
		expect(bottoms.length, 'precondition: the labels are the wrapped kind').toBeGreaterThan(LABELS.length - 1);
		expect(Math.max(...bottoms) - Math.min(...bottoms), 'one- and two-line labels sit on different baselines').toBeLessThanOrEqual(1);

		// (b) sorting by the WRAPPED label still works and its arrow stays visible inside the cell.
		await longCell.locator('button').click();
		const arrow = longCell.locator('.sort-arrow');
		await expect(arrow).toBeVisible();
		const [a, c] = [await arrow.boundingBox(), await longCell.boundingBox()];
		expect(a!.x + a!.width, 'the sort arrow is clipped out of its cell').toBeLessThanOrEqual(c!.x + c!.width + 0.5);

		// NOT ASSERTED HERE: that the sticky header covers rows scrolled beneath
		// it. Measured on unfixed main AND on this fix: the header does not stick
		// on page scroll at all (its top went to -461px after the page scrolled
		// 600px), because `.table-scroll`'s horizontal overflow makes it the
		// sticky container and it never scrolls vertically. Pre-existing, filed
		// separately, and unchanged by this unit.

		// (c) at the narrowest width: the field columns at their 90px floor.
		await page.setViewportSize({ width: 800, height: 800 });
		await page.waitForTimeout(300);
		const widths = await page.evaluate(() =>
			[...document.querySelectorAll<HTMLElement>('.table-header [role="columnheader"]')]
				.filter((c) => c.querySelector('.header-label') && !c.classList.contains('col-title'))
				.map((c) => Math.round(c.getBoundingClientRect().width)),
		);
		expect(Math.max(...widths), `precondition: field columns at their floor, got ${widths}`).toBeLessThanOrEqual(91);
		expect(await overlaps(page), 'header labels overlap at the narrowest width').toEqual([]);
	} finally {
		await deleteCollection(fixture, request, coll.slug);
	}
});
