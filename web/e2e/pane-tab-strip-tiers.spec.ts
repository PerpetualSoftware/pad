import { test, expect } from './fixtures';
import { browserLogin, SYNCED_BADGE_SELECTOR } from './lib/collab-helpers';
import type { APIRequestContext, Locator, Page } from '@playwright/test';
import type { SuiteFixture } from './fixtures';

/**
 * Tab-strip TIER rules (PLAN-2326 / TASK-2329).
 *
 * `.tab-strip` is `container-type: inline-size` and carries three rules keyed
 * on its OWN width, not the viewport's:
 *
 *   < 560px   SPLIT — `.strip-actions` takes a full row of its own, `order:
 *             -1` so it sits ABOVE the tablist, and the tabs get the full
 *             width. Everything the shared row folded comes back.
 *   560-699   FOLD  — still one row, but tight: badge icons drop to bare
 *             counts and the star folds (node retained, DR-11).
 *   <= 330px  the tabs' horizontal padding trims 11px -> 8px, the last 24px
 *             needed to fit four tabs in a 312px strip.
 *
 * Why the split exists at all: combining tabs and actions on one row is not
 * survivable at phone widths and no amount of folding fixes it. The four tab
 * labels are a fixed 321px, and even a hypothetical action group of nothing
 * but `⋯` (38px) needs 359px against the 342px a 390px phone has. Measured on
 * the WIDEST common phone before this change, 430px: 382px of strip, 179px of
 * tab scrollport, two of four tabs clipped — under `scrollbar-width: none`,
 * so with no hint they existed.
 *
 * Three things here need a real browser:
 *
 *   1. The rules key off the CONTAINER. The docked pane's width is dragged by
 *      the user and persisted (`PaneHost.svelte`: `PANE_WIDTH_MIN 360` /
 *      `PANE_WIDTH_MAX 720`), so it is decoupled from the viewport entirely,
 *      and the full page's own strip is capped at 912px by
 *      `--content-max-width` at ANY monitor size. Test 1 drives the split at
 *      a FIXED viewport by squeezing the full-page master with a docked pane;
 *      a `@media` rule would sail through every assertion in it.
 *   2. Whether four tabs actually fit once they own a row. That is a box-model
 *      fact, and the ≤330px padding trim is what makes it true at 360.
 *   3. That the split did not break the active tab's 1px overlap of the
 *      strip's bottom divider — the reason the actions go ABOVE rather than
 *      below.
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

/** Rendered width of the strip inside `scope`, straight off the box model. */
async function stripWidth(scope: Locator): Promise<number> {
	return Math.round(
		await scope.locator('.tab-strip').evaluate((n) => n.getBoundingClientRect().width),
	);
}

interface StripLayout {
	/** true when `.strip-actions` sits on its own row above the tablist. */
	split: boolean;
	/** every tab shares one offsetTop — the tablist itself never wraps. */
	tabsOnOneRow: boolean;
	/** the tablist is not overflowing: all four labels are actually shown. */
	tabsFit: boolean;
	/** active tab's border-box bottom minus the strip's — 0 means it covers
	 *  the 1px divider exactly, which is the designed overlap. */
	underlineOffset: number;
	/** the action group must never become a child of the tablist (DR-4). */
	tablistContainsActions: boolean;
}

async function stripLayout(scope: Locator): Promise<StripLayout> {
	return scope.locator('.tab-strip').evaluate((strip) => {
		const tabs = strip.querySelector('.pane-tabs') as HTMLElement;
		const acts = strip.querySelector('.strip-actions') as HTMLElement;
		const tabEls = Array.from(tabs.querySelectorAll<HTMLElement>('[role="tab"]'));
		const active = tabs.querySelector('.pane-tab.on');
		const sr = strip.getBoundingClientRect();
		const tr = tabs.getBoundingClientRect();
		const ar = acts.getBoundingClientRect();
		return {
			split: ar.bottom <= tr.top + 1,
			tabsOnOneRow:
				new Set(tabEls.map((t) => Math.round(t.getBoundingClientRect().top))).size === 1,
			tabsFit: tabs.scrollWidth <= tabs.clientWidth,
			underlineOffset: active
				? Math.round(active.getBoundingClientRect().bottom - sr.bottom)
				: NaN,
			tablistContainsActions: tabs.contains(acts),
		};
	});
}

test.describe('tab-strip tiers (PLAN-2326 / TASK-2329)', () => {
	test.beforeEach(({}, testInfo) => {
		test.skip(
			testInfo.project.name !== 'desktop-chromium',
			'the pane is a desktop-split concern; one project is enough',
		);
	});

	test('the SPLIT keys off the strip CONTAINER, not the viewport: docking a pane beside the full-page master splits its strip in place at a fixed viewport, and closing the pane rejoins it', async ({
		page,
		fixture,
		request,
	}) => {
		await page.setViewportSize(DESKTOP);
		await browserLogin(page);

		// Master with a CHILD (renders the 🌳 badge) plus a RELATED item to open
		// in the pane.
		const master = await seedDocWith(fixture, request, 'Tier master');
		await seedDocWith(fixture, request, 'Tier child', { parent: master.id });
		const related = await seedDocWith(fixture, request, 'Tier related');
		await seedRelatedLink(fixture, request, master.slug, related.id);

		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/docs/${master.slug}`);

		const col = masterCol(page);
		const star = col.locator('button.star-btn');
		const badgeIcon = col.locator('.strip-actions .badge-icon');
		const badgeCount = col.locator('.strip-actions .badge-count');

		// ── SHARED ROW. 892px of strip at this viewport, way over 699. ──
		await expect(col.locator('.tab-strip')).toBeVisible();
		await expect(badgeCount.first()).toBeVisible(); // children badge has arrived
		expect(await stripWidth(col)).toBeGreaterThanOrEqual(700);
		let layout = await stripLayout(col);
		expect(layout.split).toBe(false);
		expect(layout.tabsFit).toBe(true);
		expect(layout.tablistContainsActions).toBe(false);
		const sharedRowUnderline = layout.underlineOffset;
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

		// The viewport is byte-identical to what it was on the shared row above.
		// A media query cannot distinguish these two states; a container query can.
		expect(page.viewportSize()).toEqual(DESKTOP);

		// ── SPLIT, driven purely by the container shrinking to ~526px. ──
		await expect
			.poll(async () => (await stripLayout(col)).split, { timeout: 5000 })
			.toBe(true);
		expect(await stripWidth(col)).toBeLessThan(560);
		layout = await stripLayout(col);
		expect(layout.tabsOnOneRow).toBe(true);
		expect(layout.tabsFit).toBe(true);
		expect(layout.tablistContainsActions).toBe(false);
		// Actions ABOVE, not below: the strip owns the bottom divider and the
		// active tab overlaps it by 1px. Keeping the tablist last preserves that,
		// so the underline offset is unchanged from the shared row.
		expect(layout.underlineOffset).toBe(sharedRowUnderline);
		// Below 560 the actions have room again, so what the FOLD band hides is
		// visible here. This is the behaviour a single <700 fold got wrong.
		await expect(star).toBeVisible();
		await expect(badgeIcon.first()).toBeVisible();
		// The collab badge lives in `.meta-info`, outside the query container —
		// asserted only because SYNCED_BADGE_SELECTOR is load-bearing for four
		// other suites and this is the narrowest surface any of them exercises.
		await expect(col.locator(SYNCED_BADGE_SELECTOR)).toBeVisible({ timeout: SYNC_TIMEOUT });

		// ── Close the pane → the container widens and the rows rejoin. A
		//    one-way rule would pass every assertion above. ──
		await page.locator('.item-pane').getByRole('button', { name: 'Close pane' }).click();
		await expect(page.locator('.item-pane')).toHaveCount(0);
		await expect.poll(async () => (await stripLayout(col)).split, { timeout: 5000 }).toBe(false);
		expect(await stripWidth(col)).toBeGreaterThanOrEqual(700);
	});

	test('worst case at the 360px pane floor — children + backlinks + quick actions + owner rights: actions take their own row and all four tabs FIT without scrolling', async ({
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

		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/docs?item=${master.slug}`);
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

		// The strip is at its most crowded: both badges + quick actions + ⋯.
		await expect(pane.locator('.strip-actions .badge-count')).toHaveCount(2);
		await expect(pane.locator('button.trigger-btn[title="Quick actions"]')).toBeVisible();
		await expect(pane.locator('button.pane-more-btn')).toBeVisible();
		// And the star is VISIBLE here, not folded — below 560 the actions own a
		// row, so there is room for it. (A single <700 fold hid it on every
		// phone; that was the bug this keying fixes.)
		await expect(pane.locator('button.star-btn')).toBeVisible();
		await expect(pane.locator('.strip-actions .badge-icon').first()).toBeVisible();

		const layout = await stripLayout(pane);
		expect(layout.split).toBe(true);
		expect(layout.tabsOnOneRow).toBe(true);
		// The whole point: FIT, not "reachable by scrolling". At 312px of strip
		// the ≤330px padding rule trims the tabs from 321px to 297px.
		expect(layout.tabsFit).toBe(true);
		expect(layout.tablistContainsActions).toBe(false);

		// Every tab genuinely on screen and selectable.
		const tabs = pane.getByRole('tab');
		await expect(tabs).toHaveCount(4);
		for (const name of ['Details', 'Relationships', 'Activity', 'Versions']) {
			const tab = pane.getByRole('tab', { name });
			await expect(tab).toBeInViewport();
			await tab.click();
			await expect(tab).toHaveAttribute('aria-selected', 'true');
		}

		// `order: -1` is visual only — DOM order is untouched, so the tablist's
		// arrow-key roving tabindex still walks exactly the four tabs.
		await pane.locator('.pane-tab').first().focus();
		const walk: string[] = [];
		for (let i = 0; i < 4; i++) {
			walk.push(await page.evaluate(() => (document.activeElement?.textContent || '').trim()));
			await page.keyboard.press('ArrowRight');
		}
		expect(walk).toEqual(['Details', 'Relationships', 'Activity', 'Versions']);

		// The ⋯ panel is anchored inside `.strip-actions`. Nothing on
		// `.tab-strip` clips it — the "no overflow/contain on the container"
		// constraint still holds at the narrowest width, where a clip would be
		// most visible.
		await pane.locator('button.pane-more-btn').click();
		await expect(pane.getByRole('menuitem', { name: 'Move to collection…' })).toBeVisible();
		await page.keyboard.press('Escape');
	});

	test('the FOLD band (560-699px) still folds: a pane dragged to its 720px maximum keeps one row and drops the badge icons + star, with Star still reachable in the ⋯ menu', async ({
		page,
		fixture,
		request,
	}) => {
		// A 720px pane (PANE_WIDTH_MAX) is a 672px strip — inside the fold band
		// and the ONLY way to reach it from a docked pane.
		await page.addInitScript(() => localStorage.setItem('pad-pane-width', '720'));
		await page.setViewportSize({ width: 2000, height: 1000 });
		await browserLogin(page);

		const master = await seedDocWith(fixture, request, 'Tier fold');
		await seedDocWith(fixture, request, 'Tier fold child', { parent: master.id });

		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/docs?item=${master.slug}`);
		const pane = page.locator('.item-pane');
		await expect(pane.locator('.tab-strip')).toBeVisible();
		await expect(pane.locator('.strip-actions .badge-count').first()).toBeVisible();

		const width = await stripWidth(pane);
		expect(width).toBeGreaterThanOrEqual(560);
		expect(width).toBeLessThan(700);

		const layout = await stripLayout(pane);
		expect(layout.split).toBe(false);
		expect(layout.tabsFit).toBe(true);

		// Folded: icons gone, counts kept, star hidden but RETAINED (DR-11 — the
		// BUG-2263 freeze assertions in pane-full-page-capstone.spec.ts need the
		// node to exist).
		await expect(pane.locator('.strip-actions .badge-icon').first()).toBeHidden();
		await expect(pane.locator('.strip-actions .badge-count').first()).toBeVisible();
		await expect(pane.locator('button.star-btn')).toBeHidden();
		await expect(pane.locator('button.star-btn')).toHaveCount(1);

		// The folded star still has a home, and it works.
		await pane.locator('button.pane-more-btn').click();
		await pane.getByRole('menuitem', { name: 'Star' }).click();
		await pane.locator('button.pane-more-btn').click();
		await expect(pane.getByRole('menuitem', { name: 'Unstar' })).toBeVisible();
		await page.keyboard.press('Escape');
	});
});
