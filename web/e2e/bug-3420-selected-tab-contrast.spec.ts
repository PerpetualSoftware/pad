import { test, expect, type Page } from '@playwright/test';
import { asNewUser, openTabs, show } from './lib/workspace-tabs';

/**
 * BUG-3420: in dark mode the selected workspace tab was hard to tell from the
 * others. Its background is the page colour (the tab flows into the page), and
 * that measured 1.07:1 against the strip and its neighbours in dark mode
 * (1.09:1 in light), with the same text colour and weight. The fix is an accent
 * line along the active tab's top edge, an inset box-shadow.
 *
 * This pins the requirement, not the style: in BOTH colour schemes the active
 * tab carries an indicator colour that reaches WCAG 1.4.11's 3:1 against an
 * inactive tab's background AND the strip's. Computed in the browser from the
 * rendered colours, so a token change that sinks the contrast fails here.
 */

type Ratios = { indicator: string | null; vsInactive: number; vsStrip: number };

async function activeTabIndicator(page: Page): Promise<Ratios> {
	return page.evaluate(() => {
		const parse = (c: string) => {
			const m = c.match(/rgba?\(([^)]+)\)/);
			if (!m) return null;
			const p = m[1].split(/[ ,/]+/).filter(Boolean).map(Number);
			return { r: p[0], g: p[1], b: p[2], a: p.length > 3 ? p[3] : 1 };
		};
		type C = NonNullable<ReturnType<typeof parse>>;
		const lum = ({ r, g, b }: C) => {
			const f = (v: number) => ((v /= 255) <= 0.03928 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4);
			return 0.2126 * f(r) + 0.7152 * f(g) + 0.0722 * f(b);
		};
		const ratio = (a: C, b: C) => {
			const [x, y] = [lum(a), lum(b)].sort((p, q) => q - p);
			return (x + 0.05) / (y + 0.05);
		};
		// The background an element shows: blend up to the first opaque ancestor.
		const effBg = (el: Element | null): C => {
			const stack: C[] = [];
			for (let n = el; n; n = n.parentElement) {
				const c = parse(getComputedStyle(n).backgroundColor);
				if (c && c.a > 0) {
					stack.push(c);
					if (c.a >= 1) break;
				}
			}
			let acc: C = { r: 0, g: 0, b: 0, a: 1 };
			for (let i = stack.length - 1; i >= 0; i--) {
				const t = stack[i];
				acc = { r: t.r * t.a + acc.r * (1 - t.a), g: t.g * t.a + acc.g * (1 - t.a), b: t.b * t.a + acc.b * (1 - t.a), a: 1 };
			}
			return acc;
		};
		const active = document.querySelector('.workspace-tab.active')!;
		const inactive = document.querySelector('.workspace-tab:not(.active)')!;
		const strip = active.parentElement!;
		const shadow = getComputedStyle(active).boxShadow;
		const ind = shadow === 'none' ? null : parse(shadow);
		if (!ind || ind.a < 1) return { indicator: null, vsInactive: 0, vsStrip: 0 };
		return {
			indicator: shadow,
			vsInactive: +ratio(ind, effBg(inactive)).toFixed(2),
			vsStrip: +ratio(ind, effBg(strip)).toFixed(2)
		};
	});
}

for (const colorScheme of ['dark', 'light'] as const) {
	test(`BUG-3420: the active workspace tab has a 3:1 indicator (${colorScheme})`, async ({ page }, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'the tab bar is desktop only');
		await page.emulateMedia({ colorScheme });
		const { username, slugs } = await asNewUser(page);
		await openTabs(page, slugs, 3);
		await show(page, username, slugs[1], 3);
		await expect
			.poll(() => page.evaluate(() => document.documentElement.getAttribute('data-theme')))
			.toBe(colorScheme === 'light' ? 'light' : null);
		const r = await activeTabIndicator(page);
		expect(r.indicator, 'the active tab carries no solid indicator').not.toBeNull();
		expect(r.vsInactive, `indicator vs an inactive tab (${r.indicator})`).toBeGreaterThanOrEqual(3);
		expect(r.vsStrip, `indicator vs the strip (${r.indicator})`).toBeGreaterThanOrEqual(3);
	});
}
