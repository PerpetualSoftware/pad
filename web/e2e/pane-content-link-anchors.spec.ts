import { test, expect } from './fixtures';
import { browserLogin, seedDoc } from './lib/collab-helpers';
import type { APIRequestContext, Page } from '@playwright/test';
import type { SuiteFixture } from './fixtures';

/**
 * Anchor-based content-link interception (PLAN-2154 Architecture B.1/B.2/B.4
 * / TASK-2159).
 *
 * TASK-2158 wired the `onOpenTarget`/`fireOpenTarget` seam and the
 * collection host's resolve→drill path (`handleOpenTarget` →
 * `resolvePaneTarget` → `navigatePaneTo`), but shipped with no UI caller.
 * This task adds the callers: the relationships `<a class="link-target">`
 * in ItemDetail, `ChildItems`' `.child-row` + recursive `NestedChildren`'s
 * `.nested-link`, and `ItemGraph`'s node "Open" anchors (NOT the node click,
 * which stays select-only). A plain click on any of them must re-target the
 * pane IN PLACE (URL `?item=` changes, pathname unchanged, no full
 * navigation); a modifier-click must fall through to the anchor's plain
 * `href` and open the full page normally (the interceptor must never call
 * `preventDefault()` on a click it isn't going to handle).
 *
 * `?item=` values in this app are REF-preferred (`resolvePaneTarget`'s
 * ref > slug > href-segment order, mirroring `itemUrlId`/`formatItemRef`)
 * — every PaneTarget built by the interceptors under test carries a `ref`,
 * so assertions below compare against the item's PREFIX-NUMBER ref, not its
 * slug.
 *
 * The modifier bail-out itself (button / meta / ctrl / shift / alt /
 * defaultPrevented) is exhaustively unit-tested against the SHARED
 * predicate every surface here reuses — `shouldOpenInPane`
 * (itemCardClick.test.ts). This spec verifies the WIRING: that each new
 * onclick handler actually calls that predicate before intercepting, in a
 * real browser (ctrl-click's "open a new background tab" behavior has no
 * jsdom equivalent).
 *
 * Viewport is driven explicitly (desktop split), so one project is enough.
 */

const DESKTOP = { width: 1200, height: 900 };

function docsUrl(fixture: SuiteFixture, query = ''): string {
	return `/${fixture.adminUsername}/${fixture.workspaceSlug}/docs${query}`;
}

function openItemParam(page: Page): string | null {
	return new URL(page.url()).searchParams.get('item');
}

function pathname(page: Page): string {
	return new URL(page.url()).pathname;
}

function authHeaders(fixture: SuiteFixture) {
	return { Authorization: `Bearer ${fixture.apiToken}`, 'Content-Type': 'application/json' };
}

interface SeededItem {
	id: string;
	slug: string;
}

/** Seed a doc with `fields.parent` set — a child of `parentId` (mirrors
 *  `ChildItems.submitCreate`'s `fields: { parent }` shape). */
async function seedChildDoc(
	fixture: SuiteFixture,
	request: APIRequestContext,
	titlePrefix: string,
	parentId: string,
): Promise<SeededItem> {
	const resp = await request.post(
		`/api/v1/workspaces/${fixture.workspaceSlug}/collections/docs/items`,
		{
			headers: authHeaders(fixture),
			data: {
				title: `${titlePrefix} ${Date.now()}`,
				fields: JSON.stringify({ parent: parentId }),
				content: '',
			},
		},
	);
	if (!resp.ok()) throw new Error(`child doc create failed (${resp.status()}): ${await resp.text()}`);
	return (await resp.json()) as SeededItem;
}

/** Create a `related` link FROM `sourceSlug` TO `targetId` — surfaces under
 *  the "Related" relationship group on both ends. */
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

/** The PREFIX-NUMBER ref (e.g. "DOC-5") the app renders for an item —
 *  computed the same way `formatItemRef` does, client-side. */
async function itemRef(
	fixture: SuiteFixture,
	request: APIRequestContext,
	slug: string,
): Promise<string> {
	const resp = await request.get(`/api/v1/workspaces/${fixture.workspaceSlug}/items/${slug}`, {
		headers: authHeaders(fixture),
	});
	if (!resp.ok()) throw new Error(`item get failed (${resp.status()}): ${await resp.text()}`);
	const item = (await resp.json()) as { collection_prefix?: string; item_number?: number };
	if (!item.collection_prefix || !item.item_number) throw new Error('item has no ref');
	return `${item.collection_prefix}-${item.item_number}`;
}

/** Wait for a ctrl/cmd-clicked popup to actually navigate to the full-page
 *  item route ending in `idSegment` (a ref OR a slug — hrefs are built with
 *  whichever the surface prefers; `ChildItems` uses slugs, relationships and
 *  the graph use refs). Chromium briefly reports the new tab's url() as
 *  "about:blank" before the real navigation commits, so `waitForLoadState`
 *  alone can race it — wait on the URL itself instead. */
async function waitForItemPopup(popup: Page, idSegment: string): Promise<void> {
	await popup.waitForURL((url) => url.pathname.endsWith(`/docs/${idSegment}`));
}

/** Locate an `.item-card` by its OWN title, scoped to `.card-title` — NOT a
 *  bare `hasText` on the whole card, which also matches substring hits in
 *  `.card-meta`'s parent breadcrumb (a child's card renders "· PARENT-REF:
 *  Parent Title" there, so a parent's exact title can spuriously match its
 *  OWN child's card first in DOM order). */
function itemCard(page: Page, title: string) {
	return page.locator('.item-card').filter({ has: page.locator('.card-title', { hasText: title }) });
}

test.describe('content-link anchor interception (PLAN-2154 / TASK-2159)', () => {
	test.beforeEach(({}, testInfo) => {
		test.skip(
			testInfo.project.name !== 'desktop-chromium',
			'the pane is a desktop-split concern; one project is enough',
		);
	});

	test('Children: clicking a child row drills the pane in place; ctrl-click still opens the full page', async ({
		page,
		fixture,
		request,
	}) => {
		await page.setViewportSize(DESKTOP);
		await browserLogin(page);
		const parent = await seedDoc(fixture, request, 'Anchors children parent');
		const child = await seedChildDoc(fixture, request, 'Anchors children kid', parent.id);
		const childRef = await itemRef(fixture, request, child.slug);
		await page.goto(docsUrl(fixture));

		await itemCard(page, 'Anchors children parent').first().click();
		const pane = page.locator('.item-pane');
		await expect(pane).toBeVisible();
		await expect.poll(() => openItemParam(page)).not.toBeNull();
		const paneUrlAtParent = page.url();

		const childRow = pane.locator('.child-row', { hasText: 'Anchors children kid' });
		await expect(childRow).toBeVisible();

		// Plain click: re-targets the pane IN PLACE — same pathname, `?item=`
		// swaps to the child's ref, and the pane header now shows the child's
		// title. No full navigation (the collection list stays mounted
		// underneath).
		await childRow.click();
		await expect.poll(() => openItemParam(page)).toBe(childRef);
		expect(pathname(page)).toBe(new URL(paneUrlAtParent).pathname);
		await expect(pane.locator('.title', { hasText: /Anchors children kid/ })).toBeVisible();
		await expect(itemCard(page, 'Anchors children parent').first()).toBeVisible();

		// Back to the parent, then ctrl-click the SAME child row: the interceptor
		// must NOT call preventDefault for a modifier click, so the browser's
		// native "open link in a new background tab" behavior fires instead —
		// the pane's own `?item=` must stay put (proving our handler bailed).
		await page.goto(paneUrlAtParent);
		await expect(pane).toBeVisible();
		await expect(childRow).toBeVisible();
		const [popup] = await Promise.all([
			page.context().waitForEvent('page'),
			childRow.click({ modifiers: ['ControlOrMeta'] }),
		]);
		await waitForItemPopup(popup, child.slug);
		await popup.close();
		expect(openItemParam(page)).toBe(new URL(paneUrlAtParent).searchParams.get('item'));
	});

	test('Relationships: clicking a linked item drills the pane in place; ctrl-click still opens the full page', async ({
		page,
		fixture,
		request,
	}) => {
		await page.setViewportSize(DESKTOP);
		await browserLogin(page);
		const a = await seedDoc(fixture, request, 'Anchors rel alpha');
		const b = await seedDoc(fixture, request, 'Anchors rel bravo');
		const bRef = await itemRef(fixture, request, b.slug);
		await seedRelatedLink(fixture, request, a.slug, b.id);
		await page.goto(docsUrl(fixture));

		await itemCard(page, 'Anchors rel alpha').first().click();
		const pane = page.locator('.item-pane');
		await expect(pane).toBeVisible();
		const paneUrlAtA = page.url();

		const relLink = pane.locator('.link-target', { hasText: 'Anchors rel bravo' });
		await expect(relLink).toBeVisible();

		await relLink.click();
		await expect.poll(() => openItemParam(page)).toBe(bRef);
		await expect(pane.locator('.title', { hasText: /Anchors rel bravo/ })).toBeVisible();
		expect(pathname(page)).toBe(new URL(paneUrlAtA).pathname);

		// Ctrl-click falls through to the plain `href` (full-page popout); the
		// pane's own `?item=` is untouched.
		await page.goto(paneUrlAtA);
		await expect(pane).toBeVisible();
		await expect(relLink).toBeVisible();
		const [popup] = await Promise.all([
			page.context().waitForEvent('page'),
			relLink.click({ modifiers: ['ControlOrMeta'] }),
		]);
		await waitForItemPopup(popup, bRef);
		await popup.close();
		expect(openItemParam(page)).toBe(new URL(paneUrlAtA).searchParams.get('item'));
	});

	test('Graph: clicking a node then its "Open item" anchor drills the pane in place (node click itself stays select-only)', async ({
		page,
		fixture,
		request,
	}) => {
		await page.setViewportSize(DESKTOP);
		await browserLogin(page);
		const parent = await seedDoc(fixture, request, 'Anchors graph parent');
		const child = await seedChildDoc(fixture, request, 'Anchors graph kid', parent.id);
		const childRef = await itemRef(fixture, request, child.slug);
		await page.goto(docsUrl(fixture));

		await itemCard(page, 'Anchors graph parent').first().click();
		const pane = page.locator('.item-pane');
		await expect(pane).toBeVisible();
		const paneUrlAtParent = page.url();

		await pane.locator('button[title="View this item\'s dependency graph"]').click();
		const drawer = pane.locator('.graph-drawer');
		await expect(drawer).toBeVisible();
		const childNode = drawer.locator('.node', { hasText: childRef });
		await expect(childNode).toBeVisible();

		// A plain click on the NODE selects it (opens the detail card) but does
		// NOT navigate — the pane must still be showing the parent.
		await childNode.click();
		const detailCard = drawer.locator('.detail-card');
		await expect(detailCard).toBeVisible();
		expect(openItemParam(page)).toBe(new URL(paneUrlAtParent).searchParams.get('item'));

		// The detail card's "Open item ↗" anchor is the actual interception
		// point (Architecture B.4) — clicking IT drills the pane to the child.
		await detailCard.locator('.open-btn', { hasText: 'Open item' }).click();
		await expect.poll(() => openItemParam(page)).toBe(childRef);
		await expect(pane.locator('.title', { hasText: /Anchors graph kid/ })).toBeVisible();
	});

	test('Graph: ctrl-click on an "Open" anchor still opens the full page instead of drilling', async ({
		page,
		fixture,
		request,
	}) => {
		await page.setViewportSize(DESKTOP);
		await browserLogin(page);
		const parent = await seedDoc(fixture, request, 'Anchors graph ctrl parent');
		const parentRef = await itemRef(fixture, request, parent.slug);
		await page.goto(docsUrl(fixture));

		await itemCard(page, 'Anchors graph ctrl parent').first().click();
		const pane = page.locator('.item-pane');
		await expect(pane).toBeVisible();
		const paneUrlAtParent = page.url();

		await pane.locator('button[title="View this item\'s dependency graph"]').click();
		const drawer = pane.locator('.graph-drawer');
		await expect(drawer).toBeVisible();

		// The controls-bar "Open ↗" anchor targets the currently-focused node
		// (the origin item itself here) — a modifier click must still fall
		// through to its plain `href` rather than being swallowed by the
		// interceptor (which would otherwise preventDefault + no-op on the
		// same-item guard, dropping the click entirely).
		const openBtn = drawer.locator('.controls .open-btn', { hasText: 'Open' });
		await expect(openBtn).toBeVisible();
		const [popup] = await Promise.all([
			page.context().waitForEvent('page'),
			openBtn.click({ modifiers: ['ControlOrMeta'] }),
		]);
		await waitForItemPopup(popup, parentRef);
		await popup.close();
		// The pane itself never navigated away.
		expect(openItemParam(page)).toBe(new URL(paneUrlAtParent).searchParams.get('item'));
	});
});

/**
 * Focus per hop (PLAN-2154 Architecture C / R1, TASK-2162).
 *
 * Each in-pane hop (a content-link DRILL or an in-pane BACK) changes `?item=`,
 * which remounts ItemDetail's `{#key itemSlug}` subtrees and destroys the very
 * link/row the user just activated. `keepFocus` then has nothing to restore, so
 * focus falls to `<body>` — where the next `j`/`k` runs list-nav
 * (`handlePageKeydown`'s `.item-pane` bail doesn't match `<body>`) and could
 * laterally re-target the drilled `?item=`. The host moves focus onto the
 * STABLE aria-labeled `<aside>` region synchronously at each hop
 * (`focusPaneRegion`), backed by a desktop `focusin` net, so focus stays inside
 * `.item-pane` across the remount and `j`/`k` stay inert.
 *
 * These drive REAL content-link clicks (and a keyboard activation) so the
 * anchor-removed-by-remount path is exercised end to end — `pane-controller`'s
 * `drillTo` test hook calls `navigatePaneTo` directly and can't reproduce it.
 * The editor content-body link path (EditorLinkPopover, which additionally
 * hides its popover — removing the focused anchor — synchronously) funnels
 * through the SAME `navigatePaneTo` → `focusPaneRegion` chokepoint these cover,
 * with the synchronous-removal case caught by the same desktop `focusin` net.
 */
function paneHasFocus(page: Page): Promise<boolean> {
	return page.evaluate(() => {
		const active = document.activeElement;
		const pane = document.querySelector('.item-pane');
		return !!active && !!pane && pane.contains(active);
	});
}

test.describe('focus per hop (PLAN-2154 / TASK-2162)', () => {
	test.beforeEach(({}, testInfo) => {
		test.skip(
			testInfo.project.name !== 'desktop-chromium',
			'focus-per-hop is a desktop-split concern; the mobile overlay runs its own focus trap',
		);
	});

	// Gate GET fetches for one item ref so a hop's DESTINATION stays on the
	// minimal (loading) pane header — no content, no editor. That isolates
	// `focusPaneRegion` as the ONLY thing that could have put focus in the pane
	// during the load window (ItemDetail's editor autofocuses once the item
	// loads, which would otherwise mask whether the synchronous hop focus fired
	// at all). Returns a `release` to drain the fetch afterward.
	async function gateItemFetch(page: Page, ref: string): Promise<() => void> {
		let release: () => void = () => {};
		const gate = new Promise<void>((resolve) => {
			release = resolve;
		});
		await page.route(`**/api/v1/workspaces/*/items/${ref}`, async (route) => {
			if (route.request().method() !== 'GET') {
				await route.continue();
				return;
			}
			await gate;
			await route.continue();
		});
		return release;
	}

	test('a mouse content-link drill lands focus inside the pane (while loading), and the next j does not steal to list-nav', async ({
		page,
		fixture,
		request,
	}) => {
		await page.setViewportSize(DESKTOP);
		await browserLogin(page);
		const a = await seedDoc(fixture, request, 'Focus hop rel alpha');
		const b = await seedDoc(fixture, request, 'Focus hop rel bravo');
		const bRef = await itemRef(fixture, request, b.slug);
		await seedRelatedLink(fixture, request, a.slug, b.id);
		await page.goto(docsUrl(fixture));

		await itemCard(page, 'Focus hop rel alpha').first().click();
		const pane = page.locator('.item-pane');
		await expect(pane).toBeVisible();

		// Gate B so the drill's destination stalls on the loading header — the
		// editor never mounts, so focus in the pane can only be `focusPaneRegion`.
		const releaseB = await gateItemFetch(page, bRef);

		// DRILL A→B by clicking the related-item link. Its anchor lives inside a
		// `{#key itemSlug}` subtree that this very drill remounts — the exact R1
		// "clicked link destroyed, focus falls to <body>" case.
		const relLink = pane.locator('.link-target', { hasText: 'Focus hop rel bravo' });
		await expect(relLink).toBeVisible();
		await relLink.click();
		await expect.poll(() => openItemParam(page)).toBe(bRef);
		await expect(pane.locator('.pane-header--minimal')).toBeVisible();

		// R1: focus is INSIDE the pane even mid-load, purely from the hop's
		// synchronous region focus — not dropped to <body>.
		await expect.poll(() => paneHasFocus(page)).toBe(true);

		// The next `j` is inert (focus in the pane → the keydown handler bails),
		// so the drilled `?item=` is untouched — no lateral list re-target.
		await page.keyboard.press('j');
		await page.waitForTimeout(250); // outlast the pane-follow debounce
		expect(openItemParam(page)).toBe(bRef);
		await expect.poll(() => paneHasFocus(page)).toBe(true);

		releaseB();
		await expect(pane.locator('.title', { hasText: /Focus hop rel bravo/ })).toBeVisible();
	});

	test('a keyboard content-link drill (focus the link, press Enter) also lands focus inside the pane while loading', async ({
		page,
		fixture,
		request,
	}) => {
		await page.setViewportSize(DESKTOP);
		await browserLogin(page);
		const a = await seedDoc(fixture, request, 'Focus hop kbd alpha');
		const b = await seedDoc(fixture, request, 'Focus hop kbd bravo');
		const bRef = await itemRef(fixture, request, b.slug);
		await seedRelatedLink(fixture, request, a.slug, b.id);
		await page.goto(docsUrl(fixture));

		await itemCard(page, 'Focus hop kbd alpha').first().click();
		const pane = page.locator('.item-pane');
		await expect(pane).toBeVisible();

		const releaseB = await gateItemFetch(page, bRef);

		const relLink = pane.locator('.link-target', { hasText: 'Focus hop kbd bravo' });
		await expect(relLink).toBeVisible();
		// KEYBOARD drill: focus the anchor and activate it with Enter (an <a>
		// fires its click handler on Enter). This is the keyboard analogue of
		// the editor-link path, where the focused anchor is removed as the drill
		// fires and focus must be recovered into the pane.
		await relLink.focus();
		await page.keyboard.press('Enter');
		await expect.poll(() => openItemParam(page)).toBe(bRef);
		await expect(pane.locator('.pane-header--minimal')).toBeVisible();
		await expect.poll(() => paneHasFocus(page)).toBe(true);

		releaseB();
		await expect(pane.locator('.title', { hasText: /Focus hop kbd bravo/ })).toBeVisible();
	});

	test('an in-pane Back keeps focus inside the pane (while the popped-to item loads)', async ({
		page,
		fixture,
		request,
	}) => {
		await page.setViewportSize(DESKTOP);
		await browserLogin(page);
		const a = await seedDoc(fixture, request, 'Focus hop back alpha');
		const b = await seedDoc(fixture, request, 'Focus hop back bravo');
		await seedRelatedLink(fixture, request, a.slug, b.id);
		await page.goto(docsUrl(fixture));

		await itemCard(page, 'Focus hop back alpha').first().click();
		const pane = page.locator('.item-pane');
		await expect(pane).toBeVisible();
		const aRef = openItemParam(page)!;

		const relLink = pane.locator('.link-target', { hasText: 'Focus hop back bravo' });
		await expect(relLink).toBeVisible();
		await relLink.click();
		await expect(pane.locator('.title', { hasText: /Focus hop back bravo/ })).toBeVisible();

		// Gate A (the Back destination) so the pop stalls on the loading header —
		// focus in the pane during that window is purely `focusPaneRegion`.
		const releaseA = await gateItemFetch(page, aRef);

		// In-pane Back (‹) pops B→A. The pop remounts the pane content and
		// unmounts the just-clicked Back button; focus must stay inside the pane.
		const backBtn = pane.locator('button[aria-label="Back"]');
		await expect(backBtn).toBeVisible();
		await backBtn.click();
		await expect.poll(() => openItemParam(page)).toBe(aRef);
		await expect(pane.locator('.pane-header--minimal')).toBeVisible();
		await expect.poll(() => paneHasFocus(page)).toBe(true);

		releaseA();
		await expect(pane.locator('.title', { hasText: /Focus hop back alpha/ })).toBeVisible();
	});
});
