import { expect, type Page } from '@playwright/test';
import { test } from './fixtures';

/**
 * TASK-2979 — the admin console's tab strip used the same hidden-scrollbar
 * scrollport C82 removed from the workspace settings strip: below 640px it
 * scrolled with `scrollbar-width: none`, so the row ended after a tab with
 * clean trailing whitespace and the tabs past the fold were unadvertised
 * rather than merely awkward.
 *
 * Measured before the fix (trail): 4 tabs (self-host) clip `Settings` to 61%
 * at 320px, and with the two `cloudMode` tabs present the strip is 478px
 * intrinsic against a 288-398px box — `Settings` 0% visible at every phone
 * width from 320 to 430.
 *
 * THE VIEWPORT IS 320, NOT THE PROJECT DEFAULT. Self-host renders four tabs,
 * which fit from 360px up, so a leg at the mobile project's 412px would pass
 * on the broken build. The width is chosen to be the one where THIS tab set
 * genuinely overflows, and the precondition below fails loudly if that ever
 * stops being true (a renamed or removed tab would do it).
 */

const NARROW = { width: 320, height: 844 };

async function probeAdminTabs(page: Page) {
	return page.evaluate(() => {
		const bar = document.querySelector('.admin-tabs') as HTMLElement;
		const barBox = bar.getBoundingClientRect();
		const gap = parseFloat(getComputedStyle(bar).columnGap || '0') || 0;
		const tabs = [...bar.querySelectorAll('.admin-tab')].map((t) => {
			const r = t.getBoundingClientRect();
			const visible = Math.max(0, Math.min(r.right, barBox.right) - Math.max(r.left, barBox.left));
			return { label: (t.textContent ?? '').trim(), y: Math.round(r.y), width: r.width, pct: (100 * visible) / r.width };
		});
		const de = document.scrollingElement as HTMLElement;
		return {
			count: tabs.length,
			rows: new Set(tabs.map((t) => t.y)).size,
			clientWidth: bar.clientWidth,
			intrinsicWidth: tabs.reduce((sum, t) => sum + t.width, 0) + gap * Math.max(0, tabs.length - 1),
			barScrolls: bar.scrollWidth > bar.clientWidth,
			pageScrollsHorizontally: de.scrollWidth > de.clientWidth,
			clipped: tabs.filter((t) => t.pct < 99.5).map((t) => `${t.label} ${t.pct.toFixed(1)}%`),
		};
	});
}

test('TASK-2979: no admin console tab is clipped at 320px', async ({ browser, fixture }) => {
	const context = await browser.newContext({ viewport: NARROW });
	await context.setExtraHTTPHeaders({ Authorization: `Bearer ${fixture.apiToken}` });
	const page = await context.newPage();
	try {
		await page.goto(`${fixture.baseURL}/console/admin`);
		await expect(page.locator('.admin-tabs .admin-tab').first()).toBeVisible();

		const bar = await probeAdminTabs(page);

		// Non-vacuous: at this width the tabs genuinely cannot fit on one row.
		// If a tab is renamed or dropped and they start fitting, this fails and
		// the leg gets re-chosen rather than passing for the wrong reason.
		expect(bar.intrinsicWidth).toBeGreaterThan(bar.clientWidth);

		expect(bar.clipped, 'admin console tabs clipped out of view').toEqual([]);
		expect(bar.barScrolls, 'admin tab strip still scrolls horizontally').toBe(false);
		expect(bar.pageScrollsHorizontally).toBe(false);
		expect(bar.rows).toBeGreaterThan(1);
	} finally {
		await context.close();
	}
});

test('TASK-2979: the wrap rule is inert on desktop', async ({ page, fixture }, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'this is the desktop control leg');

	await page.goto(`/console/admin`);
	await expect(page.locator('.admin-tabs .admin-tab').first()).toBeVisible();

	const bar = await probeAdminTabs(page);

	// Non-vacuous in the other direction: the tabs fit here, so one row is a
	// real claim rather than an accident of width.
	expect(bar.intrinsicWidth).toBeLessThanOrEqual(bar.clientWidth);
	expect(bar.rows, 'desktop admin tabs wrapped when they did not need to').toBe(1);
	expect(bar.clipped).toEqual([]);
});
