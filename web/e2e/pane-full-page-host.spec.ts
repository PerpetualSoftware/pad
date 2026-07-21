import { test, expect } from './fixtures';
import { browserLogin, seedDoc } from './lib/collab-helpers';
import type { APIRequestContext, Page } from '@playwright/test';
import type { SuiteFixture } from './fixtures';

/**
 * Full-page pane HOST (PLAN-2154 Phase 2 / Architecture E, bullet 5 /
 * TASK-2174 — the Q1 payoff).
 *
 * The full-page item route (`[collection]/[slug]/+page.svelte`) now mounts the
 * SAME right-docked detail pane the collection page carries. From a full-page
 * item, clicking a child / related / wiki-linked item opens a navigable
 * mini-browser pane BESIDE the master (the `[slug]` PATH param) via the `?item=`
 * QUERY param — no collision. The master goes retain-alive READ-ONLY (peeking)
 * while the pane is open; drill / in-pane Back / browser Back / close all work;
 * and a `?item=` that resolves to the MASTER itself is refused (no second collab
 * provider on the master's own room) — on cold load it's stripped.
 *
 * This spec drives that host end-to-end in a real browser against the built
 * binary. The pane is a desktop-split concern, so one project is enough.
 */

const DESKTOP = { width: 1200, height: 900 };

function authHeaders(fixture: SuiteFixture) {
	return { Authorization: `Bearer ${fixture.apiToken}`, 'Content-Type': 'application/json' };
}

function openItemParam(page: Page): string | null {
	return new URL(page.url()).searchParams.get('item');
}

interface SeededItem {
	id: string;
	slug: string;
}

/** Seed a doc with real markdown body content — needed for hover tests where a
 *  concrete block must exist for the block drag handle to attach to. */
async function seedDocWithContent(
	fixture: SuiteFixture,
	request: APIRequestContext,
	titlePrefix: string,
	content: string,
): Promise<SeededItem> {
	const resp = await request.post(
		`/api/v1/workspaces/${fixture.workspaceSlug}/collections/docs/items`,
		{ headers: authHeaders(fixture), data: { title: `${titlePrefix} ${Date.now()}`, fields: '{}', content } },
	);
	if (!resp.ok()) throw new Error(`doc create failed (${resp.status()}): ${await resp.text()}`);
	return (await resp.json()) as SeededItem;
}

/** Seed a doc whose `fields.parent` makes it a child of `parentId` (mirrors
 *  `ChildItems.submitCreate`'s `{ parent }` shape) so the parent's pane renders
 *  a `.child-row` to drill into. */
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
			data: { title: `${titlePrefix} ${Date.now()}`, fields: JSON.stringify({ parent: parentId }), content: '' },
		},
	);
	if (!resp.ok()) throw new Error(`child doc create failed (${resp.status()}): ${await resp.text()}`);
	return (await resp.json()) as SeededItem;
}

/** A `related` link FROM `sourceSlug` TO `targetId` — surfaces under the
 *  "Related" relationship group on the source's page. */
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

/** The PREFIX-NUMBER ref (e.g. "DOC-5") the app renders + puts in `?item=`. */
async function itemRef(fixture: SuiteFixture, request: APIRequestContext, slug: string): Promise<string> {
	const resp = await request.get(`/api/v1/workspaces/${fixture.workspaceSlug}/items/${slug}`, {
		headers: authHeaders(fixture),
	});
	if (!resp.ok()) throw new Error(`item get failed (${resp.status()}): ${await resp.text()}`);
	const item = (await resp.json()) as { collection_prefix?: string; item_number?: number };
	if (!item.collection_prefix || !item.item_number) throw new Error('item has no ref');
	return `${item.collection_prefix}-${item.item_number}`;
}

function fullPageUrl(fixture: SuiteFixture, slug: string, query = ''): string {
	return `/${fixture.adminUsername}/${fixture.workspaceSlug}/docs/${slug}${query}`;
}

test.describe('full-page pane host (PLAN-2154 Phase 2 / TASK-2174)', () => {
	test.beforeEach(({}, testInfo) => {
		test.skip(
			testInfo.project.name !== 'desktop-chromium',
			'the pane is a desktop-split concern; one project is enough',
		);
	});

	test('focus follows editing is INVISIBLE (BUG-2263): open keeps the master editable (pane = preview); clicking a side activates it and freezes the other; the frozen side keeps its editable title — only the content editor stops being typeable (PLAN-2179 DR-2/DR-3 / TASK-2181)', async ({
		page,
		fixture,
		request,
	}) => {
		await page.setViewportSize(DESKTOP);
		await browserLogin(page);

		// A (master) --related--> B (pane target); C is a CHILD of B (the drill
		// target once B is in the pane).
		const master = await seedDoc(fixture, request, 'FP host master');
		const related = await seedDoc(fixture, request, 'FP host related');
		const grandchild = await seedChildDoc(fixture, request, 'FP host grandchild', related.id);
		await seedRelatedLink(fixture, request, master.slug, related.id);
		const relatedRef = await itemRef(fixture, request, related.slug);
		const grandchildRef = await itemRef(fixture, request, grandchild.slug);

		// BUG-2263: the freeze is INVISIBLE — the title stays a click-to-edit
		// `button.title` on BOTH sides, so it is NO LONGER a peeking probe. Which
		// side is EDITABLE is signalled by its CONTENT editor's `contenteditable`.
		const masterTitleBtn = page.locator('.item-page-host > .item-page button.title', { hasText: 'FP host master' });
		const masterEditor = page.locator('.item-page-host > .item-page .editor-wrapper .ProseMirror');

		// Land on the MASTER full page. No pane yet: the flex-row host is present,
		// the master title is an editable button, its editor is typeable, no `?item=`.
		await page.goto(fullPageUrl(fixture, master.slug));
		await expect(page.locator('.item-page-host')).toBeVisible();
		await expect(masterTitleBtn).toBeVisible();
		await expect(masterEditor).toHaveAttribute('contenteditable', 'true');
		await expect(page.locator('.item-pane')).toHaveCount(0);
		expect(openItemParam(page)).toBeNull();

		// Click the RELATED link on the master → FIRST-OPEN the pane beside it.
		await page
			.locator('.relationship-group', { hasText: 'Related' })
			.locator('a.link-target', { hasText: 'FP host related' })
			.click();

		const pane = page.locator('.item-pane');
		await expect(pane).toBeVisible();
		await expect.poll(() => openItemParam(page)).toBe(relatedRef);
		const paneEditor = pane.locator('.editor-wrapper .ProseMirror');

		// DR-2: opening does NOT freeze the master — it stays the active/editable
		// side. The pane opens as a PREVIEW, but INVISIBLY (BUG-2263): its title is
		// STILL an editable button (not a degraded <h1>); only its content editor is
		// not typeable (contenteditable=false).
		await expect(masterEditor).toHaveAttribute('contenteditable', 'true');
		await expect(pane.locator('button.title', { hasText: 'FP host related' })).toBeVisible();
		await expect(pane.locator('h1.title.title-readonly')).toHaveCount(0);
		await expect(paneEditor).toHaveAttribute('contenteditable', 'false');

		// Depth 0: the pane's Back chevron is hidden.
		await expect(pane.locator('button.pane-back-btn')).toHaveCount(0);

		// Drill a CHILD row directly from the preview — on the FIRST click. The drill
		// is pane-internal, so it ALSO activates the pane — pane editable, master
		// frozen (PLAN-2179 DR-2 / TASK-2181). No pre-activation click.
		const masterPathname = new URL(page.url()).pathname;
		await pane.locator('.child-row', { hasText: 'FP host grandchild' }).click();
		await expect.poll(() => openItemParam(page)).toBe(grandchildRef);
		expect(new URL(page.url()).pathname).toBe(masterPathname);
		await expect(pane.locator('button.pane-back-btn')).toBeVisible();
		await expect(pane.locator('button.title', { hasText: 'FP host grandchild' })).toBeVisible();
		// Pane active, master frozen — but the master's freeze is INVISIBLE: its
		// title is still an editable button; only its editor is not typeable.
		await expect(paneEditor).toHaveAttribute('contenteditable', 'true');
		await expect(masterEditor).toHaveAttribute('contenteditable', 'false');
		await expect(masterTitleBtn).toBeVisible();

		// Browser BACK → pops one drill level back to B in the pane. A drill-pop is
		// still pane-internal, so `activePane` stays 'pane' — pane editable, master
		// frozen.
		await page.goBack();
		await expect.poll(() => openItemParam(page)).toBe(relatedRef);
		await expect(pane.locator('button.title', { hasText: 'FP host related' })).toBeVisible();
		await expect(pane.locator('button.pane-back-btn')).toHaveCount(0);
		await expect(paneEditor).toHaveAttribute('contenteditable', 'true');
		await expect(masterEditor).toHaveAttribute('contenteditable', 'false');

		// Click BACK into the MASTER content editor → the pointerdown activator
		// re-activates the master (and the same click lands the caret in the now-
		// editable view — one gesture), and the desktop backstop must NOT yank focus
		// back to the pane. Master editable again; pane freezes. Exactly one side.
		await masterEditor.click();
		await expect(masterEditor).toHaveAttribute('contenteditable', 'true');
		await expect(paneEditor).toHaveAttribute('contenteditable', 'false');
		// The frozen pane's title stays an editable button (invisible freeze).
		await expect(pane.locator('button.title', { hasText: 'FP host related' })).toBeVisible();

		// Click into the PANE content editor → the pointerdown activator makes the
		// pane the active side; the master freezes.
		await paneEditor.click();
		await expect(paneEditor).toHaveAttribute('contenteditable', 'true');
		await expect(masterEditor).toHaveAttribute('contenteditable', 'false');

		// Close (✕) → the pane unmounts cleanly, `?item=` drops, and the master's
		// editor is typeable again.
		await pane.locator('button[title="Close pane"]').click();
		await expect(page.locator('.item-pane')).toHaveCount(0);
		await expect.poll(() => openItemParam(page)).toBeNull();
		await expect(masterEditor).toHaveAttribute('contenteditable', 'true');
	});

	test('opening AND closing the pane freezes/thaws the master WITHOUT remounting its editor (PLAN-2179 DR-1 / TASK-2180 — reactive freeze)', async ({
		page,
		fixture,
		request,
	}) => {
		await page.setViewportSize(DESKTOP);
		await browserLogin(page);

		// A (master) --related--> B.
		const master = await seedDoc(fixture, request, 'FP host reactive-freeze master');
		const related = await seedDoc(fixture, request, 'FP host reactive-freeze related');
		await seedRelatedLink(fixture, request, master.slug, related.id);
		const relatedRef = await itemRef(fixture, request, related.slug);

		await page.goto(fullPageUrl(fixture, master.slug));
		await expect(page.locator('button.title', { hasText: 'FP host reactive-freeze master' })).toBeVisible();

		// The MASTER's main content editor (its ProseMirror), scoped to the
		// master column (`.item-page-host > .item-page`, never the `.item-pane`).
		// Wait until it has synced to editable — the collab skeleton renders no
		// ProseMirror, so this also guarantees the real editor is mounted before
		// we capture a handle to its DOM node.
		const masterMainEditor = page.locator(
			'.item-page-host > .item-page .editor-wrapper .ProseMirror',
		);
		await expect(masterMainEditor).toHaveAttribute('contenteditable', 'true');
		const editorNode = await masterMainEditor.elementHandle();
		expect(editorNode).not.toBeNull();
		expect(await editorNode!.evaluate((el) => el.isConnected)).toBe(true);

		// Open the pane → focus-follows keeps the MASTER active (DR-2), so its editor
		// stays editable and the SAME DOM node stays connected — no open-driven remount.
		const pane = page.locator('.item-pane');
		await page
			.locator('.relationship-group', { hasText: 'Related' })
			.locator('a.link-target', { hasText: 'FP host reactive-freeze related' })
			.click();
		await expect(pane).toBeVisible();
		await expect.poll(() => openItemParam(page)).toBe(relatedRef);
		expect(await editorNode!.evaluate((el) => el.isConnected)).toBe(true);
		await expect(masterMainEditor).toHaveAttribute('contenteditable', 'true');

		// Click INTO the pane's content editor → the master FREEZES. The freeze is
		// REACTIVE: the SAME editor DOM node is still connected (a `{#key}`-driven
		// remount — the OLD peeking-in-the-key behavior — would have detached this
		// handle, isConnected → false), and it merely flipped contenteditable=false in
		// place. This is the whole point of PLAN-2179 DR-1: freeze without
		// destroying/recreating the editor.
		await pane.locator('.editor-wrapper .ProseMirror').click();
		expect(await editorNode!.evaluate((el) => el.isConnected)).toBe(true);
		await expect(masterMainEditor).toHaveAttribute('contenteditable', 'false');
		// BUG-2263 invisibility: the frozen master's title is STILL an editable
		// button — the freeze degrades nothing but the content editor's typeability.
		await expect(page.locator('.item-page-host > .item-page button.title', { hasText: 'FP host reactive-freeze master' })).toBeVisible();

		// Close the pane → the master thaws back to editable. The editor node
		// survives the un-freeze too (no remount on either edge), flipping
		// contenteditable=true again in place.
		await page.locator('button[title="Close pane"]').click();
		await expect(page.locator('.item-pane')).toHaveCount(0);
		await expect(page.locator('button.title', { hasText: 'FP host reactive-freeze master' })).toBeVisible();
		await expect(page.locator('h1.title.title-readonly')).toHaveCount(0);
		expect(await editorNode!.evaluate((el) => el.isConnected)).toBe(true);
		await expect(masterMainEditor).toHaveAttribute('contenteditable', 'true');
	});

	test('the master block drag handle appears on hover while editable, is frozen (never appears) while peeking, and returns on close (PLAN-2179 DR-1 / TASK-2180)', async ({
		page,
		fixture,
		request,
	}) => {
		await page.setViewportSize(DESKTOP);
		await browserLogin(page);

		const master = await seedDocWithContent(
			fixture,
			request,
			'FP host drag-handle master',
			'Alpha paragraph here\n\nBravo paragraph here',
		);
		const related = await seedDoc(fixture, request, 'FP host drag-handle related');
		await seedRelatedLink(fixture, request, master.slug, related.id);
		const relatedRef = await itemRef(fixture, request, related.slug);

		await page.goto(fullPageUrl(fixture, master.slug));
		const masterMain = page.locator('.item-page-host > .item-page .editor-wrapper .ProseMirror');
		await expect(masterMain).toHaveAttribute('contenteditable', 'true');
		await expect(masterMain.locator('p').first()).toContainText('Alpha paragraph');

		// The block drag handle lives inside the master's editor wrapper; it is
		// display:none until a hover reveals it over a block.
		const handle = page.locator('.item-page-host > .item-page .editor-wrapper .block-drag-handle');
		const handleDisplay = () =>
			handle.evaluate((el) => getComputedStyle(el as HTMLElement).display).catch(() => 'missing');

		// EDITABLE: hovering a paragraph reveals the handle.
		await masterMain.locator('p').first().hover();
		await expect.poll(handleDisplay, { timeout: 3000 }).not.toBe('none');

		// PEEK: open the pane (focus-follows keeps the master editable — DR-2), then
		// click INTO the pane → the master FREEZES (contenteditable=false). Hovering
		// the SAME paragraph must NOT reveal the handle — the reactive-editable choke
		// (onMouseMove/update bail on !editorView.editable) keeps it hidden.
		const pane = page.locator('.item-pane');
		await page
			.locator('.relationship-group', { hasText: 'Related' })
			.locator('a.link-target', { hasText: 'FP host drag-handle related' })
			.click();
		await expect(pane).toBeVisible();
		await expect.poll(() => openItemParam(page)).toBe(relatedRef);
		await pane.locator('.editor-wrapper .ProseMirror').click();
		await expect(masterMain).toHaveAttribute('contenteditable', 'false');
		await page.mouse.move(5, 5); // leave the editor first
		await masterMain.locator('p').first().hover({ force: true });
		await page.waitForTimeout(250);
		expect(await handleDisplay()).toBe('none');

		// CLOSE: the master thaws → hovering reveals the handle again.
		await page.locator('button[title="Close pane"]').click();
		await expect(page.locator('.item-pane')).toHaveCount(0);
		await expect(masterMain).toHaveAttribute('contenteditable', 'true');
		await page.mouse.move(5, 5);
		await masterMain.locator('p').first().hover();
		await expect.poll(handleDisplay, { timeout: 3000 }).not.toBe('none');
	});

	test('the Rich/Markdown mode toggle renders on the peeking side and flips in ONE gesture (BUG-2263 follow-up)', async ({
		page,
		fixture,
		request,
	}) => {
		await page.setViewportSize(DESKTOP);
		await browserLogin(page);

		const master = await seedDocWithContent(fixture, request, 'FP mode-toggle master', 'Master body paragraph.');
		const related = await seedDoc(fixture, request, 'FP mode-toggle related');
		await seedRelatedLink(fixture, request, master.slug, related.id);
		const relatedRef = await itemRef(fixture, request, related.slug);

		await page.goto(fullPageUrl(fixture, master.slug));
		const masterCol = page.locator('.item-page-host > .item-page');
		const masterEditor = masterCol.locator('.editor-wrapper .ProseMirror');
		const masterToggle = masterCol.locator('.editor-mode-toggle');
		await expect(masterEditor).toHaveAttribute('contenteditable', 'true');
		await expect(masterToggle).toBeVisible();

		// Open the pane → the master stays active (DR-2); the pane is a read-only
		// preview. NEW (BUG-2263 follow-up): the peeking PANE shows the mode toggle —
		// previously it was hidden on the peeking side.
		await page
			.locator('.relationship-group', { hasText: 'Related' })
			.locator('a.link-target', { hasText: 'FP mode-toggle related' })
			.click();
		const pane = page.locator('.item-pane');
		await expect(pane).toBeVisible();
		await expect.poll(() => openItemParam(page)).toBe(relatedRef);
		const paneEditor = pane.locator('.editor-wrapper .ProseMirror');
		await expect(pane.locator('.editor-mode-toggle')).toBeVisible();
		await expect(paneEditor).toHaveAttribute('contenteditable', 'false');

		// Click into the pane → the MASTER becomes the peeking side; its toggle stays.
		await paneEditor.click();
		await expect(masterEditor).toHaveAttribute('contenteditable', 'false');
		await expect(masterToggle).toBeVisible();

		// ONE GESTURE: click the peeking master's "Markdown" button. Its onclick bails
		// on `if (peeking) return`, so a successful flip PROVES the pointerdown
		// activator un-peeked the master FIRST, in the same gesture.
		await masterCol.locator('.editor-mode-toggle .mode-btn', { hasText: 'Markdown' }).click();
		await expect(masterCol.locator('.editor-mode-toggle .mode-btn', { hasText: 'Markdown' })).toHaveClass(/active/);
		// Master is now in raw markdown mode: the ProseMirror unmounted, and the pane
		// is the frozen side — exactly one typeable editor throughout.
		await expect(masterEditor).toHaveCount(0);
		await expect(paneEditor).toHaveAttribute('contenteditable', 'false');
	});

	test('a cold-loaded `?item=<the master itself>` is stripped — never mounts a pane on the master', async ({
		page,
		fixture,
		request,
	}) => {
		await page.setViewportSize(DESKTOP);
		await browserLogin(page);

		const master = await seedDoc(fixture, request, 'FP host self-ref');
		const masterRef = await itemRef(fixture, request, master.slug);

		// Hand-craft a `?item=` that aliases the MASTER's own ref. Two providers on
		// one collab room (sharing the itemID-only sessionStorage cursor) is
		// forbidden, so once the master's identity resolves the host strips `?item=`
		// in place — no pane mounts.
		await page.goto(fullPageUrl(fixture, master.slug, `?item=${masterRef}`));
		await expect(page.locator('button.title', { hasText: 'FP host self-ref' })).toBeVisible();
		// The pane must never appear, and `?item=` must be stripped from the URL.
		await expect(page.locator('.item-pane')).toHaveCount(0);
		await expect.poll(() => openItemParam(page)).toBeNull();
		// The master stays fully EDITABLE (never went peeking).
		await expect(page.locator('.item-page-host > .item-page .editor-wrapper .ProseMirror')).toHaveAttribute('contenteditable', 'true');
	});

	test('a cold-loaded shared `?item=<a different item>` still mounts the pane (the self-collision mount-gate does not suppress legitimate cross-item cold loads)', async ({
		page,
		fixture,
		request,
	}) => {
		await page.setViewportSize(DESKTOP);
		await browserLogin(page);

		const master = await seedDoc(fixture, request, 'FP host cold master');
		const other = await seedDoc(fixture, request, 'FP host cold other');
		const otherRef = await itemRef(fixture, request, other.slug);

		// Deep-link straight to the master with a shared `?item=` pointing at a
		// DIFFERENT item. The mount-gate holds the pane one master-load beat (so a
		// `?item=<master-alias>` can't transiently mint a 2nd provider), then mounts
		// it once identity confirms the target isn't the master.
		await page.goto(fullPageUrl(fixture, master.slug, `?item=${otherRef}`));
		const pane = page.locator('.item-pane');
		await expect(pane).toBeVisible();
		await expect(pane.locator('.title', { hasText: 'FP host cold other' })).toBeVisible();
		await expect.poll(() => openItemParam(page)).toBe(otherRef);
		// DR-2 cold-load initializer (desktop): no focusin fires on a `?item=` deep
		// load, so `activePane` seeds to the MASTER — it's EDITABLE beside the pane,
		// which opens as a PREVIEW. INVISIBLE freeze (BUG-2263): the pane's title is
		// an editable button; only its content editor is not typeable.
		await expect(page.locator('.item-page-host > .item-page button.title', { hasText: 'FP host cold master' })).toBeVisible();
		await expect(page.locator('.item-page-host > .item-page .editor-wrapper .ProseMirror')).toHaveAttribute('contenteditable', 'true');
		await expect(pane.locator('button.title', { hasText: 'FP host cold other' })).toBeVisible();
		await expect(pane.locator('.editor-wrapper .ProseMirror')).toHaveAttribute('contenteditable', 'false');
	});

	test('a cold-loaded `?item=<the master by its ref-shaped slug>` is stripped (server slug-fallback self-collision)', async ({
		page,
		fixture,
		request,
	}) => {
		await page.setViewportSize(DESKTOP);
		await browserLogin(page);

		// `seedDoc` titles are `<prefix> <Date.now()>`, and the server slugifies that
		// to `<prefix>-<digits>` — so a doc titled "plan" gets the REF-SHAPED slug
		// `plan-<timestamp>` while its own item number is small. `?item=plan-<ts>`
		// therefore parses as a ref at a NON-existent number; the server falls back
		// to the SLUG and resolves to THIS master. The ref-NUMBER channel alone
		// (timestamp != item number) would MISS this — the guard must also match the
		// raw slug string.
		const master = await seedDoc(fixture, request, 'plan');
		expect(master.slug).toMatch(/^[A-Za-z]+-\d+$/); // fixture assumption: ref-shaped slug
		const masterRef = await itemRef(fixture, request, master.slug);
		expect(master.slug).not.toBe(masterRef); // slug number != item ref number

		await page.goto(fullPageUrl(fixture, master.slug, `?item=${encodeURIComponent(master.slug)}`));
		await expect(page.locator('button.title')).toBeVisible();
		// Must be stripped — never mount a second provider on the master's own room.
		await expect(page.locator('.item-pane')).toHaveCount(0);
		await expect.poll(() => openItemParam(page)).toBeNull();
		await expect(page.locator('.item-page-host > .item-page .editor-wrapper .ProseMirror')).toHaveAttribute('contenteditable', 'true');
	});

	test('Expand a pane item to full page, then browser Back, restores the pane (no stale-master strip)', async ({
		page,
		fixture,
		request,
	}) => {
		await page.setViewportSize(DESKTOP);
		await browserLogin(page);

		// A (master) --related--> B.
		const master = await seedDoc(fixture, request, 'FP host expand master');
		const related = await seedDoc(fixture, request, 'FP host expand related');
		await seedRelatedLink(fixture, request, master.slug, related.id);
		const relatedRef = await itemRef(fixture, request, related.slug);

		await page.goto(fullPageUrl(fixture, master.slug));
		await page
			.locator('.relationship-group', { hasText: 'Related' })
			.locator('a.link-target', { hasText: 'FP host expand related' })
			.click();
		const pane = page.locator('.item-pane');
		await expect(pane).toBeVisible();
		await expect.poll(() => openItemParam(page)).toBe(relatedRef);
		const paneUrl = page.url();

		// Expand B to the full page (same route reused, master A -> B), dropping ?item=.
		await pane.locator('button.pane-header-btn[aria-label="Expand to full page"]').click();
		await expect(page.locator('.item-pane')).toHaveCount(0);
		await expect(page.locator('button.title', { hasText: 'FP host expand related' })).toBeVisible();
		await expect.poll(() => openItemParam(page)).toBeNull();

		// Browser Back -> `master?item=B`. The route is reused and `masterIdentity`
		// briefly still holds B; the fresh-for-`ref` gate must NOT let the strip
		// delete `?item=B`, so the pane RESTORES to B beside the (reloaded) A master.
		await page.goBack();
		await expect(page).toHaveURL(paneUrl);
		await expect(pane).toBeVisible();
		await expect.poll(() => openItemParam(page)).toBe(relatedRef);
		// Clicking "Expand to full page" is a click INSIDE the pane, so `activePane`
		// latched to 'pane' and PERSISTS across the expand + Back (this route
		// component is REUSED, never remounted). The restored pane is therefore the
		// ACTIVE/editable side and the master A is frozen — the pane "stays active"
		// across browser Back (PLAN-2179 DR-2). The freeze is INVISIBLE (BUG-2263):
		// the master's frozen state shows only on its content editor, not its title.
		await expect(pane.locator('button.title', { hasText: 'FP host expand related' })).toBeVisible();
		await expect(page.locator('.item-page-host > .item-page .editor-wrapper .ProseMirror')).toHaveAttribute('contenteditable', 'false');
	});
});
