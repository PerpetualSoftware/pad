import { expect, type Page } from '@playwright/test';
import { test } from './fixtures';

/**
 * At 320px the admin segmented control and Settings tabs must occupy multiple
 * rows with every item visible, without scrolling either row or the page.
 * The desktop control checks that the admin items still fit on one row.
 */

const NARROW = { width: 320, height: 844 };

async function probeTabs(page: Page, selector: string) {
	return page.locator(selector).evaluate((element) => {
		const bar = element as HTMLElement;
		const barBox = bar.getBoundingClientRect();
		const gap = parseFloat(getComputedStyle(bar).columnGap || '0') || 0;
		const tabs = [...bar.querySelectorAll<HTMLElement>('a')].map((t) => {
			const r = t.getBoundingClientRect();
			const visible = Math.max(0, Math.min(r.right, barBox.right) - Math.max(r.left, barBox.left));
			return { label: (t.textContent ?? '').trim(), y: Math.round(r.y), width: r.width, contentClipped: t.scrollWidth > t.clientWidth, pct: (100 * visible) / r.width };
		});
		const de = document.scrollingElement as HTMLElement;
		return {
			count: tabs.length,
			rows: new Set(tabs.map((t) => t.y)).size,
			clientWidth: bar.clientWidth,
			intrinsicWidth: tabs.reduce((sum, t) => sum + t.width, 0) + gap * Math.max(0, tabs.length - 1),
			barScrolls: bar.scrollWidth > bar.clientWidth,
			pageScrollsHorizontally: de.scrollWidth > de.clientWidth,
			clipped: tabs.filter((t) => t.pct < 99.5 || t.contentClipped).map((t) => `${t.label} ${t.pct.toFixed(1)}%`),
		};
	});
}

test('TASK-2979: admin segments and Settings tabs wrap without clipping at 320px', async ({ browser, fixture }) => {
	const context = await browser.newContext({ viewport: NARROW });
	// The admin console is platform administration: the admin's session,
	// not its API token (BUG-3361).
	await context.setExtraHTTPHeaders({ Authorization: `Bearer ${fixture.adminSessionToken}` });
	const page = await context.newPage();
	try {
		await page.goto(`${fixture.baseURL}/settings/admin`);
		await expect(page.locator('.admin-segments .admin-segment').first()).toBeVisible();

		for (const selector of ['.admin-segments', '.settings-tabs']) {
			const bar = await probeTabs(page, selector);
			// Non-vacuous: these items cannot fit on one row at this width.
			expect(bar.intrinsicWidth, selector).toBeGreaterThan(bar.clientWidth);
			expect(bar.clipped, `${selector} items clipped out of view`).toEqual([]);
			expect(bar.barScrolls, `${selector} scrolls horizontally`).toBe(false);
			expect(bar.pageScrollsHorizontally, selector).toBe(false);
			expect(bar.rows, selector).toBeGreaterThan(1);
		}
	} finally {
		await context.close();
	}
});

test('TASK-2979: the wrap rule is inert on desktop', async ({ page, fixture }, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'this is the desktop control leg');

	await page.context().setExtraHTTPHeaders({ Authorization: `Bearer ${fixture.adminSessionToken}` });
	await page.goto(`/settings/admin`);
	await expect(page.locator('.admin-segments .admin-segment').first()).toBeVisible();

	const bar = await probeTabs(page, '.admin-segments');

	// Non-vacuous in the other direction: the tabs fit here, so one row is a
	// real claim rather than an accident of width.
	expect(bar.intrinsicWidth).toBeLessThanOrEqual(bar.clientWidth);
	expect(bar.rows, 'desktop admin tabs wrapped when they did not need to').toBe(1);
	expect(bar.clipped).toEqual([]);
});
