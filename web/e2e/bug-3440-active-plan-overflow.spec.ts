import { test, expect } from './fixtures';

/**
 * BUG-3440 — a long plan title on the dashboard's Active Plans overran its
 * card: `.plan-title` was `white-space: nowrap` with nothing letting it shrink
 * or clip, so an unbroken title pushed the row (and the page) wider than the
 * viewport. Measured, not eyeballed: at every width from desktop down to 375px
 * and in both themes, each plan row's content stays inside it, the page does
 * not scroll sideways, and the full title is available on hover.
 */

const UNBROKEN = 'Plan' + 'X'.repeat(160) + 'End';
const SPACED =
	'A deliberately long plan title with many ordinary words that would wrap on a narrow screen if it were allowed to and must never push the card wider';

const WIDTHS = [1280, 768, 375];
const THEMES = ['light', 'dark'] as const;

test.describe('BUG-3440: Active Plans cards hold long titles', () => {
	test('no plan row overflows at any width, in either theme', async ({ page, fixture }) => {
		for (const title of [UNBROKEN, SPACED]) {
			const res = await page.request.post(
				`/api/v1/workspaces/${fixture.workspaceSlug}/collections/plans/items`,
				{ data: { title, fields: JSON.stringify({ status: 'active' }) } }
			);
			expect(res.status(), await res.text()).toBe(201);
		}

		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}`);
		const rows = page.locator('a.plan-row').filter({
			has: page.locator('.plan-title', { hasText: /^(PlanX|A deliberately long plan title)/ })
		});
		await expect(rows).toHaveCount(2);

		for (const theme of THEMES) {
			await page.evaluate((t) => document.documentElement.setAttribute('data-theme', t), theme);
			await expect(page.locator('html')).toHaveAttribute('data-theme', theme);
			for (const width of WIDTHS) {
				await page.setViewportSize({ width, height: 900 });
				for (let i = 0; i < 2; i++) {
					const m = await rows.nth(i).evaluate((row) => {
						const r = row.getBoundingClientRect();
						const kids = Array.from(row.children).map((c) => c.getBoundingClientRect());
						const title = row.querySelector('.plan-title') as HTMLElement;
						return {
							rowScroll: row.scrollWidth,
							rowClient: row.clientWidth,
							rowRight: r.right,
							kidsRight: Math.max(...kids.map((k) => k.right)),
							titleAttr: title.getAttribute('title') ?? '',
							titleText: title.textContent ?? '',
							docScroll: document.documentElement.scrollWidth,
							viewport: window.innerWidth
						};
					});
					const where = `${theme} @${width}px row ${i}`;
					expect(m.rowScroll, `${where}: the row's content is wider than the row`).toBeLessThanOrEqual(m.rowClient + 1);
					expect(m.kidsRight, `${where}: a child sticks out past the row`).toBeLessThanOrEqual(m.rowRight + 1);
					expect(m.docScroll, `${where}: the page scrolls sideways`).toBeLessThanOrEqual(m.viewport + 1);
					expect(m.titleAttr, `${where}: the full title is not on hover`).toBe(m.titleText.trim());
				}
			}
		}
	});
});
