import { test, expect } from './fixtures';
import { browserLogin, seedDoc, SYNCED_BADGE_SELECTOR } from './lib/collab-helpers';
import type { APIRequestContext, Locator, Page } from '@playwright/test';
import type { SuiteFixture } from './fixtures';

/**
 * Tab-strip TIER rule (PLAN-2326 DR-7 / DR-9 / DR-10 / TASK-2329).
 *
 * The strip carries `container-type: inline-size` and a single
 * `@container item-tab-strip (max-width: 699px)` rule: below 700px of STRIP
 * width the badge icons drop to bare counts and the star folds away, node
 * retained.
 *
 * Two things are worth a real browser here, and neither is unit-testable:
 *
 *   1. The tier keys off the CONTAINER, not the viewport. This is the whole
 *      reason DR-2 chose `@container`: the docked pane's width is dragged by
 *      the user and persisted (`PaneHost.svelte` — `clamp(360px, 38%, 640px)`
 *      by CSS, `PANE_WIDTH_MIN 360` / `PANE_WIDTH_MAX 720` by JS), so it is
 *      decoupled from the viewport entirely. A `@media (min-width: 700px)`
 *      rule would sail through every assertion in test 1 — the viewport never
 *      changes size in it. Squeezing the FULL-PAGE master by opening a pane
 *      beside it drives the flip in place, at a fixed viewport, with no
 *      reload and no remount.
 *
 *   2. DR-9's width allocation actually holds at the 360px pane floor. Four
 *      tabs, two count badges, quick actions and the ⋯ overflow have to share
 *      a ~312px strip; `.strip-actions` is `flex: 0 0 auto` and `.pane-tabs`
 *      scrolls, so the contract is "one row, every tab still reachable" — not
 *      "everything fits". That is a layout property of the real box model.
 *
 * Measured strip widths this rule was calibrated against (TASK-2329):
 *   full page 892-912px (capped by `--content-max-width`, identical at 1440
 *   and 2560) · full page with a pane docked 526-678px · docked pane 312-672px
 *   · mobile overlay 342px. Only the pane-free full page reaches the full tier.
 *
 * Desktop-split concern, so the desktop project alone covers it.
 */

const DESKTOP = { width: 1200, height: 900 };
const WIDE = { width: 1440, height: 1000 };
const SYNC_TIMEOUT = 20_000;

function authHeaders(fixture: SuiteFixture) {
	return { Authorization: `Bearer ${fixture.apiToken}`, 'Content-Type': 'application/json' };
}

interface SeededItem {
	id: string;
	slug: string;
	title: string;
}

async function seedDocWith(
	fixture: SuiteFixture,
	request: APIRequestContext,
	titlePrefix: string,
	{ content = '', parent = '' }: { content?: string; parent?: string } = {},
): Promise<SeededItem> {
	const title = `${titlePrefix} ${Date.now()}${Math.floor(Math.random() * 1000)}`;
	const resp = await request.post(
		`/api/v1/workspaces/${fixture.workspaceSlug}/collections/docs/items`,
		{
			headers: authHeaders(fixture),
			data: { title, fields: parent ? JSON.stringify({ parent }) : '{}', content },
		},
	);
	if (!resp.ok()) throw new Error(`doc create failed (${resp.status()}): ${await resp.text()}`);
	return { ...((await resp.json()) as { id: string; slug: string }), title };
}

async function seedRelatedLink(
	fixture: SuiteFixture,
	request: APIRequestContext,
	sourceSlug: string,
	targetId: string,
): Promise<void> {
	const resp = await request.post(
		`/api/v1/workspaces/${fixture.workspaceSlug}/items/${sourceSlug}/links`,
		{ headers: authHeaders(fixture), data: { target_id: targetId, link_type: 'related' } },
	);
	if (!resp.ok()) throw new Error(`link create failed (${resp.status()}): ${await resp.text()}`);
}

/** The full-page master column — the `.item-page` the host renders directly. */
function masterCol(page: Page): Locator {
	return page.locator('.item-page-host > .item-page');
}

/** Rendered width of a strip, straight off the box model. */
async function stripWidth(scope: Locator): Promise<number> {
	return Math.round(
		await scope.locator('.tab-strip').evaluate((n) => n.getBoundingClientRect().width),
	);
}

test.describe('tab-strip tiers (PLAN-2326 / TASK-2329)', () => {
	test.beforeEach(({}, testInfo) => {
		test.skip(
			testInfo.project.name !== 'desktop-chromium',
			'the pane is a desktop-split concern; one project is enough',
		);
	});

	test('the tier keys off the STRIP CONTAINER, not the viewport: squeezing the full-page master with a docked pane flips it compact in place — and closing the pane flips it back (DR-2 / DR-10)', async ({
		page,
		fixture,
		request,
	}) => {
		await page.setViewportSize(DESKTOP);
		await browserLogin(page);

		// Master with a CHILD (renders the 🌳 badge) plus a RELATED item to open
		// in the pane. The child badge is what proves `.badge-icon` folds while
		// `.badge-count` survives — a text-node split TASK-2328 made possible.
		const master = await seedDocWith(fixture, request, 'Tier master');
		await seedDocWith(fixture, request, 'Tier child', { parent: master.id });
		const related = await seedDocWith(fixture, request, 'Tier related');
		await seedRelatedLink(fixture, request, master.slug, related.id);

		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/docs/${master.slug}`);

		const col = masterCol(page);
		const star = col.locator('button.star-btn');
		const badgeIcon = col.locator('.strip-actions .badge-icon');
		const badgeCount = col.locator('.strip-actions .badge-count');

		// ── FULL tier. 892px of strip at this viewport, comfortably over 700. ──
		await expect(col.locator('.tab-strip')).toBeVisible();
		await expect(badgeCount.first()).toBeVisible(); // children badge has arrived
		expect(await stripWidth(col)).toBeGreaterThanOrEqual(700);
		await expect(star).toBeVisible();
		await expect(badgeIcon.first()).toBeVisible();

		// ── Squeeze it. Open the pane from the Relationships tab — same route,
		//    same viewport, no reload: ONLY the master column's width changes. ──
		await col.getByRole('tab', { name: 'Relationships' }).click();
		await col
			.locator('.relationship-group', { hasText: 'Related' })
			.locator('a.link-target', { hasText: 'Tier related' })
			.click();
		await expect(page.locator('.item-pane')).toBeVisible();

		// The viewport is byte-identical to what it was in the full tier above.
		// A media query cannot distinguish these two states; a container query can.
		expect(page.viewportSize()).toEqual(DESKTOP);

		// ── COMPACT tier, driven purely by the container shrinking. ──
		await expect(star).toBeHidden();
		expect(await stripWidth(col)).toBeLessThan(700);
		await expect(badgeIcon.first()).toBeHidden();
		// DR-11: the star FOLDS, it is not removed. The BUG-2263 freeze
		// assertions in pane-full-page-capstone.spec.ts depend on the node.
		await expect(star).toHaveCount(1);
		// The count survives the icon — the point of DR-9's span split.
		await expect(badgeCount.first()).toBeVisible();
		// The collab badge is untouched by the tier rule — it lives in
		// `.meta-info`, outside the query container entirely. Asserted here
		// only because SYNCED_BADGE_SELECTOR is load-bearing for four other
		// suites and this is the narrowest surface any of them exercise.
		await expect(col.locator(SYNCED_BADGE_SELECTOR)).toBeVisible({ timeout: SYNC_TIMEOUT });
		// ⋯ never folds at any tier — it is the compact tier's escape hatch, and
		// it carries the Star the strip just folded away.
		await expect(col.locator('button.pane-more-btn')).toBeVisible();
		await col.locator('button.pane-more-btn').click();
		await expect(col.getByRole('menuitem', { name: /^(Star|Unstar)$/ })).toBeVisible();
		await page.keyboard.press('Escape');

		// ── Close the pane → the container widens again and the tier reverts.
		//    A one-way rule would pass every assertion above. ──
		await page.locator('.item-pane').getByRole('button', { name: 'Close pane' }).click();
		await expect(page.locator('.item-pane')).toHaveCount(0);
		expect(await stripWidth(col)).toBeGreaterThanOrEqual(700);
		await expect(star).toBeVisible();
		await expect(badgeIcon.first()).toBeVisible();
	});

	test('worst case at the 360px pane floor — children + backlinks + quick actions + owner rights: the strip stays ONE row and every tab is still reachable through the scroll (DR-9)', async ({
		page,
		fixture,
		request,
	}) => {
		await page.setViewportSize(WIDE);
		await browserLogin(page);

		// Every action the strip can hold at once: a child badge, a backlink
		// badge, the owner quick-actions trigger (the fixture user owns the
		// workspace, so it renders even with no seeded prompt actions), and ⋯.
		const master = await seedDocWith(fixture, request, 'Tier worst');
		await seedDocWith(fixture, request, 'Tier worst child', { parent: master.id });
		await seedDocWith(fixture, request, 'Tier worst mention', {
			content: `mentions [[${master.title}]] here`,
		});

		await page.goto(
			`/${fixture.adminUsername}/${fixture.workspaceSlug}/docs?item=${master.slug}`,
		);
		const pane = page.locator('.item-pane');
		await expect(pane.locator('.tab-strip')).toBeVisible();

		// Drag the divider hard right — `clampPaneWidth` pins it at the 360px
		// floor, the narrowest the pane can ever be on a roomy container.
		const divider = page.locator('.pane-divider');
		const box = await divider.boundingBox();
		if (!box) throw new Error('pane divider has no box');
		await page.mouse.move(box.x + box.width / 2, box.y + 200);
		await page.mouse.down();
		await page.mouse.move(box.x + box.width / 2 + 600, box.y + 200, { steps: 20 });
		await page.mouse.up();
		await expect
			.poll(async () => Math.round(await pane.evaluate((n) => n.getBoundingClientRect().width)))
			.toBe(360);

		// Both badges present (the strip is at its most crowded).
		await expect(pane.locator('.strip-actions .badge-count')).toHaveCount(2);
		await expect(pane.locator('button.trigger-btn[title="Quick actions"]')).toBeVisible();
		await expect(pane.locator('button.pane-more-btn')).toBeVisible();
		// Compact tier throughout.
		await expect(pane.locator('button.star-btn')).toBeHidden();
		await expect(pane.locator('button.star-btn')).toHaveCount(1);
		await expect(pane.locator('.strip-actions .badge-icon').first()).toBeHidden();

		// ── ONE ROW. `.strip-actions` holds its size and `.pane-tabs` scrolls
		//    rather than wrapping, so all four tabs share a single offsetTop. ──
		const tabTops = await pane.locator('.pane-tabs').evaluate((n) => {
			const tabs = Array.from(n.querySelectorAll<HTMLElement>('[role="tab"]'));
			return Array.from(new Set(tabs.map((t) => Math.round(t.getBoundingClientRect().top))));
		});
		expect(tabTops).toHaveLength(1);

		// ── EVERY TAB REACHABLE. The tablist is a scrollport narrower than its
		//    content; each tab must scroll into it and take the selection. ──
		const tabs = pane.getByRole('tab');
		await expect(tabs).toHaveCount(4);
		for (const name of ['Details', 'Relationships', 'Activity', 'Versions']) {
			const tab = pane.getByRole('tab', { name });
			await tab.scrollIntoViewIfNeeded();
			await expect(tab).toBeInViewport();
			await tab.click();
			await expect(tab).toHaveAttribute('aria-selected', 'true');
		}

		// The ⋯ panel is anchored inside `.strip-actions`, a sibling of the
		// scrolling tablist. Nothing on `.tab-strip` clips it — proving the
		// "no overflow/contain on the container" constraint still holds at the
		// narrowest width, where a clip would be most visible.
		await pane.locator('button.pane-more-btn').click();
		await expect(pane.getByRole('menuitem', { name: 'Move to collection…' })).toBeVisible();

		// ── The folded star's escape hatch actually WORKS here. This is the
		//    width the mobile overlay always runs at, so "Star lives in ⋯" has
		//    to be a working action, not just a rendered row. ──
		await pane.getByRole('menuitem', { name: 'Star' }).click();
		await expect(pane.getByRole('menuitem', { name: 'Star' })).toHaveCount(0); // menu closed
		await pane.locator('button.pane-more-btn').click();
		await expect(pane.getByRole('menuitem', { name: 'Unstar' })).toBeVisible();
		await page.keyboard.press('Escape');
	});
});
