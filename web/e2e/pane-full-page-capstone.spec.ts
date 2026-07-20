import { test, expect } from './fixtures';
import { browserLogin, seedDoc, EDITOR_SELECTOR, SYNCED_BADGE_SELECTOR } from './lib/collab-helpers';
import type { APIRequestContext, Page } from '@playwright/test';
import type { SuiteFixture } from './fixtures';

/**
 * Full-page pane HOST — Phase-2 CAPSTONE runtime verification
 * (PLAN-2154 Phase 2 / Architecture E / TASK-2175).
 *
 * The host tests (pane-full-page-host.spec.ts, TASK-2174) prove the pane
 * opens/drills/closes and the `?item==master` self-collision is stripped.
 * The collection-page R14 async-race capstone (pane-async-race.spec.ts,
 * TASK-2167) proves the controller's late-async-continuation fences on the
 * COLLECTION page. This suite is the last piece: it exercises, on the
 * FULL-PAGE host specifically, the three properties that host alone
 * introduces or that only it can now demonstrate:
 *
 *   1. Option-A mutation-SILENCE (the D2 / HT-2176 freeze acceptance). While
 *      a pane is peeking beside the retain-alive master, NO NEW user edit can
 *      be INITIATED on the master — the title click-to-edit is gone, field
 *      inputs are read-only, the comment composer / compose surfaces are
 *      unmounted, the rich editor is contenteditable=false, and the star /
 *      Share / Quick-actions affordances are gated. This is Option A — "no NEW
 *      edit can be INITIATED while peeking", NOT "zero network writes": a
 *      pre-pane pending save legitimately completes and remote collab sync is
 *      expected, so we assert the INITIATION surfaces are disabled/absent, not
 *      the absence of REST/WS traffic. Un-peeking (close) restores every one.
 *
 *   2. The bounded TWO-WS cost while peeking (D2's "cost model" note). Opening
 *      the pane yields at most the master's provider + the pane's provider = 2
 *      collab WS to DISTINCT item rooms, and NEVER a second provider on the
 *      MASTER's own room (the `?item==master` guard's whole reason to exist). A
 *      drill re-targets the ONE pane provider (master + drilled = still 2), it
 *      does not accumulate N.
 *
 *   3. Host-side R14 late-async continuations — the same fence family the
 *      collection-page async-race suite covers, re-exercised against the
 *      full-page host's persistent no-`{#key}` master + pane: a drill fired
 *      right after a browser Back (the stale back-settle must not revert it), a
 *      close-then-reopen (a stale loadData must not clobber the new pane), a
 *      held Back burst across `?item=` entries (coalesces to one teardown/mint),
 *      and a rapid double-close (cannot stack a second history.go and overshoot
 *      the master route).
 *
 * DEFERRED / out of scope (do NOT assert): BUG-2177 (an editor-bound
 * upload/crop orphaned by the peeking editor remount — accepted) and BUG-2178
 * (collection-rename/move-from-pane URL shaping on this host — deferred).
 *
 * The pane is a desktop-split concern, so the desktop project alone covers it.
 */

const DESKTOP = { width: 1200, height: 900 };
const FOLLOW_DEBOUNCE_MS = 140; // == PANE_MINT_SETTLE_MS
const SYNC_TIMEOUT = 20_000; // slow collab handshake + reconnect backoff

function authHeaders(fixture: SuiteFixture) {
	return { Authorization: `Bearer ${fixture.apiToken}`, 'Content-Type': 'application/json' };
}

function openItemParam(page: Page): string | null {
	return new URL(page.url()).searchParams.get('item');
}

function pathname(page: Page): string {
	return new URL(page.url()).pathname;
}

function fullPageUrl(fixture: SuiteFixture, collSlug: string, slug: string, query = ''): string {
	return `/${fixture.adminUsername}/${fixture.workspaceSlug}/${collSlug}/${slug}${query}`;
}

/** The route column that holds the MASTER (route-owned `.item-page-host >
 *  .item-page` direct child). The pane is a SEPARATE direct child
 *  (`aside.item-pane`), and ItemDetail's OWN inner content wrapper reuses the
 *  `.item-page` class — so scoping master-freeze assertions to the DIRECT-CHILD
 *  column excludes the pane's copy of every surface. */
function masterCol(page: Page) {
	return page.locator('.item-page-host > .item-page');
}

interface SeededItem {
	id: string;
	slug: string;
}

/** A `related` link FROM `sourceSlug` TO `targetId` — surfaces under the master
 *  page's "Related" relationship group as an `a.link-target` (the first-open
 *  affordance this host uses; mirrors pane-full-page-host.spec.ts). */
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

/** A doc whose `fields.parent` makes it a child of `parentId` — the parent's
 *  pane renders a `.child-row` to drill into. */
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

/** The PREFIX-NUMBER ref (e.g. "DOC-5") the app renders + puts in `?item=` after
 *  a relationship-link first-open. */
async function itemRef(fixture: SuiteFixture, request: APIRequestContext, slug: string): Promise<string> {
	const resp = await request.get(`/api/v1/workspaces/${fixture.workspaceSlug}/items/${slug}`, {
		headers: authHeaders(fixture),
	});
	if (!resp.ok()) throw new Error(`item get failed (${resp.status()}): ${await resp.text()}`);
	const item = (await resp.json()) as { collection_prefix?: string; item_number?: number };
	if (!item.collection_prefix || !item.item_number) throw new Error('item has no ref');
	return `${item.collection_prefix}-${item.item_number}`;
}

/** A fresh, test-scoped collection with a plain `note` text field, so the
 *  master renders a real editable field input whose readonly transition proves
 *  the freeze. */
async function seedNoteCollection(
	fixture: SuiteFixture,
	request: APIRequestContext,
	namePrefix: string,
	itemPrefix: string,
): Promise<{ id: string; slug: string }> {
	const name = `${namePrefix} ${Date.now()}`;
	const schema = JSON.stringify({ fields: [{ key: 'note', label: 'Note', type: 'text' }] });
	const resp = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/collections`, {
		headers: authHeaders(fixture),
		data: { name, prefix: itemPrefix, schema },
	});
	if (!resp.ok()) throw new Error(`collection create failed (${resp.status()}): ${await resp.text()}`);
	return (await resp.json()) as { id: string; slug: string };
}

async function seedNoteItem(
	fixture: SuiteFixture,
	request: APIRequestContext,
	collSlug: string,
	title: string,
	note: string,
	body = '',
): Promise<SeededItem> {
	const resp = await request.post(
		`/api/v1/workspaces/${fixture.workspaceSlug}/collections/${collSlug}/items`,
		{ headers: authHeaders(fixture), data: { title, fields: JSON.stringify({ note }), content: body } },
	);
	if (!resp.ok()) throw new Error(`item create failed (${resp.status()}): ${await resp.text()}`);
	return (await resp.json()) as SeededItem;
}

interface HookState {
	paneDepth: number;
	paneOwned: boolean;
}

/** Read the host controller's live {paneDepth, paneOwned} via the test hook
 *  (installed by `[collection]/[slug]/+page.svelte` under the localStorage
 *  gate — TASK-2175). */
function paneState(page: Page): Promise<HookState | null> {
	return page.evaluate(() => {
		const c = (window as unknown as { __padPaneController?: { getPaneState(): HookState } })
			.__padPaneController;
		return c ? c.getPaneState() : null;
	});
}

/** Drive an in-pane DRILL (`navigatePaneTo`) via the test hook. */
async function drillTo(page: Page, ref: string): Promise<void> {
	await page.evaluate((r) => {
		(
			window as unknown as { __padPaneController?: { navigatePaneTo(ref: string): void } }
		).__padPaneController?.navigatePaneTo(r);
	}, ref);
}

/** Enable the controller test hook for all navigations in this context. */
async function enableHook(page: Page): Promise<void> {
	await page.addInitScript(() => {
		try {
			localStorage.setItem('pad:pane-test-hook', '1');
		} catch {
			/* private mode / disabled storage — hook simply won't install */
		}
	});
}

/**
 * Every bare item-GET the browser issues (`/items/{seg}` — the plain item
 * fetch `loadData` fires; `/items/{seg}/links` etc. carry an extra segment and
 * are excluded). A live array of trailing segments so a test can prove a
 * fenced-out continuation issued NO fetch, or a re-target fetched the RIGHT
 * item, not the stale one (mirrors pane-async-race.spec.ts).
 */
function trackItemGets(page: Page): string[] {
	const segs: string[] = [];
	page.on('request', (req) => {
		if (req.method() !== 'GET') return;
		const m = /\/api\/v1\/workspaces\/[^/]+\/items\/([^/?]+)(?:\?|$)/.exec(req.url());
		if (m) segs.push(decodeURIComponent(m[1]));
	});
	return segs;
}

/** The collab-room segment (`item.id`) of a `/api/v1/collab/{itemID}` WS URL,
 *  or null for a non-collab socket. */
function collabRoom(url: string): string | null {
	const m = /\/api\/v1\/collab\/([^/?]+)/.exec(url);
	return m ? decodeURIComponent(m[1]) : null;
}

/** Open the pane by clicking a MASTER relationship link (the host's real
 *  first-open path — `handleMasterOpenTarget` → `openItemPaneByRef`). */
async function openPaneViaRelated(page: Page, titlePrefix: string): Promise<void> {
	await masterCol(page)
		.locator('.relationship-group', { hasText: 'Related' })
		.locator('a.link-target', { hasText: titlePrefix })
		.click();
}

test.describe('full-page pane host CAPSTONE (PLAN-2154 Phase 2 / TASK-2175)', () => {
	test.beforeEach(({}, testInfo) => {
		test.skip(
			testInfo.project.name !== 'desktop-chromium',
			'the pane is a desktop-split concern; one project is enough',
		);
	});

	// ── 1. Option-A mutation-SILENCE (the D2 / HT-2176 freeze acceptance) ────
	// While a pane peeks beside the retain-alive master, NO NEW user edit can be
	// INITIATED on the master. We assert the KEY initiation surfaces are
	// disabled/absent — NOT the absence of network writes (Option A explicitly
	// permits a pre-pane pending save to complete + remote collab sync). This is
	// the RUNTIME smoke of the freeze; the exhaustive per-mutation-path audit
	// (raw mode, tags, assignment, timeline reply/reaction/version, drag-reorder,
	// etc.) is unit-tested in masterFreeze.svelte.test.ts + mutationGate.test.ts.
	// Then un-peek (close) and assert the surfaces are restored.
	test('peeking freezes the master NEW-edit-initiation surfaces; closing the pane restores them', async ({
		page,
		fixture,
		request,
	}) => {
		test.setTimeout(60_000);
		await page.setViewportSize(DESKTOP);
		await browserLogin(page);

		// A note-collection MASTER (a real `note` text field whose readonly
		// transition proves the field freeze) --related--> a pane target.
		const coll = await seedNoteCollection(fixture, request, 'FP freeze', 'FPFZ');
		const master = await seedNoteItem(
			fixture,
			request,
			coll.slug,
			`FP freeze master ${Date.now()}`,
			'master-note-value',
			'master editor body FREEZE-11',
		);
		const target = await seedNoteItem(fixture, request, coll.slug, `FP freeze target ${Date.now()}`, 'target-note', '');
		await seedRelatedLink(fixture, request, master.slug, target.id);

		await page.goto(fullPageUrl(fixture, coll.slug, master.slug));
		const col = masterCol(page);

		// ── Pre-peek: the master is fully EDITABLE (no pane yet). ──
		await expect(page.locator('.item-page-host')).toBeVisible();
		const editableTitle = col.locator('button.title', { hasText: 'FP freeze master' });
		await expect(editableTitle).toBeVisible();
		// The Note field is an editable input (FieldEditor, not readonly-display).
		const noteInput = col.locator('.field-row', { hasText: 'Note' }).locator('input.field-input');
		await expect(noteInput).toBeVisible();
		await expect(noteInput).toBeEnabled();
		// The comment composer is present.
		await expect(col.locator('.compose')).toBeVisible();
		// Star is enabled; Share + Quick-actions + Delete are present.
		await expect(col.locator('button.star-btn')).toBeEnabled();
		await expect(col.locator('button.action-btn', { hasText: 'Share' })).toBeVisible();
		await expect(col.locator('button.trigger-btn[title="Quick actions"]')).toBeVisible();
		await expect(col.locator('button.delete-btn')).toBeVisible();
		await expect(col.locator('button.action-btn', { hasText: 'Move to' })).toBeVisible();
		// Relationship mutation surfaces (the master HAS a `related` link): the
		// per-link remove button (rendered but `display:none` until row-hover, so
		// assert DOM presence, not visibility) and the "+ Add relationship" opener.
		await expect(col.locator('button.link-delete-btn')).toHaveCount(1);
		await expect(col.locator('button.add-relationship-btn')).toBeVisible();
		// The rich editor is the live collab editor and is EDITABLE.
		await expect(col.locator(SYNCED_BADGE_SELECTOR)).toBeVisible({ timeout: SYNC_TIMEOUT });
		const masterEditor = col.locator(EDITOR_SELECTOR);
		await expect(masterEditor).toBeVisible({ timeout: SYNC_TIMEOUT });
		await expect(masterEditor).toHaveAttribute('contenteditable', 'true');

		// ── Open the pane → the master goes PEEKING (retain-alive read-only). ──
		await openPaneViaRelated(page, 'FP freeze target');
		const pane = page.locator('.item-pane');
		await expect(pane).toBeVisible();
		await expect(pane.locator('.title', { hasText: 'FP freeze target' })).toBeVisible();

		// ── Peeking: every NEW-edit-initiation surface is gone/disabled. ──
		// Title: click-to-edit button replaced by a non-editable <h1>.
		await expect(col.locator('h1.title.title-readonly', { hasText: 'FP freeze master' })).toBeVisible();
		await expect(col.locator('button.title', { hasText: 'FP freeze master' })).toHaveCount(0);
		// Field: the editable input is gone — rendered as a readonly-display.
		await expect(col.locator('input.field-input')).toHaveCount(0);
		await expect(col.locator('.field-row', { hasText: 'Note' }).locator('.readonly-display')).toBeVisible();
		// Comment composer: unmounted entirely.
		await expect(col.locator('.compose')).toHaveCount(0);
		// Star: disabled (gated on `peeking`, not unmounted — it stays visible).
		await expect(col.locator('button.star-btn')).toBeDisabled();
		// Share + Quick-actions triggers: unmounted.
		await expect(col.locator('button.action-btn', { hasText: 'Share' })).toHaveCount(0);
		await expect(col.locator('button.trigger-btn[title="Quick actions"]')).toHaveCount(0);
		// Move-to + Delete: unmounted (gated on mutationsEnabled).
		await expect(col.locator('button.action-btn', { hasText: 'Move to' })).toHaveCount(0);
		await expect(col.locator('button.delete-btn')).toHaveCount(0);
		// Relationship mutation surfaces: the per-link remove + Add opener are gone.
		await expect(col.locator('button.link-delete-btn')).toHaveCount(0);
		await expect(col.locator('button.add-relationship-btn')).toHaveCount(0);
		// Rich editor: retained (still visible — no teardown), but read-only.
		await expect(masterEditor).toBeVisible();
		await expect(masterEditor).toHaveAttribute('contenteditable', 'false');

		// ── Close (un-peek) → every surface is EDITABLE again. ──
		await pane.locator('button[aria-label="Close pane"]').click();
		await expect(pane).toBeHidden();
		await expect(col.locator('button.title', { hasText: 'FP freeze master' })).toBeVisible();
		await expect(col.locator('h1.title.title-readonly')).toHaveCount(0);
		await expect(col.locator('.field-row', { hasText: 'Note' }).locator('input.field-input')).toBeVisible();
		await expect(col.locator('.compose')).toBeVisible();
		await expect(col.locator('button.star-btn')).toBeEnabled();
		await expect(col.locator('button.action-btn', { hasText: 'Share' })).toBeVisible();
		await expect(col.locator('button.trigger-btn[title="Quick actions"]')).toBeVisible();
		await expect(col.locator('button.delete-btn')).toBeVisible();
		await expect(col.locator('button.action-btn', { hasText: 'Move to' })).toBeVisible();
		await expect(col.locator('button.add-relationship-btn')).toBeVisible();
		// The per-link remove is back in the DOM (display:none until row-hover).
		await expect(col.locator('button.link-delete-btn')).toHaveCount(1);
		await expect(masterEditor).toHaveAttribute('contenteditable', 'true', { timeout: SYNC_TIMEOUT });
	});

	// ── 2. Bounded TWO-WS cost while peeking (D2 cost model / the guard core) ─
	// Opening the pane yields master-provider + pane-provider = 2 collab WS to
	// DISTINCT rooms, and the master's OWN room is NEVER given a second provider.
	// A drill RE-TARGETS the one pane provider (master + drilled = still 2), it
	// does not accumulate N. Proven by counting LIVE sockets PER ROOM (so a
	// double master provider shows as count 2, and accumulation shows as a 3rd
	// room) plus the reconnect-safe max-concurrent-per-room ceiling.
	test('opening + drilling the pane stays bounded to 2 collab WS to distinct rooms; the master room never gets a second provider', async ({
		page,
		fixture,
		request,
	}) => {
		test.setTimeout(60_000);
		await page.setViewportSize(DESKTOP);
		await browserLogin(page);

		const master = await seedDoc(fixture, request, 'FP ws master');
		const related = await seedDoc(fixture, request, 'FP ws related');
		const grandchild = await seedChildDoc(fixture, request, 'FP ws grandchild', related.id);
		await seedRelatedLink(fixture, request, master.slug, related.id);

		// LIVE collab sockets per room + the running max concurrent per room. A
		// reconnect closes-then-opens, so `maxByRoom[master] === 1` is the
		// reconnect-SAFE encoding of "never two concurrent providers on the
		// master's own room" — the `?item==master` guard's exact contract.
		const liveByRoom = new Map<string, number>();
		const maxByRoom = new Map<string, number>();
		// The running peak of TOTAL concurrent collab sockets across ALL rooms —
		// the real "at most 2 collab WS" ceiling. Per-room maxima alone would miss
		// a transient master + old-pane + new-pane = 3 during a drill (Codex P1).
		let maxTotalLive = 0;
		const totalLive = () => [...liveByRoom.values()].reduce((a, b) => a + b, 0);
		page.on('websocket', (ws) => {
			const room = collabRoom(ws.url());
			if (!room) return;
			const n = (liveByRoom.get(room) ?? 0) + 1;
			liveByRoom.set(room, n);
			maxByRoom.set(room, Math.max(maxByRoom.get(room) ?? 0, n));
			maxTotalLive = Math.max(maxTotalLive, totalLive());
			ws.on('close', () => {
				const c = (liveByRoom.get(room) ?? 0) - 1;
				if (c <= 0) liveByRoom.delete(room);
				else liveByRoom.set(room, c);
			});
		});
		const liveRoomsObj = () => Object.fromEntries([...liveByRoom.entries()]);

		await page.goto(fullPageUrl(fixture, 'docs', master.slug));
		await expect(page.locator('.item-page-host')).toBeVisible();
		// The master mounts its ONE collab provider (its own room, count 1).
		await expect.poll(liveRoomsObj, { timeout: SYNC_TIMEOUT }).toEqual({ [master.id]: 1 });

		// Open the pane on the related item → master + pane = exactly 2 distinct
		// rooms, each with a single provider. A regressed guard that minted a
		// SECOND provider on the master's room would show `{[master.id]: 2, ...}`.
		await openPaneViaRelated(page, 'FP ws related');
		const pane = page.locator('.item-pane');
		await expect(pane).toBeVisible();
		await expect(pane.locator('.title', { hasText: 'FP ws related' })).toBeVisible();
		await expect
			.poll(liveRoomsObj, { timeout: SYNC_TIMEOUT })
			.toEqual({ [master.id]: 1, [related.id]: 1 });

		// Drill related → grandchild INSIDE the pane (a real `.child-row` click).
		// The pane provider RE-TARGETS: `related`'s socket is torn down, the
		// grandchild's minted — master + grandchild = STILL 2 distinct rooms, not
		// {master, related, grandchild}. This is "one pane provider that
		// re-targets, not N".
		await pane.locator('.child-row', { hasText: 'FP ws grandchild' }).click();
		await expect(pane.locator('.title', { hasText: 'FP ws grandchild' })).toBeVisible();
		await expect
			.poll(liveRoomsObj, { timeout: SYNC_TIMEOUT })
			.toEqual({ [master.id]: 1, [grandchild.id]: 1 });

		// Close the pane → the pane provider is torn down; the master's retained
		// provider remains its single, only-ever provider.
		await pane.locator('button[aria-label="Close pane"]').click();
		await expect(pane).toBeHidden();
		await expect.poll(liveRoomsObj, { timeout: SYNC_TIMEOUT }).toEqual({ [master.id]: 1 });

		// The load-bearing guard assertion: at NO point did the master's own room
		// carry two concurrent providers (reconnect-safe max). The pane rooms
		// (`related`, `grandchild`) likewise each peaked at a single provider.
		expect(maxByRoom.get(master.id), 'master room must never have 2 concurrent providers').toBe(1);
		expect(maxByRoom.get(related.id)).toBe(1);
		expect(maxByRoom.get(grandchild.id)).toBe(1);
		// The TOTAL ceiling: never more than master + ONE pane provider = 2 collab
		// sockets alive at once (the drill's `collabKey→null` teardown closes the
		// outgoing pane socket before the drilled one mints, so no transient 3).
		expect(maxTotalLive, 'never more than master + one pane provider live at once').toBe(2);
	});

	// The self-collision case, WS-instrumented (the guard's raison d'être). The
	// bounded-cost test above opens the pane on DIFFERENT items, so it can't catch
	// a `?item==master` guard regression that briefly mints a SECOND provider on
	// the master's OWN room. pane-full-page-host.spec.ts proves the URL is
	// stripped + no pane mounts; THIS proves it at the WS layer: a cold-loaded
	// `?item=<master>` must leave the master room at ONE provider and mint NO pane
	// provider (a pane on `?item=master` would connect to the SAME room, so a
	// regression shows as master-room concurrency 2 / total 2).
	test('a cold-loaded ?item=<master> never mints a second provider on the master room (WS-instrumented)', async ({
		page,
		fixture,
		request,
	}) => {
		test.setTimeout(60_000);
		await page.setViewportSize(DESKTOP);
		await browserLogin(page);

		const master = await seedDoc(fixture, request, 'FP selfref master');
		const masterRef = await itemRef(fixture, request, master.slug);

		const liveByRoom = new Map<string, number>();
		const maxByRoom = new Map<string, number>();
		let maxTotalLive = 0;
		const openedRooms: string[] = [];
		const totalLive = () => [...liveByRoom.values()].reduce((a, b) => a + b, 0);
		page.on('websocket', (ws) => {
			const room = collabRoom(ws.url());
			if (!room) return;
			openedRooms.push(room);
			const n = (liveByRoom.get(room) ?? 0) + 1;
			liveByRoom.set(room, n);
			maxByRoom.set(room, Math.max(maxByRoom.get(room) ?? 0, n));
			maxTotalLive = Math.max(maxTotalLive, totalLive());
			ws.on('close', () => {
				const c = (liveByRoom.get(room) ?? 0) - 1;
				if (c <= 0) liveByRoom.delete(room);
				else liveByRoom.set(room, c);
			});
		});

		// Cold-load a hand-crafted `?item=<master ref>` — the forbidden self-alias.
		await page.goto(fullPageUrl(fixture, 'docs', master.slug, `?item=${masterRef}`));
		await expect(masterCol(page).locator('button.title', { hasText: 'FP selfref master' })).toBeVisible();
		// The guard stripped `?item=` and never mounted a pane.
		await expect(page.locator('.item-pane')).toHaveCount(0);
		await expect.poll(() => openItemParam(page)).toBeNull();
		// Let the master editor sync and settle any transient the mount-gate had to
		// close, then read the WS ledger.
		await expect(masterCol(page).locator(SYNCED_BADGE_SELECTOR)).toBeVisible({ timeout: SYNC_TIMEOUT });
		await page.waitForTimeout(500);
		// Steady state: the master's own provider, and ONLY it.
		await expect.poll(() => Object.fromEntries([...liveByRoom.entries()])).toEqual({ [master.id]: 1 });
		// Only the master's room was ever opened (no pane provider for the self-ref).
		expect(new Set(openedRooms)).toEqual(new Set([master.id]));
		// The master room never carried two concurrent providers, and total collab
		// sockets never exceeded 1 — the guard prevented the second-provider mint.
		expect(maxByRoom.get(master.id), 'master room must never have 2 concurrent providers').toBe(1);
		expect(maxTotalLive, 'no pane provider may be minted for a self-ref').toBe(1);
	});

	// ── 3. Host-side R14 late-async continuations ────────────────────────────

	// R14-a — a drill fired right after a browser Back lands on the new target;
	// the stale back-settle never reverts it. The Back is a popstate that arms
	// the host's ~140ms mint-settle for the popped-to ref; a drill inside that
	// window is a deliberate `goto` that must CANCEL the pending settle and apply
	// immediately, so the settle can't drag the pane back. Proven non-vacuously:
	// the popped-to ref is never re-minted (fetched) in the post-Back window.
	test('R14: a drill fired right after a browser Back lands on the new target; the stale back-settle never reverts it', async ({
		page,
		fixture,
		request,
	}) => {
		test.setTimeout(60_000);
		await page.setViewportSize(DESKTOP);
		await enableHook(page);
		await browserLogin(page);

		// MASTER --related--> A; B and C are drilled to by the hook.
		const master = await seedDoc(fixture, request, 'FP drillback master');
		const a = await seedDoc(fixture, request, 'FP drillback alpha');
		const b = await seedDoc(fixture, request, 'FP drillback bravo');
		const c = await seedDoc(fixture, request, 'FP drillback charlie');
		await seedRelatedLink(fixture, request, master.slug, a.id);

		await page.goto(fullPageUrl(fixture, 'docs', master.slug));
		await expect(page.locator('.item-page-host')).toBeVisible();

		const itemGets = trackItemGets(page);
		// First-open the pane on A (owned depth 0), then drill A→B (depth 1).
		await openPaneViaRelated(page, 'FP drillback alpha');
		const pane = page.locator('.item-pane');
		await expect(pane).toBeVisible();
		await expect.poll(() => paneState(page)).toEqual({ paneDepth: 0, paneOwned: true });
		const refA = openItemParam(page);
		await drillTo(page, b.slug);
		await expect.poll(() => paneState(page)).toEqual({ paneDepth: 1, paneOwned: true });

		// Back to A (depth 0) — a popstate that arms the mint-settle for A — then
		// IMMEDIATELY drill to C, inside the settle window.
		const getsBeforeBack = itemGets.length;
		await page.goBack();
		await expect.poll(() => openItemParam(page)).toBe(refA);
		await drillTo(page, c.slug);

		await expect.poll(() => openItemParam(page)).toBe(c.slug);
		await expect.poll(() => paneState(page)).toEqual({ paneDepth: 1, paneOwned: true });
		await expect(pane.locator('.title', { hasText: /FP drillback charlie/ })).toBeVisible();

		// The cancelled A-settle must NOT fire late and drag the pane back to A.
		await page.waitForTimeout(FOLLOW_DEBOUNCE_MS + 120);
		expect(openItemParam(page)).toBe(c.slug);
		await expect(pane.locator('.title', { hasText: /FP drillback charlie/ })).toBeVisible();
		// Non-vacuous: A's ref was never re-minted (fetched) after the Back — the
		// drill's `goto` cancelled the settle before it could re-fetch A. Had the
		// settle fired, A's ref would appear in the post-Back fetch sequence.
		const postBack = itemGets.slice(getsBeforeBack);
		expect(postBack).toContain(c.slug);
		expect(postBack).not.toContain(refA);
	});

	// R14-b — a close then a re-open: a loadData GET still outstanding when the
	// pane closed must write nothing over the newly-opened item. Closing unmounts
	// PaneHost (its ItemDetail `onDestroy` bumps `loadGeneration`); re-opening
	// mounts a fresh instance. The orphaned A GET, on resolving, must bail at the
	// post-item-GET gen-check — so A's downstream sub-resource GETs never fire,
	// and B (the visible item) is untouched.
	test('R14: a loadData GET outstanding when the pane closed writes nothing over the re-opened item', async ({
		page,
		fixture,
		request,
	}) => {
		test.setTimeout(60_000);
		await page.setViewportSize(DESKTOP);
		await enableHook(page);
		await browserLogin(page);

		// MASTER --related--> A and --related--> B (two first-open targets).
		const master = await seedDoc(fixture, request, 'FP closeload master');
		const a = await seedDoc(fixture, request, 'FP closeload alpha', {});
		const b = await seedDoc(fixture, request, 'FP closeload bravo', {});
		await seedRelatedLink(fixture, request, master.slug, a.id);
		await seedRelatedLink(fixture, request, master.slug, b.id);
		// The relationship first-open puts A's REF (not slug) in `?item=`, and the
		// pane fetches `/items/{aRef}` — gate THAT.
		const aRef = await itemRef(fixture, request, a.slug);

		const pageErrors: string[] = [];
		page.on('pageerror', (err) => pageErrors.push(String(err)));
		// A's DOWNSTREAM sub-resource GETs (progress/links) fire only if loadData
		// proceeds PAST the post-item-GET gen-check — i.e. only if the fence failed.
		// `loadData` fetches progress/links by the LOADED item's SLUG (ItemDetail
		// ~L1211/1233 `itemData.slug`), NOT the `?item=` REF — so track the SLUG
		// (plus the ref as a belt) or the fence proof is vacuous (Codex P1).
		const aSubResourceGets: string[] = [];
		page.on('request', (req) => {
			if (req.method() !== 'GET') return;
			const p = new URL(req.url()).pathname;
			if (p.includes(`/items/${a.slug}/`) || p.includes(`/items/${aRef}/`)) aSubResourceGets.push(p);
		});

		let releaseA: () => void = () => {};
		const aGate = new Promise<void>((resolve) => {
			releaseA = resolve;
		});
		await page.route(`**/api/v1/workspaces/*/items/${aRef}`, async (route) => {
			if (route.request().method() !== 'GET') {
				await route.continue();
				return;
			}
			await aGate;
			await route.continue();
		});

		await page.goto(fullPageUrl(fixture, 'docs', master.slug));
		await expect(page.locator('.item-page-host')).toBeVisible();

		// Open the pane on A — A's loadData stalls on the gate (minimal header).
		await openPaneViaRelated(page, 'FP closeload alpha');
		const pane = page.locator('.item-pane');
		await expect(pane).toBeVisible();
		await expect(pane.locator('.pane-header--minimal')).toBeVisible();

		// Close the pane while A's GET is still outstanding (the ItemDetail
		// unmounts mid-load). Wait for the close to settle before re-opening.
		await page.evaluate(() => {
			(
				window as unknown as { __padPaneController?: { closeItemPane(): void } }
			).__padPaneController?.closeItemPane();
		});
		await expect.poll(() => openItemParam(page)).toBeNull();
		await expect(pane).toBeHidden();

		// Re-open the pane on B (B's GET is NOT gated) — B is now the live item and
		// the evidence surface for any stale A write.
		await openPaneViaRelated(page, 'FP closeload bravo');
		await expect(pane).toBeVisible();
		await expect(pane.locator('.title', { hasText: /FP closeload bravo/ })).toBeVisible();
		const refB = openItemParam(page);
		expect(refB).not.toBeNull();

		// Release A's now-orphaned GET and deterministically drain its post-unmount
		// continuation (`finished()` = full body delivered; a `setTimeout(0)`
		// browser task turn runs after all the continuation's microtasks).
		const aResponse = page.waitForResponse(
			(r) => r.request().method() === 'GET' && new URL(r.url()).pathname.endsWith(`/items/${aRef}`),
		);
		releaseA();
		await (await aResponse).finished();
		await page.evaluate(() => new Promise<void>((r) => setTimeout(() => r(), 0)));

		// B is untouched, `?item=` unmoved, nothing threw.
		await expect(pane.locator('.title', { hasText: /FP closeload bravo/ })).toBeVisible();
		expect(openItemParam(page)).toBe(refB);
		await expect(pane.locator('.title', { hasText: /FP closeload alpha/ })).toHaveCount(0);
		expect(pageErrors, `no page errors from the post-close load: ${pageErrors.join('; ')}`).toEqual([]);
		// The load-bearing fence proof: A's continuation bailed at the gen-check
		// immediately after its item GET resolved — it never proceeded to A's
		// collection/progress/links, so no sub-resource GET for A was ever issued.
		expect(
			aSubResourceGets,
			`A's post-close continuation must not proceed to sub-resource fetches: ${aSubResourceGets.join(', ')}`,
		).toEqual([]);
	});

	// R14-c — a held browser Back BURST through a drilled stack coalesces to ONE
	// teardown/mint. The mint-settle's raison d'être is coalescing MULTIPLE
	// popstates (a held Back firing one per entry) into a single teardown/mint.
	// Fire three separate `history.back()` in one tick — a real burst — and
	// assert the pane coalesces to the correct final item at depth 0, exactly one
	// pane instance, with the intermediate stops NEVER minted.
	test('R14: a held Back burst through the host drill stack coalesces to the base item with one mint', async ({
		page,
		fixture,
		request,
	}) => {
		test.setTimeout(60_000);
		await page.setViewportSize(DESKTOP);
		await enableHook(page);
		await browserLogin(page);

		const master = await seedDoc(fixture, request, 'FP burst master');
		const a = await seedDoc(fixture, request, 'FP burst alpha');
		const b = await seedDoc(fixture, request, 'FP burst bravo');
		const c = await seedDoc(fixture, request, 'FP burst charlie');
		const d = await seedDoc(fixture, request, 'FP burst delta');
		await seedRelatedLink(fixture, request, master.slug, a.id);

		await page.goto(fullPageUrl(fixture, 'docs', master.slug));
		await expect(page.locator('.item-page-host')).toBeVisible();

		const itemGets = trackItemGets(page);
		// First-open A (depth 0), drill A→B→C→D (depth 3).
		await openPaneViaRelated(page, 'FP burst alpha');
		const pane = page.locator('.item-pane');
		await expect(pane).toBeVisible();
		const refA = openItemParam(page);
		await drillTo(page, b.slug);
		await expect.poll(() => paneState(page)).toEqual({ paneDepth: 1, paneOwned: true });
		await drillTo(page, c.slug);
		await expect.poll(() => paneState(page)).toEqual({ paneDepth: 2, paneOwned: true });
		await drillTo(page, d.slug);
		await expect.poll(() => paneState(page)).toEqual({ paneDepth: 3, paneOwned: true });
		// D must be fully RENDERED before the burst, so the post-burst A-title
		// assertion can't be satisfied by a stale, previously-rendered A (Codex).
		await expect(pane.locator('.title', { hasText: /FP burst delta/ })).toBeVisible({ timeout: SYNC_TIMEOUT });
		const getsBeforeBurst = itemGets.length;

		// The burst: THREE genuine popstates, each `history.back()` fired from the
		// PRECEDING popstate handler so the browser can't coalesce them at the
		// event level — a real held-Back the pane's mint-settle (not the browser)
		// must coalesce. `popstateCount` proves all three fired.
		const popstateCount = await page.evaluate(
			() =>
				new Promise<number>((resolve) => {
					let count = 0;
					const onPop = () => {
						count += 1;
						if (count < 3) {
							history.back();
						} else {
							window.removeEventListener('popstate', onPop);
							resolve(count);
						}
					};
					window.addEventListener('popstate', onPop);
					history.back();
				}),
		);
		expect(popstateCount).toBe(3);

		// Coalesced to the base A at depth 0 — exactly one pane, showing A.
		await expect.poll(() => paneState(page)).toEqual({ paneDepth: 0, paneOwned: true });
		await expect.poll(() => openItemParam(page)).toBe(refA);
		await expect(page.locator('.item-pane')).toHaveCount(1);
		await expect(pane.locator('.title', { hasText: /FP burst alpha/ })).toBeVisible({ timeout: SYNC_TIMEOUT });
		// Non-vacuous proof of COALESCING (not just the right destination): the
		// intermediate stops (C, B) traversed mid-burst were NEVER minted — their
		// item GETs are absent from the burst window. Had each popstate minted
		// eagerly, C and B would appear here.
		await page.waitForTimeout(200);
		const burstGets = itemGets.slice(getsBeforeBurst);
		expect(burstGets).not.toContain(c.slug);
		expect(burstGets).not.toContain(b.slug);
		// ...and the base A was minted EXACTLY ONCE — the settle coalesced the
		// three popstates into a SINGLE teardown/mint, not one per traversed entry
		// (proves "one mint", not just "the right destination"; Codex P2).
		expect(burstGets.filter((g) => g === refA)).toHaveLength(1);
	});

	// R14-d — a rapid double-close cannot stack a second history.go and overshoot
	// the master route. The owned close is a one-phase `history.go(-1)`; without
	// the in-flight fence a second synchronous close would read the still-stale
	// state and stack a SECOND go(-1), overshooting PAST the master page (here,
	// back to /login). We COUNT `history.go` calls across the burst: the fence
	// must let EXACTLY ONE through, and the pane must land on the master route.
	test('R14: a rapid double-close issues exactly one history.go and lands on the master route (never overshoots)', async ({
		page,
		fixture,
		request,
	}) => {
		test.setTimeout(60_000);
		await page.setViewportSize(DESKTOP);
		await enableHook(page);
		await browserLogin(page);

		const master = await seedDoc(fixture, request, 'FP dblclose master');
		const a = await seedDoc(fixture, request, 'FP dblclose alpha');
		await seedRelatedLink(fixture, request, master.slug, a.id);

		await page.goto(fullPageUrl(fixture, 'docs', master.slug));
		await expect(page.locator('.item-page-host')).toBeVisible();
		const masterUrl = page.url();
		const masterPath = pathname(page);

		// First-open the pane on A (owned depth 0).
		await openPaneViaRelated(page, 'FP dblclose alpha');
		const pane = page.locator('.item-pane');
		await expect(pane).toBeVisible();
		await expect.poll(() => paneState(page)).toEqual({ paneDepth: 0, paneOwned: true });

		// Fire closeItemPane TWICE synchronously, counting `history.go`. The owned
		// close issues one go(-1); the second call hits the in-flight fence and
		// no-ops (issues no go). Counting is the only timing-immune way to
		// distinguish "second close fenced" from "second go(-1) browser-coalesced"
		// — both leave the same end URL (Codex round 4 discipline).
		const goCount = await page.evaluate(() => {
			let count = 0;
			const orig = history.go.bind(history);
			history.go = ((delta?: number) => {
				count += 1;
				return orig(delta);
			}) as typeof history.go;
			const ctrl = (
				window as unknown as { __padPaneController?: { closeItemPane(): void } }
			).__padPaneController;
			ctrl?.closeItemPane();
			ctrl?.closeItemPane();
			history.go = orig; // restore before the async traversal settles
			return count;
		});
		expect(goCount).toBe(1);

		// Landed EXACTLY on the master route (pane gone, `?item=` gone) — never
		// overshot the master page back to /login.
		await expect.poll(() => openItemParam(page)).toBeNull();
		await expect(pane).toBeHidden();
		await expect.poll(() => page.url()).toBe(masterUrl);
		expect(pathname(page)).toBe(masterPath);
		expect(page.url()).not.toContain('/login');
		// The master is EDITABLE again (un-peeked) — a clean single unwind.
		await expect(masterCol(page).locator('button.title', { hasText: 'FP dblclose master' })).toBeVisible();
	});
});
