import { expect, type APIRequestContext } from '@playwright/test';
import { test, type SuiteFixture } from './fixtures';

/**
 * BUG-2602 — ItemDetail's collection writes raced under fences that
 * ordered STARTS, and loadData's cross-collection escape hatch admitted
 * any generation-stale write that fetched a different collection. A
 * loadData continuation spanning a cross-collection MOVE could therefore
 * restore the SOURCE collection over the freshly adopted TARGET — the
 * live item (itemGen-fenced) kept the move, so the pane rendered the
 * item against the WRONG collection's schema.
 *
 * Deterministic construction (route-hold, same technique as
 * pane-collection-migration-race.spec.ts):
 *
 *   1. An embedded pane is opened via a cross-collection `?item=` — the
 *      route is the suite workspace's `tasks`, the item lives in SRC —
 *      which sends loadData down its realColl branch: `GET
 *      /collections/<src>`. That GET is HELD by route interception.
 *   2. While held, the item is MOVED src → tgt via the API. The pane's
 *      live SSE delivers item_updated; refreshCollectionIfMoved fetches
 *      TGT (not held) and adopts it — the pane renders TGT's schema
 *      (its marker field label).
 *   3. The held SRC fetch is released. The continuation is generation-
 *      stale AND cross-collection: the old hatch admitted it (pane
 *      reverts to SRC's schema — the control build fails here); the
 *      adoptCollection semantic veto rejects it (live item's
 *      collection_id is TGT).
 *
 * The observable is the fields panel, which renders the COLLECTION
 * schema's field labels — src and tgt carry distinct marker fields.
 */

function authHeaders(fixture: SuiteFixture) {
	return { Authorization: `Bearer ${fixture.apiToken}` };
}

async function seedCollection(
	fixture: SuiteFixture,
	request: APIRequestContext,
	name: string,
	prefix: string,
	markerLabel: string,
) {
	const schema = JSON.stringify({
		fields: [
			{ key: 'status', type: 'select', options: ['open', 'done'], default: 'open' },
			{ key: 'marker', label: markerLabel, type: 'text' },
		],
	});
	const resp = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/collections`, {
		headers: authHeaders(fixture),
		data: { name, prefix, schema },
	});
	if (!resp.ok()) throw new Error(`collection create failed (${resp.status()}): ${await resp.text()}`);
	return (await resp.json()) as { id: string; slug: string };
}

test('BUG-2602: a held cross-collection load released after a move cannot revert the adopted target collection', async ({
	page,
	fixture,
	request,
}, testInfo) => {
	test.skip(
		testInfo.project.name !== 'desktop-chromium',
		'switch-safety is viewport-agnostic; one project is enough',
	);
	test.setTimeout(60_000);

	const uniq = `${test.info().workerIndex}${Date.now().toString(36)}`;
	const src = await seedCollection(fixture, request, `b2602src${uniq}`, 'BSRC', 'SrcMarkerField');
	const tgt = await seedCollection(fixture, request, `b2602tgt${uniq}`, 'BTGT', 'TgtMarkerField');

	const itemResp = await request.post(
		`/api/v1/workspaces/${fixture.workspaceSlug}/collections/${src.slug}/items`,
		{
			headers: authHeaders(fixture),
			data: { title: `b2602 race probe ${uniq}`, fields: JSON.stringify({ status: 'open' }), content: '' },
		},
	);
	expect(itemResp.ok(), await itemResp.text()).toBeTruthy();
	const item = (await itemResp.json()) as { id: string; slug: string };

	// A warm-up item IN the route collection: refreshCollectionIfMoved
	// (and the real-world race) requires the pane to already hold a
	// collection snapshot — a cold pane with `collection === null` bails
	// out of the move handling before the fetch this spec gates on.
	const warmResp = await request.post(
		`/api/v1/workspaces/${fixture.workspaceSlug}/collections/tasks/items`,
		{
			headers: authHeaders(fixture),
			data: { title: `b2602 warmup ${uniq}`, fields: JSON.stringify({}), content: '' },
		},
	);
	expect(warmResp.ok(), await warmResp.text()).toBeTruthy();
	const warm = (await warmResp.json()) as { slug: string };

	// Hold loadData's realColl fetch for SRC. GETs only; the move below
	// must still be able to touch anything else.
	let releaseSrc = () => {};
	const srcGate = new Promise<void>((resolve) => {
		releaseSrc = resolve;
	});
	let heldCount = 0;
	await page.route(`**/api/v1/workspaces/${fixture.workspaceSlug}/collections/${src.slug}`, async (route) => {
		if (route.request().method() !== 'GET') {
			await route.continue();
			return;
		}
		heldCount++;
		await srcGate;
		await route.continue();
	});

	// Warm the pane on the route collection's own item so `collection`
	// is loaded (tasks), then switch to the cross-collection SRC item —
	// loadData(src item) takes the realColl branch (the held GET) while
	// the pane keeps its persistent (no {#key}) state.
	await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/tasks?item=${warm.slug}`);
	const pane = page.locator('.item-pane');
	await expect(pane.locator('.title', { hasText: /b2602 warmup/ })).toBeVisible();

	// The switch: same route, new ?item= — no remount.
	// NOTE: from here until release, the pane shows a loading state, so
	// the mid-hold oracles below are network-level.
	await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/tasks?item=${item.slug}`);

	// PRECONDITION: the hold actually caught loadData's realColl fetch —
	// otherwise the race below is vacuous. Reaching that fetch also
	// means loadData already adopted the item (item precedes collection
	// in loadData), so the SSE move handling below has an item to match.
	await expect.poll(() => heldCount, { timeout: 15_000 }).toBeGreaterThan(0);

	// The MOVE, while the SRC fetch is held. The pane's live SSE delivers
	// item_updated and the pane refetches the item by slug — that
	// response (carrying the moved item) is the deterministic "move
	// processed" oracle; the pane UI can't serve as one while loading.
	// (On this freshly-switched pane `collection` is still null, so
	// refreshCollectionIfMoved's `!collection` guard self-skips — the
	// convergence happens on the veto path after release.)
	const paneRefetch = page.waitForResponse(
		(r) =>
			r.url().endsWith(`/items/${item.slug}`) &&
			r.request().method() === 'GET' &&
			r.ok(),
		{ timeout: 15_000 },
	);
	const moveResp = await request.post(
		`/api/v1/workspaces/${fixture.workspaceSlug}/items/${item.slug}/move`,
		{ headers: authHeaders(fixture), data: { target_collection: tgt.slug, source: 'cli' } },
	);
	expect(moveResp.ok(), await moveResp.text()).toBeTruthy();
	await paneRefetch;
	// Small settle: the item adopt happens just after the response body
	// is consumed.
	await page.waitForTimeout(300);

	// RELEASE the stale SRC continuation — the race under test. loadData
	// then completes and the pane renders with whichever collection won.
	releaseSrc();

	// The veto must hold: the pane renders the live item's collection
	// (TGT). On the control build the released continuation wins via the
	// old escape hatch and SrcMarkerField renders instead.
	await expect(pane.locator('.title', { hasText: /b2602 race probe/ })).toBeVisible({
		timeout: 15_000,
	});
	await expect(pane.getByText('TgtMarkerField')).toBeVisible({ timeout: 10_000 });
	await expect(pane.getByText('SrcMarkerField')).not.toBeVisible();
});
