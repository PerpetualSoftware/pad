import { test, expect } from './fixtures';
import { browserLogin } from './lib/collab-helpers';
import type { APIRequestContext, Locator, Page } from '@playwright/test';
import type { SuiteFixture } from './fixtures';

// Run this file's tests one at a time. Each seeds its own collection(s), so
// they don't race a shared resource the way an earlier version of this
// spec did — but the suite's collection-prefix derivation
// (collections.DerivePrefix, workspace-scoped) isn't guaranteed collision-
// free across concurrently-created collections with similar names, and a
// prefix collision would cross-contaminate item numbering between the two
// tests' otherwise-isolated fixtures. Serial execution removes the
// ambiguity entirely.
test.describe.configure({ mode: 'serial' });

/**
 * Playwright has no built-in "locate an input by its current value"
 * finder (getByDisplayValue is a Testing Library concept, not a Playwright
 * one) — and a CSS `[value=...]` attribute selector only sees the
 * server-rendered/initial attribute, not the live DOM property Svelte's
 * `bind:value` actually writes to. `.inputValue()` reads the live
 * property, so we scan with it instead of guessing at field/option order.
 */
async function locatorByInputValue(candidates: Locator, value: string): Promise<Locator> {
	const count = await candidates.count();
	for (let i = 0; i < count; i++) {
		const candidate = candidates.nth(i);
		if ((await candidate.inputValue()) === value) return candidate;
	}
	throw new Error(`no element among ${count} candidates has value "${value}"`);
}

async function fieldCardByLabel(page: Page, label: string): Promise<Locator> {
	const cards = page.locator('.field-card');
	const count = await cards.count();
	for (let i = 0; i < count; i++) {
		const card = cards.nth(i);
		if ((await card.locator('.field-label-input').inputValue()) === label) return card;
	}
	throw new Error(`no field card among ${count} has label "${label}"`);
}

function authHeaders(fixture: SuiteFixture) {
	return { Authorization: `Bearer ${fixture.apiToken}`, 'Content-Type': 'application/json' };
}

/**
 * Create a fresh, test-scoped collection with a migratable `status` select
 * field (options draft/published/archived, matching the docs template's
 * shape) plus a plain `category` text field. Each test gets its OWN
 * collection (unique slug) rather than reusing a shared template
 * collection like `docs` — these tests rename a select option, a
 * workspace-global schema mutation, so sharing a collection across tests
 * (or across parallel runs) would race two concurrent renames against the
 * same option.
 */
async function seedMigratableCollection(
	fixture: SuiteFixture,
	request: APIRequestContext,
	namePrefix: string,
	itemPrefix: string,
) {
	// IMPORTANT #1: don't pass an explicit `slug` that diverges from
	// slugify(name). handleUpdateCollection unconditionally re-derives the
	// slug from `name` on EVERY save (EditCollectionModal always submits
	// `name`, even unchanged) via uniqueSlugExcluding(slugify(name)) —
	// internal/store/collections.go's UpdateCollection. If name and slug
	// were seeded out of sync, the very first Fields-tab-only save (no
	// name edit at all) would silently reassign the collection a NEW slug
	// out from under the still-loaded route, 404-ing every subsequent
	// fetch by the OLD slug. Keeping name unique (timestamped) and letting
	// the server derive slug from it keeps them in sync exactly like a
	// real single collection does, so a same-collection resave is a no-op
	// slug-wise.
	//
	// IMPORTANT #2: pass an explicit, LETTERS-ONLY `itemPrefix` rather than
	// letting the server derive one from `name` (collections.DerivePrefix).
	// The derived prefix can pick up a leading digit from a numeric "word"
	// in the name (e.g. a trailing Date.now() uniqueness suffix) — item
	// refs are `{PREFIX}-{N}`, and the server's ref parser expects the
	// prefix to be pure letters, so a digit-containing prefix makes every
	// by-ref lookup (GET /items/{ref}) 404 with "Item not found" even
	// though the item exists (confirmed empirically: creating a collection
	// named "bug2129-switch <timestamp>" derived prefix "BS1" — the
	// leading digit of the timestamp "word" — and every subsequent
	// GET /items/BS1-10 404'd while GET /items/{slug} succeeded).
	const name = `${namePrefix} ${Date.now()}`;
	const schema = JSON.stringify({
		fields: [
			{
				key: 'status',
				label: 'Status',
				type: 'select',
				options: ['draft', 'published', 'archived'],
				default: 'draft',
				required: true,
			},
			{ key: 'category', label: 'Category', type: 'text' },
		],
	});
	const resp = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/collections`, {
		headers: authHeaders(fixture),
		data: { name, prefix: itemPrefix, schema },
	});
	if (!resp.ok()) throw new Error(`collection create failed (${resp.status()}): ${await resp.text()}`);
	return (await resp.json()) as { id: string; slug: string; name: string };
}

async function seedItem(
	fixture: SuiteFixture,
	request: APIRequestContext,
	collSlug: string,
	title: string,
	fields: Record<string, unknown>,
) {
	const resp = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/collections/${collSlug}/items`, {
		headers: authHeaders(fixture),
		data: { title, fields: JSON.stringify(fields), content: '' },
	});
	if (!resp.ok()) throw new Error(`item create failed (${resp.status()}): ${await resp.text()}`);
	return (await resp.json()) as { id: string; slug: string };
}

/**
 * BUG-2129 regression e2e (PLAN-2154 Phase 0 / TASK-2155).
 *
 * BACKGROUND — this spec's investigation found the switch-fence guarding
 * <EditCollectionModal>'s `onupdated` callback (ItemDetail.svelte) was a
 * `{@const keyedSlug = itemSlug}` comparison that LOOKS like it freezes the
 * item identity at the moment the modal's `{#key itemSlug}` block renders,
 * but does not: Svelte 5 compiles `{@const}` bindings (and prop-expression
 * closures passed across a component boundary, e.g. an IIFE inside a prop
 * value) as lazily-pulled derived signals. Since `keyedSlug` was read ONLY
 * inside the `onupdated` closure — never in the template body — the signal
 * was never "pulled" until the closure actually ran, at which point it
 * computed the CURRENT `itemSlug`, not the value at render time. Verified
 * by runtime instrumentation (console logging + stack traces) during this
 * investigation: `keyedSlug` and `itemSlug` were byte-identical at every
 * `onupdated` firing, in BOTH the pre-fix and a naively "fixed" build,
 * because the comparison could never actually diverge.
 *
 * Net effect pre-fix: `keyedSlug !== itemSlug` was always false, so the
 * "superseded" branch was DEAD CODE — the callback always ran as if it were
 * still on the original item, which happens to reload the CURRENTLY shown
 * item's fields correctly (a lucky non-symptom for the same-collection
 * pane case: see the first test below) but ALSO means the archive/rename
 * navigation always targeted whatever is currently displayed, with no
 * actual identity check — a real hazard once panes/routes span different
 * collections (the second test below, and the target of PLAN-2154's
 * upcoming cross-collection pane work).
 *
 * THE FIX replaces the non-functional `{@const}` freeze with a genuine
 * gen+id snapshot (`pendingCollectionEditGen` / `pendingCollectionEditItemId`
 * in ItemDetail.svelte) captured SYNCHRONOUSLY inside the `onmanage` click
 * handler that opens the modal — a real event-handler execution, not a
 * template binding or a prop-expression closure, so it's genuine JS
 * variable semantics with no signal/getter indirection. `onupdated` then
 * compares that snapshot against the LIVE `loadGeneration`/`item.id` to
 * decide whether it's superseded — mirroring the gen+id pattern used
 * throughout this file (loadData, updateField, the SSE handlers). When
 * superseded, it does the minimal safe thing: if the currently-shown item
 * is STILL in the collection that changed, refresh it (closing BUG-2129's
 * stale-fields gap); otherwise it does nothing (no wrongful navigation).
 *
 * Two tests:
 *   1. Same-collection pane switch (BUG-2129's literal repro): asserts the
 *      post-migration reload happens and a subsequent field edit doesn't
 *      clobber the migrated value. This demonstrates correct end-state
 *      behavior but — per the finding above — does NOT by itself
 *      discriminate the fix from the pre-fix code (both happen to reload
 *      correctly here, for different reasons).
 *   2. Cross-collection full-page navigation: the case that DOES
 *      discriminate. Pre-fix, the non-functional fence lets a completed
 *      rename/migration hijack navigation to whatever item is CURRENTLY
 *      displayed, even if that item is in a totally unrelated collection —
 *      an observable, user-facing bug (broken redirect / wrong page).
 *      Post-fix, the genuine gen+id fence correctly no-ops for a
 *      different-collection supersession.
 */

const MIGRATION_TIMEOUT = 20_000;

test('a collection migration completing after a rapid A->B pane switch refreshes B instead of leaving it stale (BUG-2129)', async ({
	page,
	fixture,
	request,
}, testInfo) => {
	test.skip(
		testInfo.project.name !== 'desktop-chromium',
		'switch-safety is viewport-agnostic; one project is enough',
	);
	test.setTimeout(60_000);

	const coll = await seedMigratableCollection(fixture, request, 'bug2129-switch', 'BGSW');
	// Both items start with status=draft. B also carries a distinguishable
	// `category` so we can later prove the clobber-check edit lands on B,
	// not A.
	const a = await seedItem(fixture, request, coll.slug, 'BUG-2129 switch A', { status: 'draft' });
	const b = await seedItem(fixture, request, coll.slug, 'BUG-2129 switch B', {
		status: 'draft',
		category: 'pre-migration',
	});

	await browserLogin(page);
	await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/${coll.slug}?item=${a.slug}`);

	const pane = page.locator('.item-pane');
	await expect(pane.locator('.title', { hasText: /BUG-2129 switch A/ })).toBeVisible();

	// Hold the collection-update PATCH open until the test explicitly lets
	// it through — this is what lets us deterministically land the switch
	// to B WHILE the migration is still in flight. GETs to the same
	// endpoint (the page's own collection load) pass straight through.
	let releaseMigration = () => {};
	const migrationGate = new Promise<void>((resolve) => {
		releaseMigration = resolve;
	});
	await page.route(`**/api/v1/workspaces/*/collections/${coll.slug}`, async (route) => {
		if (route.request().method() !== 'PATCH') {
			await route.continue();
			return;
		}
		await migrationGate;
		await route.continue();
	});

	// Open Quick Actions -> Manage actions -> Edit Collection modal, Fields tab.
	await pane.locator('button.trigger-btn[title="Quick actions"]').click();
	await pane.locator('button.action-item.footer-row', { hasText: 'Manage actions' }).click();
	await expect(page.locator('#edit-collection-title')).toBeVisible();
	await page.locator('button.tab', { hasText: 'Fields' }).click();

	// Rename the Status field's "draft" option — this is what makes the
	// server treat the save as a migration (buildMigrations() in
	// EditCollectionModal.svelte diffs original vs. edited select options).
	const statusCard = await fieldCardByLabel(page, 'Status');
	const draftOptionInput = await locatorByInputValue(
		statusCard.locator('.option-row .option-name-input'),
		'draft',
	);
	await draftOptionInput.fill('draft-migrated');

	const patchSeen = page.waitForRequest(
		(r) => r.url().endsWith(`/collections/${coll.slug}`) && r.method() === 'PATCH',
	);
	await page.locator('button.btn-save', { hasText: 'Save Changes' }).click();
	await patchSeen;

	// Close the modal WITHOUT waiting for the (held) save to resolve — a
	// user glancing away mid-save. The modal component itself keeps
	// running its pending `handleSave()`; only its DOM/visibility close.
	await page.locator('button.btn-cancel', { hasText: 'Cancel' }).click();
	await expect(page.locator('#edit-collection-title')).toHaveCount(0);

	// Switch the pane to B — a client-side `?item=` re-target, not a full
	// navigation. This is the moment the modal's still-pending save's
	// `onupdated` closure becomes "superseded".
	await page.locator('a.item-card', { hasText: 'BUG-2129 switch B' }).first().click();
	await expect(pane.locator('.title', { hasText: /BUG-2129 switch B/ })).toBeVisible();

	// Register the post-migration reload wait BEFORE releasing the gate, so
	// there's no window where the response could land un-observed. Matched
	// on the response BODY's item id (not slug/ref, which differ) so it can
	// only be satisfied by a GET that actually re-fetched B.
	const reloadResponse = page.waitForResponse(
		async (r) => {
			if (r.request().method() !== 'GET' || !r.url().includes('/items/')) return false;
			try {
				const body = await r.json();
				return body.id === b.id;
			} catch {
				return false;
			}
		},
		{ timeout: MIGRATION_TIMEOUT },
	);

	releaseMigration();

	// If BUG-2129's gap regresses, the switch fence drops the superseded
	// callback unconditionally and this reload never fires -> honest
	// timeout failure here, not a silent false pass.
	const reloaded = await reloadResponse;
	const reloadedItem = await reloaded.json();
	const reloadedFields = JSON.parse(reloadedItem.fields);
	expect(
		reloadedFields.status,
		'B should have reloaded post-migration fields (the fix), not stayed on the stale pre-migration snapshot',
	).toBe('draft-migrated');

	// Now the clobber check: edit an UNRELATED field on B. `updateField()`
	// serializes B's entire local `fields` object back to the server
	// (ItemDetail.svelte). Pre-fix, B's local `fields.status` would still
	// be the stale "draft" (the reload above never happened), so this PATCH
	// would silently clobber the server's migrated "draft-migrated" back to
	// "draft". Post-fix, B's local fields already reflect the migration, so
	// the round-trip preserves it.
	const categoryRow = pane.locator('.field-row:has(.field-label:text-is("Category"))');
	const categoryInput = categoryRow.locator('input.field-input');
	await expect(categoryInput).toHaveValue('pre-migration');

	// waitForResponse (not waitForRequest) — we need the PATCH to actually
	// COMMIT server-side before the belt-and-suspenders readback below,
	// not just be dispatched, or that GET can race an in-flight write and
	// intermittently observe pre-PATCH data (Codex round 2).
	const fieldPatch = page.waitForResponse(
		(r) => r.url().endsWith(`/items/${b.id}`) && r.request().method() === 'PATCH',
	);
	await categoryInput.fill('post-migration-edit');
	await categoryInput.blur();
	const patchRes = await fieldPatch;
	expect(patchRes.ok()).toBe(true);
	const patchBody = JSON.parse(patchRes.request().postData() ?? '{}');
	const patchFields = JSON.parse(patchBody.fields ?? '{}');
	expect(
		patchFields.status,
		'editing an unrelated field on B must not clobber the migrated status back to the stale value',
	).toBe('draft-migrated');
	expect(patchFields.category).toBe('post-migration-edit');

	// Belt-and-suspenders: read B back from the server and confirm neither
	// value was clobbered by the round-trip.
	const finalItem = await request.get(
		`/api/v1/workspaces/${fixture.workspaceSlug}/items/${b.slug}`,
		{ headers: { Authorization: `Bearer ${fixture.apiToken}` } },
	);
	expect(finalItem.ok()).toBe(true);
	const finalFields = JSON.parse((await finalItem.json()).fields);
	expect(finalFields.status).toBe('draft-migrated');
	expect(finalFields.category).toBe('post-migration-edit');

	// A (never re-opened) also carries the migration server-side, proving
	// the migration itself was collection-wide, not a fluke of B's reload.
	const finalA = await request.get(`/api/v1/workspaces/${fixture.workspaceSlug}/items/${a.slug}`, {
		headers: { Authorization: `Bearer ${fixture.apiToken}` },
	});
	expect(JSON.parse((await finalA.json()).fields).status).toBe('draft-migrated');
});

test('a collection migration completing after a cross-collection navigation does not hijack the new page (BUG-2129 fence)', async ({
	page,
	fixture,
	request,
}, testInfo) => {
	test.skip(
		testInfo.project.name !== 'desktop-chromium',
		'switch-safety is viewport-agnostic; one project is enough',
	);
	test.setTimeout(60_000);

	const collA = await seedMigratableCollection(fixture, request, 'bug2129-xcoll-a', 'BGXA');
	const collB = await seedMigratableCollection(fixture, request, 'bug2129-xcoll-b', 'BGXB');
	const a = await seedItem(fixture, request, collA.slug, 'BUG-2129 xcoll A', { status: 'draft' });
	const bTitle = `BUG-2129 xcoll B ${Date.now()}`;
	const b = await seedItem(fixture, request, collB.slug, bTitle, { status: 'draft' });

	// Link A -> B so A's full-page view renders a clickable relationship
	// link into B — the vehicle for a REAL client-side SvelteKit navigation
	// (not page.goto, which would hard-reload and reset all component
	// state, defeating the same-instance-reuse race this test targets).
	const linkResp = await request.post(
		`/api/v1/workspaces/${fixture.workspaceSlug}/items/${a.slug}/links`,
		{ headers: authHeaders(fixture), data: { target_id: b.id, link_type: 'related' } },
	);
	if (!linkResp.ok()) throw new Error(`link create failed (${linkResp.status()}): ${await linkResp.text()}`);

	await browserLogin(page);
	// The FULL-PAGE (non-embedded) item route. `[collection]/[slug]/+page.svelte`
	// passes collSlug/ref straight through as reactive props with no {#key} —
	// SvelteKit reuses the SAME <ItemDetail> instance across a same-route
	// client-side navigation to a DIFFERENT collection/item, exactly like the
	// pane's no-remount reuse (PLAN-2105/TASK-2112's invariant, just on the
	// full-page host instead of the split pane).
	await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/${collA.slug}/${a.slug}`);
	await expect(page.locator('.title', { hasText: /BUG-2129 xcoll A/ })).toBeVisible();

	let releaseMigration = () => {};
	const migrationGate = new Promise<void>((resolve) => {
		releaseMigration = resolve;
	});
	await page.route(`**/api/v1/workspaces/*/collections/${collA.slug}`, async (route) => {
		if (route.request().method() !== 'PATCH') {
			await route.continue();
			return;
		}
		await migrationGate;
		await route.continue();
	});

	await page.locator('button.trigger-btn[title="Quick actions"]').click();
	await page.locator('button.action-item.footer-row', { hasText: 'Manage actions' }).click();
	await expect(page.locator('#edit-collection-title')).toBeVisible();
	await page.locator('button.tab', { hasText: 'Fields' }).click();

	const statusCard = await fieldCardByLabel(page, 'Status');
	const draftOptionInput = await locatorByInputValue(
		statusCard.locator('.option-row .option-name-input'),
		'draft',
	);
	await draftOptionInput.fill('draft-migrated');

	const patchSeen = page.waitForRequest(
		(r) => r.url().endsWith(`/collections/${collA.slug}`) && r.method() === 'PATCH',
	);
	await page.locator('button.btn-save', { hasText: 'Save Changes' }).click();
	await patchSeen;

	await page.locator('button.btn-cancel', { hasText: 'Cancel' }).click();
	await expect(page.locator('#edit-collection-title')).toHaveCount(0);

	// Navigate A -> B via the relationship link — a real client-side
	// SvelteKit transition (same route component, different params), while
	// A's collection-edit save is still pending.
	await page
		.locator('.relationship-group', { hasText: 'Related' })
		.locator('a.link-target', { hasText: bTitle })
		.click();
	await expect(page.locator('.title', { hasText: bTitle })).toBeVisible();
	await expect(page).toHaveURL(new RegExp(`/${collB.slug}/`));

	const urlAtRelease = page.url();
	releaseMigration();

	// Give the (possibly buggy) navigation a moment to fire — long enough
	// that a wrongful `goto()` would have landed, short enough to keep the
	// suite fast. If BUG-2129's fence regresses to the non-functional
	// `{@const keyedSlug}` comparison, this callback ALWAYS falls through to
	// the "not superseded" branch and, since the edited collection's slug
	// (collA) differs from the CURRENTLY-shown collSlug (collB), unconditionally
	// calls handleNavigateAway(`/{username}/{ws}/{collA.slug}/{itemSlug}`) —
	// hard-redirecting off B's page to a broken URL (B's ref doesn't exist
	// in collA).
	await page.waitForTimeout(1500);

	expect(
		page.url(),
		'a superseded, different-collection collection-edit must not navigate the page away from what the user is currently viewing',
	).toBe(urlAtRelease);
	await expect(page.locator('.title', { hasText: bTitle })).toBeVisible();

	// Belt-and-suspenders: the migration itself DID apply server-side
	// (proves the held PATCH actually completed, not just got dropped).
	const finalA = await request.get(`/api/v1/workspaces/${fixture.workspaceSlug}/items/${a.slug}`, {
		headers: { Authorization: `Bearer ${fixture.apiToken}` },
	});
	expect(JSON.parse((await finalA.json()).fields).status).toBe('draft-migrated');
});
