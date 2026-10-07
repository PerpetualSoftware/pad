import { test, expect } from './fixtures';
import { browserLogin } from './lib/collab-helpers';

// TASK-2262 (audit C10 + the sidebar add buttons). Measured, not eyeballed:
// a keyboard-focused select shows a real ring (it showed NOTHING before), and
// the sidebar's icon-only "+" controls reach 3:1 against their background at
// rest (WCAG 1.4.11; they measured 2.29 / 2.12 at opacity 0.5).

test.describe('TASK-2262: focus ring and non-text contrast', () => {
	test.beforeEach(({}, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'desktop layout');
	});

	for (const theme of ['dark', 'light'] as const) {
		test(`a keyboard-focused select shows a 2px ring (${theme})`, async ({ page, fixture }) => {
			await page.emulateMedia({ colorScheme: theme });
			await browserLogin(page);
			await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/settings`);
			await page.getByRole('tab', { name: /Members/ }).click();
			await page.getByPlaceholder('Email address').click();
			await page.keyboard.press('Tab');
			const focused = await page.evaluate(() => {
				const e = document.activeElement as HTMLElement;
				const s = getComputedStyle(e);
				return { tag: e.tagName, style: s.outlineStyle, width: s.outlineWidth };
			});
			expect(focused.tag).toBe('SELECT');
			expect(focused.style).not.toBe('none');
			expect(parseFloat(focused.width)).toBeGreaterThanOrEqual(2);
		});

		test(`the sidebar "+" controls reach 3:1 at rest (${theme})`, async ({ page, fixture }) => {
			await page.emulateMedia({ colorScheme: theme });
			await browserLogin(page);
			await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}`);
			await expect(page.locator('.section-add-btn').first()).toBeVisible();
			await page.mouse.move(1200, 700); // nothing hovered
			const ratios = await page.evaluate(() => {
				const parse = (c: string) => {
					const p = c.match(/rgba?\(([^)]+)\)/)![1].split(/[ ,/]+/).filter(Boolean).map(Number);
					return { r: p[0], g: p[1], b: p[2], a: p.length > 3 ? p[3] : 1 };
				};
				const lum = ({ r, g, b }: { r: number; g: number; b: number }) => {
					const f = (v: number) => ((v /= 255) <= 0.03928 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4);
					return 0.2126 * f(r) + 0.7152 * f(g) + 0.0722 * f(b);
				};
				const bgOf = (el: Element | null) => {
					for (let n = el; n; n = n.parentElement) {
						const c = parse(getComputedStyle(n).backgroundColor);
						if (c.a >= 1) return c;
					}
					return parse(getComputedStyle(document.body).backgroundColor);
				};
				return [...document.querySelectorAll<HTMLElement>('.section-add-btn, .nav-quick-add')]
					.filter((e) => e.offsetParent)
					.map((e) => {
						const s = getComputedStyle(e);
						const bg = bgOf(e.parentElement);
						const fg = parse(s.color);
						const a = parseFloat(s.opacity) * fg.a;
						const mix = { r: fg.r * a + bg.r * (1 - a), g: fg.g * a + bg.g * (1 - a), b: fg.b * a + bg.b * (1 - a) };
						const [x, y] = [lum(mix), lum(bg)].sort((p, q) => q - p);
						return (x + 0.05) / (y + 0.05);
					});
			});
			expect(ratios.length).toBeGreaterThan(0);
			for (const r of ratios) expect(r).toBeGreaterThanOrEqual(3);
		});
	}
});
