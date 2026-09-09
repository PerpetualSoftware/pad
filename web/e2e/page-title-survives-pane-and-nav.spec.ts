import { test, expect } from './fixtures';
import { browserLogin, seedDoc } from './lib/collab-helpers';
import type { SuiteFixture } from './fixtures';

/**
 * TASK-2245 / audit cluster C118 — the page title must survive a pane toggle
 * and a route change.
 *
 * THE DEFECT. The workspace layout cleared `section`/`item` from a `$effect`
 * whose comment claimed it "depends only on `page.url.pathname`". It did not:
 * reading `page.url.pathname` tracks the reactive `page.url`, so a SEARCH-only
 * change re-ran it — and opening/closing the item pane (`?item=REF`) is exactly
 * that, as is a view switch (`?view=`). The leaf then reclaimed its section and
 * the layout's clear landed AFTER it, so the tab title fell back to
 * `{Workspace} · Pad` and the mobile context bar fell through to its raw-slug
 * fallback ("share rig" for "Share Rig", as measured on device).
 *
 * The same ordering broke a genuine cross-route navigation too — the leaf set
 * its section, the clear wiped it — which was PRE-EXISTING and is why the clear
 * moved out of the effect graph into `beforeNavigate` rather than being made
 * more careful inside it.
 *
 * WHY E2E RATHER THAN A UNIT TEST. Every part of this is real-browser
 * behaviour: SvelteKit's client router, effect scheduling across a layout and
 * its leaf, and `document.title` itself. The defect was found with an
 * instrument in a real browser after two static hypotheses about it were both
 * wrong, and a jsdom test could not have distinguished them either.
 *
 * These legs FAIL against the pre-fix layout: the close leg and the nav leg
 * both report `"{Workspace} · Pad"`.
 */

function docsUrl(fixture: SuiteFixture, query = ''): string {
	return `/${fixture.adminUsername}/${fixture.workspaceSlug}/docs${query}`;
}

test.describe('page title survives a pane toggle and a route change (TASK-2245)', () => {
	test.beforeEach(({}, testInfo) => {
		test.skip(
			testInfo.project.name !== 'desktop-chromium',
			'title behaviour is width-independent; one project is enough',
		);
	});

	test('closing the item pane restores the collection title, not the bare workspace', async ({
		page,
		fixture,
		request,
	}) => {
		await browserLogin(page);
		await seedDoc(fixture, request, 'Title survives pane');
		await page.goto(docsUrl(fixture));

		const card = page
			.locator('.item-card')
			.filter({ has: page.locator('.card-title', { hasText: 'Title survives pane' }) });
		await expect(card.first()).toBeVisible();

		// Baseline: the collection owns the title.
		await expect.poll(() => page.title()).toContain('Docs');

		// Open the pane IN-APP. A `page.goto` would be a full boot and would not
		// exercise the transition at all.
		await card.first().click();
		await expect(page.locator('.item-pane')).toBeVisible();
		// The pane titles by item REF, not by the item's title (`formatItemRef`),
		// so this asserts the ref shape rather than the seeded text.
		await expect.poll(() => page.title()).toMatch(/^DOC-\d+ /);

		// Close it the way a user does.
		await page.keyboard.press('Escape');
		await expect.poll(() => new URL(page.url()).searchParams.get('item')).toBeNull();

		// THE ASSERTION. Pre-fix this settles on `{Workspace} · Pad`, because the
		// layout's clear lands after the collection page reclaims its section.
		await expect.poll(() => page.title()).toContain('Docs');
	});

	test('a cross-route SPA navigation keeps the destination section', async ({ page, fixture }) => {
		await browserLogin(page);
		await page.goto(docsUrl(fixture));
		await expect.poll(() => page.title()).toContain('Docs');

		// In-app navigation: dispatched in page so the SvelteKit router handles
		// it client-side. Off-viewport chrome makes a real click unreliable here,
		// and the point of the leg is the CLIENT-SIDE path — a full load would
		// remount everything and pass vacuously.
		const target = `/${fixture.adminUsername}/${fixture.workspaceSlug}/insights`;
		await page.evaluate((href) => {
			const el = document.querySelector(`a[href="${href}"]`);
			if (el) {
				el.dispatchEvent(
					new MouseEvent('click', { bubbles: true, cancelable: true, view: window }),
				);
			} else {
				history.pushState({}, '', href);
				dispatchEvent(new PopStateEvent('popstate'));
			}
		}, target);

		await expect.poll(() => new URL(page.url()).pathname).toBe(target);
		// Pre-fix this settles on `{Workspace} · Pad` — a separate, pre-existing
		// instance of the same ordering defect.
		await expect.poll(() => page.title()).toContain('Insights');
	});
});
