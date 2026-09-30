import { test, expect } from './fixtures';
import { browserLogin } from './lib/collab-helpers';
import type { Locator, Page } from '@playwright/test';

/**
 * TASK-2244 — touch targets at phone width (Dave's ruling, day 83: options 1+3).
 *
 * The collection control strip grows VISIBLY to 44x44 at <=768px, so its proof is
 * the element box (boundingBox). Desktop is unchanged, pinned to the sizes
 * measured on the base commit, so a mobile rule that leaks past its media query
 * goes red here rather than in a screenshot nobody compares.
 */

const MIN = 44;

function strip(page: Page) {
	return page.locator('.header-actions');
}

/** Every interactive control the strip renders, by the name a user would see. */
function stripControls(page: Page, isMobile: boolean): Array<[string, Locator]> {
	const s = strip(page);
	return [
		[
			'view',
			isMobile ? s.locator('button.view-chip') : s.locator('button.view-dd-trigger'),
		],
		['sort', s.getByRole('button', { name: 'Sort items' })],
		['filters', s.getByRole('button', { name: 'Toggle filters' })],
		['quick actions', s.locator('.quick-actions-menu > .trigger-btn')],
		['collection menu', s.getByRole('button', { name: 'Collection menu' })],
		['new', s.locator('button.new-btn')],
	];
}

async function sizes(page: Page, isMobile: boolean) {
	const out: Record<string, { w: number; h: number }> = {};
	for (const [name, loc] of stripControls(page, isMobile)) {
		await expect(loc, `${name} control is rendered`).toHaveCount(1);
		const box = await loc.boundingBox();
		expect(box, `${name} has a box`).not.toBeNull();
		out[name] = { w: Math.round(box!.width * 10) / 10, h: Math.round(box!.height * 10) / 10 };
	}
	return out;
}

for (const view of ['list', 'board'] as const) {
	test(`TASK-2244: every strip control is at least 44x44 on mobile (${view})`, async ({
		page,
		fixture,
	}, testInfo) => {
		test.skip(testInfo.project.name !== 'mobile-chromium', 'the ruling is <=768px');

		await browserLogin(page);
		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/tasks?view=${view}`);
		await expect(strip(page)).toBeVisible();

		const measured = await sizes(page, true);
		for (const [name, { w, h }] of Object.entries(measured)) {
			expect.soft(w, `${name} width (${w}x${h})`).toBeGreaterThanOrEqual(MIN);
			expect.soft(h, `${name} height (${w}x${h})`).toBeGreaterThanOrEqual(MIN);
		}
	});
}

/**
 * The strip stays ONE row where it fits (the lead's ruling on the 360 checkpoint,
 * option B: the chip drops its visible "View:" prefix). A future control or a
 * longer label would otherwise re-wrap it silently, since every size assertion
 * above still passes on two rows. At 390 the realtime dot must share the row as
 * well. List at 360 is exempt by the same ruling: its wider side padding wraps
 * the dot alone, and the dot is a status, not a control.
 */
const ONE_ROW: Array<{ width: number; view: 'list' | 'board'; withDot: boolean }> = [
	{ width: 390, view: 'list', withDot: true },
	{ width: 390, view: 'board', withDot: true },
	{ width: 360, view: 'board', withDot: true },
];

for (const { width, view, withDot } of ONE_ROW) {
	test(`TASK-2244: the strip is one row at ${width}px (${view})`, async ({ page, fixture }, testInfo) => {
		test.skip(testInfo.project.name !== 'mobile-chromium', 'phone widths');

		await page.setViewportSize({ width, height: 844 });
		await browserLogin(page);
		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/tasks?view=${view}`);
		await expect(strip(page)).toBeVisible();

		const boxes = [];
		for (const [name, loc] of stripControls(page, true)) {
			const box = await loc.boundingBox();
			expect(box, `${name} has a box`).not.toBeNull();
			boxes.push({ name, top: Math.round(box!.y) });
		}
		const rowTop = boxes[0].top;
		for (const b of boxes) expect(b.top, `${b.name} is on the first row`).toBe(rowTop);

		if (withDot) {
			const dot = await strip(page).locator('.sse-mobile').boundingBox();
			expect(dot, 'the realtime dot has a box').not.toBeNull();
			expect(dot!.y, 'the realtime dot shares the row').toBeLessThan(rowTop + MIN);
		}
	});
}

/**
 * Desktop sizes on the base commit a90f75fd (desktop-chromium, 1280x720), measured
 * by this file's `sizes()` before any change. The view control differs by view
 * because its label does ("List" vs "Board").
 */
const DESKTOP_BASE: Record<'list' | 'board', Record<string, { w: number; h: number }>> = {
	list: {
		view: { w: 75, h: 28 },
		sort: { w: 30, h: 28 },
		filters: { w: 30, h: 28 },
		'quick actions': { w: 41, h: 28 },
		'collection menu': { w: 30, h: 28 },
		new: { w: 93.8, h: 28 },
	},
	board: {
		view: { w: 88.4, h: 28 },
		sort: { w: 30, h: 28 },
		filters: { w: 30, h: 28 },
		'quick actions': { w: 41, h: 28 },
		'collection menu': { w: 30, h: 28 },
		new: { w: 93.8, h: 28 },
	},
};

for (const view of ['list', 'board'] as const) {
	test(`TASK-2244: desktop strip sizes are unchanged (${view})`, async ({ page, fixture }, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'the desktop leg');

		await browserLogin(page);
		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/tasks?view=${view}`);
		await expect(strip(page)).toBeVisible();

		expect(await sizes(page, false)).toEqual(DESKTOP_BASE[view]);
	});
}
