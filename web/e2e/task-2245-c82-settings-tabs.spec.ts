import { expect, type Page } from '@playwright/test';
import { test } from './fixtures';

/**
 * TASK-2245 / C82 — the workspace settings tab bar scrolled horizontally with
 * its scrollbar hidden (`overflow-x:auto` + `scrollbar-width:none`), so at
 * phone widths it ended after a tab with clean trailing whitespace and LOOKED
 * complete. Measured at 390x844 on the unfixed build: the five owner tabs are
 * 562px intrinsic in a 342px box, Storage 9.3% visible and Danger Zone 0% —
 * workspace export and deletion reachable only by a swipe nothing advertised.
 *
 * The fix is `flex-wrap: wrap` with no media query. Two legs, because the rule
 * carries two claims:
 *
 *  - mobile: nothing is clipped, and the bar does not scroll.
 *  - desktop: the rule is INERT. `flex-wrap` does nothing while the row fits,
 *    so the desktop bar must still be a single row. This is the leg that fails
 *    if someone "simplifies" the fix into something that wraps unconditionally.
 *
 * Both legs assert a non-vacuity precondition first: a viewport where the tabs
 * happen to fit would pass the mobile leg trivially, and one where they never
 * fit would pass the desktop leg trivially.
 */

type BarProbe = {
	tabCount: number;
	rows: number;
	clientWidth: number;
	/** Intrinsic width of the row: tab widths plus the gaps between them. */
	intrinsicWidth: number;
	barScrolls: boolean;
	/** Ancestors of the bar (up to <html>) that scroll horizontally. */
	scrollingAncestors: string[];
	clipped: string[];
};

async function probeTabBar(page: Page): Promise<BarProbe> {
	return page.evaluate(() => {
		const bar = document.querySelector('.tab-bar') as HTMLElement;
		const barBox = bar.getBoundingClientRect();
		const gap = parseFloat(getComputedStyle(bar).columnGap || '0') || 0;
		const tabs = [...bar.querySelectorAll('.tab')].map((t) => {
			const r = t.getBoundingClientRect();
			// How much of this tab is inside the bar's own box — clipping does
			// not shrink getBoundingClientRect, so the overlap is the oracle.
			const visible = Math.max(0, Math.min(r.right, barBox.right) - Math.max(r.left, barBox.left));
			return {
				label: (t.textContent ?? '').trim(),
				y: Math.round(r.y),
				width: r.width,
				pct: (100 * visible) / r.width,
			};
		});
		// "No horizontal page scroll" cannot be read off document.scrollingElement
		// here: the app scrolls in `.main-content`, whose `overflow-y:auto`
		// computes `overflow-x:auto`, so overflow is contained there and never
		// reaches the document. Walk the real chain instead. Negative control on
		// the trail: forcing a 3000px child into `.settings` makes this list
		// `[div.settings, main.main-content]`, while the document oracle stays
		// silent — so the empty list below is a measurement, not a tautology.
		const scrollingAncestors: string[] = [];
		for (let el = bar.parentElement; el; el = el.parentElement) {
			if (el.scrollWidth > el.clientWidth + 1) {
				scrollingAncestors.push(`${el.tagName.toLowerCase()}.${el.className || '(no class)'}`);
			}
		}
		return {
			tabCount: tabs.length,
			rows: new Set(tabs.map((t) => t.y)).size,
			clientWidth: bar.clientWidth,
			intrinsicWidth: tabs.reduce((sum, t) => sum + t.width, 0) + gap * Math.max(0, tabs.length - 1),
			barScrolls: bar.scrollWidth > bar.clientWidth,
			scrollingAncestors,
			clipped: tabs.filter((t) => t.pct < 99.5).map((t) => `${t.label} ${t.pct.toFixed(1)}%`),
		};
	});
}

async function openSettings(page: Page, username: string, workspace: string) {
	await page.goto(`/${username}/${workspace}/settings`);
	// Danger Zone is owner-only and arrives with /me, so waiting on the fifth
	// tab is what makes the measurement one of the FULL bar.
	await expect(page.locator('.tab-bar .tab')).toHaveCount(5);
}

test('TASK-2245 C82: no settings tab is clipped at phone width', async ({ page, fixture }, testInfo) => {
	test.skip(testInfo.project.name !== 'mobile-chromium', 'the clipping only occurs below ~610px');

	await openSettings(page, fixture.adminUsername, fixture.workspaceSlug);
	const bar = await probeTabBar(page);

	// Non-vacuous: the tabs genuinely cannot fit on one row at this width.
	expect(bar.intrinsicWidth).toBeGreaterThan(bar.clientWidth);

	expect(bar.clipped, 'settings tabs clipped out of view').toEqual([]);
	expect(bar.barScrolls, 'tab bar still scrolls horizontally').toBe(false);
	expect(bar.scrollingAncestors, 'wrapping pushed horizontal scroll onto an ancestor').toEqual([]);
	expect(bar.rows).toBeGreaterThan(1);
});

test('TASK-2245 C82: the wrap rule is inert on desktop', async ({ page, fixture }, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'this is the desktop control leg');

	await openSettings(page, fixture.adminUsername, fixture.workspaceSlug);
	const bar = await probeTabBar(page);

	// Non-vacuous: at this width the tabs fit, so a single row is a real claim.
	expect(bar.intrinsicWidth).toBeLessThanOrEqual(bar.clientWidth);

	expect(bar.rows, 'desktop tab bar wrapped when it did not need to').toBe(1);
	expect(bar.clipped).toEqual([]);
	expect(bar.scrollingAncestors).toEqual([]);
});
