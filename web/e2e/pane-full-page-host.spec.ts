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

	test('a master content-link opens a pane beside the (read-only, still-live) master; the pane drills, in-pane Back and ✕ close cleanly', async ({
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

		// Land on the MASTER full page. No pane yet: the flex-row host is present,
		// the master title is EDITABLE (a click-to-edit button — not peeking), and
		// there's no `?item=`.
		await page.goto(fullPageUrl(fixture, master.slug));
		await expect(page.locator('.item-page-host')).toBeVisible();
		await expect(page.locator('button.title', { hasText: 'FP host master' })).toBeVisible();
		await expect(page.locator('.item-pane')).toHaveCount(0);
		expect(openItemParam(page)).toBeNull();

		// Click the RELATED link on the master → FIRST-OPEN the pane beside it.
		await page
			.locator('.relationship-group', { hasText: 'Related' })
			.locator('a.link-target', { hasText: 'FP host related' })
			.click();

		const pane = page.locator('.item-pane');
		await expect(pane).toBeVisible();
		await expect(pane.locator('.title', { hasText: 'FP host related' })).toBeVisible();
		await expect.poll(() => openItemParam(page)).toBe(relatedRef);

		// The master is RETAIN-ALIVE READ-ONLY (peeking): its content is still
		// rendered (not torn down), but the title is now a non-editable <h1>
		// (`title-readonly`) instead of the click-to-edit button — the freeze.
		await expect(page.locator('h1.title.title-readonly', { hasText: 'FP host master' })).toBeVisible();
		await expect(page.locator('button.title', { hasText: 'FP host master' })).toHaveCount(0);

		// Depth 0: the pane's Back chevron is hidden.
		await expect(pane.locator('button.pane-back-btn')).toHaveCount(0);

		// Click the CHILD row INSIDE the pane → DRILL in place (same pathname,
		// `?item=` swaps to the child, the Back chevron appears at depth>0). The
		// master stays put + read-only underneath.
		const masterPathname = new URL(page.url()).pathname;
		await pane.locator('.child-row', { hasText: 'FP host grandchild' }).click();
		await expect.poll(() => openItemParam(page)).toBe(grandchildRef);
		expect(new URL(page.url()).pathname).toBe(masterPathname);
		await expect(pane.locator('button.pane-back-btn')).toBeVisible();
		await expect(pane.locator('.title', { hasText: 'FP host grandchild' })).toBeVisible();
		await expect(page.locator('h1.title.title-readonly', { hasText: 'FP host master' })).toBeVisible();

		// Browser BACK → pops one drill level back to B in the pane.
		await page.goBack();
		await expect.poll(() => openItemParam(page)).toBe(relatedRef);
		await expect(pane.locator('.title', { hasText: 'FP host related' })).toBeVisible();
		await expect(pane.locator('button.pane-back-btn')).toHaveCount(0);

		// Close (✕) → the pane unmounts cleanly, `?item=` drops, and the master is
		// EDITABLE again (no longer peeking → click-to-edit button returns).
		await pane.locator('button[title="Close pane"]').click();
		await expect(page.locator('.item-pane')).toHaveCount(0);
		await expect.poll(() => openItemParam(page)).toBeNull();
		await expect(page.locator('button.title', { hasText: 'FP host master' })).toBeVisible();
		await expect(page.locator('h1.title.title-readonly')).toHaveCount(0);
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
		await expect(page.locator('h1.title.title-readonly')).toHaveCount(0);
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
		// The master is present + peeking (read-only) beside the pane.
		await expect(page.locator('h1.title.title-readonly', { hasText: 'FP host cold master' })).toBeVisible();
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
		await expect(page.locator('h1.title.title-readonly')).toHaveCount(0);
	});

	test("a master whose SLUG equals another item's UUID does not over-block that item (server UUID-first precedence)", async ({
		page,
		fixture,
		request,
	}) => {
		await page.setViewportSize(DESKTOP);
		await browserLogin(page);

		// A is the real `?item=` target. B is a master whose SLUG is created to be
		// EXACTLY A's UUID (slugify keeps a UUID's hex+hyphens verbatim). The server
		// resolves `?item=<A's UUID>` UUID-FIRST to A — a DIFFERENT item — so the
		// pane must OPEN A, NOT be stripped as a master self-collision just because
		// B's slug string-equals it (orchestrator Codex round-8 verify).
		const a = await seedDoc(fixture, request, 'FP host uuid-slug target');
		const bResp = await request.post(
			`/api/v1/workspaces/${fixture.workspaceSlug}/collections/docs/items`,
			{ headers: authHeaders(fixture), data: { title: a.id, fields: '{}', content: '' } },
		);
		if (!bResp.ok()) throw new Error(`B create failed (${bResp.status()}): ${await bResp.text()}`);
		const b = (await bResp.json()) as SeededItem;
		expect(b.slug).toBe(a.id); // fixture assumption: B's slug IS A's UUID
		// Resolve B by its OWN id — GET /items/<B.slug> would resolve UUID-first to A.
		const bRef = await itemRef(fixture, request, b.id);

		await page.goto(fullPageUrl(fixture, bRef, `?item=${encodeURIComponent(a.id)}`));
		const pane = page.locator('.item-pane');
		await expect(pane).toBeVisible();
		await expect(pane.locator('.title', { hasText: 'FP host uuid-slug target' })).toBeVisible();
		await expect.poll(() => openItemParam(page)).toBe(a.id);
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
		await expect(pane.locator('.title', { hasText: 'FP host expand related' })).toBeVisible();
		await expect.poll(() => openItemParam(page)).toBe(relatedRef);
		await expect(page.locator('h1.title.title-readonly', { hasText: 'FP host expand master' })).toBeVisible();
	});
});
