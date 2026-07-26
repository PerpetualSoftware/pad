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
 *   560-639   FOLD  — still one row, but tight: badge icons drop to bare
 *             counts and the star folds (node retained, DR-11) with its
 *             affordance moving to a `.strip-star-row` in the ⋯ menu, gated
 *             by the same query so exactly one of the two ever shows.
 *   <= 330px  the tabs' horizontal padding trims 11px -> 8px, the last 24px
 *             needed to fit four tabs in a 312px strip.
 *
 * The boundaries are measured, not chosen: the action group is 191px folded
 * (row needs 524px), 274px full (607px), and 301px with absurd badge counts
 * like "199/199" + "999" (634px).
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

/** How much slack the fit has, at minimum, before a tab starts to clip.
 *  20px is the floor: CI's fonts render these four labels 17.5% wider than a
 *  dev box's (measured — see the header comment), so a fit tuned to 0 on one
 *  machine overflows by 25px on the other. Anything below this is a design
 *  with no margin, not a passing test. */
const MIN_SLACK_PX = 20;

interface StripLayout {
	/** true when `.strip-actions` sits on its own row above the tablist. */
	split: boolean;
	/** every tab shares one offsetTop — the tablist itself never wraps. */
	tabsOnOneRow: boolean;
	/** the tablist is not overflowing: all four labels are actually shown. */
	tabsFit: boolean;
	/** Room to spare, in px, measured as strip width minus tablist CONTENT
	 *  width. NOT `clientWidth - scrollWidth`: `.pane-tabs` has no
	 *  `flex-grow`, so while the content fits its clientWidth IS its content
	 *  width and that difference is pinned at 0 — it looks like zero slack at
	 *  every width and cannot distinguish "just fits" from "loads of room".
	 *  Negative means overflowing. */
	slackPx: number;
	/** active tab's border-box bottom minus the strip's — 0 means it covers
	 *  the 1px divider exactly, which is the designed overlap. */
	underlineOffset: number;
	/** the action group must never become a child of the tablist (DR-4). */
	tablistContainsActions: boolean;
	/** Diagnostic detail, printed in assertion messages so a failure on a
	 *  machine we cannot attach a debugger to still hands over the numbers. */
	diag: string;
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
		const cs = getComputedStyle(tabEls[0]);
		return {
			split: ar.bottom <= tr.top + 1,
			tabsOnOneRow:
				new Set(tabEls.map((t) => Math.round(t.getBoundingClientRect().top))).size === 1,
			tabsFit: tabs.scrollWidth <= tabs.clientWidth,
			slackPx: Math.round(sr.width - tabs.scrollWidth),
			underlineOffset: active
				? Math.round(active.getBoundingClientRect().bottom - sr.bottom)
				: NaN,
			tablistContainsActions: tabs.contains(acts),
			diag:
				`strip=${sr.width.toFixed(1)} content=${tabs.scrollWidth} port=${tabs.clientWidth}` +
				` font="${cs.fontFamily}" @${cs.fontSize}/${cs.fontWeight}` +
				` tabs=${JSON.stringify(
					tabEls.map((t) => [
						(t.textContent || '').trim(),
						+t.getBoundingClientRect().width.toFixed(1),
					]),
				)}`,
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

		// ── SHARED ROW. 892px of strip at this viewport, way over 640. ──
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

		// The whole point: the four tabs FIT, not "are reachable by scrolling".
		//
		// And they must fit with REAL MARGIN, which is why the slack is asserted
		// rather than just `tabsFit`. The first version of this test asserted only
		// `tabsFit`, passed locally with exactly 0px to spare, and failed on CI by
		// 25px: `--font-ui` is `-apple-system, BlinkMacSystemFont, 'Segoe UI',
		// system-ui, sans-serif`, every entry of which misses on Linux, so
		// `system-ui` resolves through fontconfig to different font FILES on a dev
		// box and on ubuntu-latest — the same declaration, 17.5% wider glyphs.
		// A fit tuned until the numbers meet is not a fit; it is a coin flip on
		// the next environment, font, zoom level or translation.
		//
		// Both messages print the measured widths and the resolved font, so a
		// failure on a machine nobody can attach to still hands over the numbers
		// instead of `true !== false`.
		expect(layout.tabsFit, `tabs must fit without scrolling — ${layout.diag}`).toBe(true);
		expect(
			layout.slackPx,
			`fit must keep >=${MIN_SLACK_PX}px of slack so a wider font cannot clip a tab — ${layout.diag}`,
		).toBeGreaterThanOrEqual(MIN_SLACK_PX);

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
		//
		// Identified by ACCESSIBLE NAME, not `textContent`. At this width the
		// Relationships tab carries both labels in the DOM — the full one for
		// the accessible name, the short `Links` one visible — so `textContent`
		// reads "RelationshipsLinks". The accessible name is the identity that
		// matters and the thing ~5 other specs address these tabs by.
		await pane.locator('.pane-tab').first().focus();
		const walk: string[] = [];
		for (let i = 0; i < 4; i++) {
			walk.push(
				await page.evaluate(() => {
					const a = document.activeElement;
					return (a?.getAttribute('aria-label') || a?.textContent || '').trim();
				}),
			);
			await page.keyboard.press('ArrowRight');
		}
		expect(walk).toEqual(['Details', 'Relationships', 'Activity', 'Versions']);

		// The short label is VISIBLE text only — the accessible name never
		// changes, at any width. This is the guarantee the ~5 specs using
		// `getByRole('tab', { name: 'Relationships' })` depend on.
		await expect(pane.getByRole('tab', { name: 'Relationships' })).toHaveCount(1);
		await expect(pane.getByRole('tab', { name: 'Links' })).toHaveCount(0);
		await expect(pane.locator('.pane-tabs .tab-label-short')).toBeVisible();
		await expect(pane.locator('.pane-tabs .tab-label-full')).toBeHidden();

		// The ⋯ panel is anchored inside `.strip-actions`. Nothing on
		// `.tab-strip` clips it — the "no overflow/contain on the container"
		// constraint still holds at the narrowest width, where a clip would be
		// most visible.
		await pane.locator('button.pane-more-btn').click();
		await expect(pane.getByRole('menuitem', { name: 'Move to collection…' })).toBeVisible();
		await page.keyboard.press('Escape');
	});

	test('the FOLD band (560-639px) folds the badge icons + star, and the Star affordance moves into the ⋯ menu — exactly one of the two exists at any width', async ({
		page,
		fixture,
		request,
	}) => {
		// A 640px pane is a 592px strip: inside the fold band. The band is only
		// reachable by dragging the pane to 608-687px, so it has to be set up
		// rather than stumbled into.
		await page.addInitScript(() => localStorage.setItem('pad-pane-width', '640'));
		await page.setViewportSize({ width: 1800, height: 1000 });
		await browserLogin(page);

		const master = await seedDocWith(fixture, request, 'Tier fold');
		await seedDocWith(fixture, request, 'Tier fold child', { parent: master.id });

		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/docs?item=${master.slug}`);
		const pane = page.locator('.item-pane');
		await expect(pane.locator('.tab-strip')).toBeVisible();
		await expect(pane.locator('.strip-actions .badge-count').first()).toBeVisible();

		const width = await stripWidth(pane);
		expect(width).toBeGreaterThanOrEqual(560);
		expect(width).toBeLessThan(640);

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

		// ...and the menu row takes over, gated by the SAME query. The folded
		// star must have exactly one home: here.
		await pane.locator('button.pane-more-btn').click();
		await expect(page.locator('.strip-star-row')).toBeVisible();
		await pane.getByRole('menuitem', { name: 'Star' }).click();
		await pane.locator('button.pane-more-btn').click();
		await expect(pane.getByRole('menuitem', { name: 'Unstar' })).toBeVisible();
		await page.keyboard.press('Escape');
	});

	test('overflow degrades VISIBLY: with the tabs fitting there is no mask at all, and if they ever do overflow a fade appears only on the edge that has more content', async ({
		page,
		fixture,
		request,
	}) => {
		// The fit now carries 20px+ of slack, so overflow needs a font far wider
		// than any we have measured. That makes this a fallback — but a
		// load-bearing one: `.pane-tabs` scrolls with `scrollbar-width: none`,
		// and a silently truncated tablist is precisely the defect this whole
		// task exists to fix (two of four tabs invisible at 430px, unhinted).
		// So overflow must be made visible, and the affordance must NOT appear
		// when everything fits.
		await page.setViewportSize({ width: 1440, height: 1000 });
		await page.addInitScript(() => localStorage.setItem('pad-pane-width', '360'));
		await browserLogin(page);
		const master = await seedDocWith(fixture, request, 'Tier overflow');
		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/docs?item=${master.slug}`);
		const pane = page.locator('.item-pane');
		const tabs = pane.locator('.pane-tabs');
		await expect(pane.locator('.tab-strip')).toBeVisible();

		// ── FITTING: no mask, both edges crisp. ──
		expect((await stripLayout(pane)).tabsFit).toBe(true);
		await expect(tabs).not.toHaveClass(/masked/);

		// ── Force an overflow the only way that is environment-independent:
		//    inflate the tab font well past anything real. ──
		await page.addStyleTag({ content: '.pane-tab{font-size:1.75em !important;}' });
		await expect(tabs).toHaveClass(/masked/);

		const fadeAt = () =>
			tabs.evaluate((el) => ({
				left: getComputedStyle(el).getPropertyValue('--fade-l').trim(),
				right: getComputedStyle(el).getPropertyValue('--fade-r').trim(),
				scrollLeft: Math.round(el.scrollLeft),
				max: el.scrollWidth - el.clientWidth,
			}));

		// At the start there is content to the RIGHT only.
		await expect.poll(async () => (await fadeAt()).right).not.toBe('0px');
		expect((await fadeAt()).left).toBe('0px');

		// Scrolled to the end: content to the LEFT only.
		await tabs.evaluate((el) => (el.scrollLeft = el.scrollWidth));
		await expect.poll(async () => (await fadeAt()).left).not.toBe('0px');
		expect((await fadeAt()).right).toBe('0px');

		// Mid-scroll: content BOTH ways, so both edges fade.
		await tabs.evaluate((el) => (el.scrollLeft = Math.round((el.scrollWidth - el.clientWidth) / 2)));
		await expect.poll(async () => (await fadeAt()).left).not.toBe('0px');
		expect((await fadeAt()).right).not.toBe('0px');

		// The mask is a PAINT effect: the active tab still overlaps the strip's
		// 1px divider exactly as it does with no mask.
		expect((await stripLayout(pane)).underlineOffset).toBe(0);
	});

	test('the tier boundaries have no sub-pixel gap: a fractional strip width either side of 560 and 640 still lands in the right tier', async ({
		page,
		fixture,
		request,
	}) => {
		// Container widths are NOT integers — the docked pane's default is
		// `clamp(360px, 38%, 640px)` and PaneHost's JS floors are `usable * 0.4`
		// — so a strip can genuinely measure 559.5px. Encoded as three adjacent
		// integer bands (`max-width: 559` / `560..639` / `640+`) that width
		// matches NOTHING and silently falls back to the full shared row, which
		// needs 607px. The fix is nested ranges plus `.99` bounds; this is the
		// regression guard, and it needs fractional widths that no other test
		// here produces.
		await page.setViewportSize({ width: 1800, height: 1000 });
		await browserLogin(page);
		const master = await seedDocWith(fixture, request, 'Tier subpixel');
		await seedDocWith(fixture, request, 'Tier subpixel child', { parent: master.id });
		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/docs?item=${master.slug}`);
		const pane = page.locator('.item-pane');
		await expect(pane.locator('.tab-strip')).toBeVisible();

		// pane width - 48px of padding = strip width.
		const cases: Array<[number, 'split' | 'fold' | 'full']> = [
			[607.5, 'split'], // strip 559.50 — was in the gap
			[607.99, 'split'], // strip 559.99 — was in the gap
			[608.5, 'fold'], // strip 560.50
			[687.5, 'fold'], // strip 639.50 — was in the gap
			[687.99, 'fold'], // strip 639.99 — was in the gap
			[688.5, 'full'], // strip 640.50
		];

		for (const [paneWidth, expected] of cases) {
			await page.addStyleTag({ content: `.item-pane{flex:0 0 ${paneWidth}px !important;}` });
			const state = await expect
				.poll(
					async () =>
						pane.locator('.tab-strip').evaluate((strip) => {
							const tabs = strip.querySelector('.pane-tabs') as HTMLElement;
							const acts = strip.querySelector('.strip-actions') as HTMLElement;
							const split =
								acts.getBoundingClientRect().bottom <= tabs.getBoundingClientRect().top + 1;
							const starHidden =
								getComputedStyle(strip.querySelector('.star-btn') as HTMLElement).display ===
								'none';
							return `${split ? 'split' : starHidden ? 'fold' : 'full'}|${
								tabs.scrollWidth <= tabs.clientWidth
							}`;
						}),
					{ timeout: 3000 },
				)
				.toBe(`${expected}|true`)
				.then(() => expected);
			expect(state).toBe(expected);
		}
	});

	test('the Star affordance is never duplicated and never absent: the ⋯ menu row appears ONLY where the chip is folded', async ({
		page,
		fixture,
		request,
	}) => {
		await browserLogin(page);
		const master = await seedDocWith(fixture, request, 'Tier symmetry');
		await seedDocWith(fixture, request, 'Tier symmetry child', { parent: master.id });
		const paneUrl = `/${fixture.adminUsername}/${fixture.workspaceSlug}/docs?item=${master.slug}`;

		// [label, viewport, stored pane width, expected tier]
		const widths: Array<[string, { width: number; height: number }, number, 'split' | 'fold' | 'full']> = [
			['360px pane (strip 312)', { width: 1440, height: 1000 }, 360, 'split'],
			['448px pane (strip 400)', { width: 1440, height: 1000 }, 448, 'split'],
			['620px pane (strip 572)', { width: 1600, height: 1000 }, 620, 'fold'],
			['720px pane (strip 672)', { width: 2000, height: 1000 }, 720, 'full'],
		];

		for (const [label, viewport, paneWidth, tier] of widths) {
			await page.setViewportSize(viewport);
			await page.addInitScript((w) => localStorage.setItem('pad-pane-width', String(w)), paneWidth);
			await page.goto(paneUrl);
			const pane = page.locator('.item-pane');
			await expect(pane.locator('.tab-strip')).toBeVisible();

			const chip = pane.locator('button.star-btn');
			const row = page.locator('.strip-star-row');
			// Open the ⋯ so the row is mounted and measurable.
			await pane.locator('button.pane-more-btn').click();
			await expect(row).toHaveCount(1); // always in the DOM, gated by CSS only

			if (tier === 'fold') {
				await expect(chip, label).toBeHidden();
				await expect(row, label).toBeVisible();
			} else {
				await expect(chip, label).toBeVisible();
				await expect(row, label).toBeHidden();
			}
			await page.keyboard.press('Escape');
		}
	});
});
