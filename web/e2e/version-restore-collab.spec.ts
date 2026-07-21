import { test, expect } from './fixtures';
import type { Request as PWRequest } from '@playwright/test';
import { browserLogin, EDITOR_SELECTOR, SYNCED_BADGE_SELECTOR } from './lib/collab-helpers';

/**
 * Version-restore under live collab (BUG-2264).
 *
 * The version-restore handler used to write items.content directly,
 * bypassing the Yjs collab applier. Under a live co-editing session the
 * editor's Y.Doc still held the PRE-restore document, so the next
 * collab-snapshot flush PATCHed that stale markdown back over the
 * restored content — silently undoing the restore.
 *
 * The fix routes the restore's content through the same applier path as
 * a normal content PATCH (server/handlers_item_versions.go): a connected
 * client applies the restored markdown into the shared Y.Doc (propagating
 * to all peers + the op-log), with a plain items.content write only when
 * no collab room is live.
 *
 * This spec drives the real chain through the embedded UI:
 *
 *   seed v1 → open live editor → type v2 (creates a version) → restore v1
 *   while the room is live → type a further edit → assert items.content
 *   reflects the restored v1 (+ the new edit) and was NOT reverted to v2.
 *
 * Under the bug, the restore leaves the live Y.Doc holding v2, so both
 * the immediate editor assertion (v2 still present) and the final
 * persisted-content assertion (v2 leaked back) go red.
 *
 * Determinism notes (mirrors collab-persistence.spec.ts):
 *   - We wait for the "Synced" collab badge before typing so the Y.Doc
 *     binding is live.
 *   - We register the collab-snapshot flush wait (the 5s idle PATCH that
 *     writes items.content) BEFORE each edit and scope it to a response
 *     body carrying the marker, so assertions can't race the write.
 *   - The restore POST blocks server-side until the connected client acks
 *     the applier request, so by the time it resolves the live Y.Doc has
 *     already been reconciled.
 *
 * Why an in-page login (browserLogin): the collab WebSocket handshake
 * can't carry an Authorization header, so the server authenticates it
 * from a same-UA session cookie. See collab-helpers.ts.
 */

test('restoring a version under live collab is not clobbered by the next flush (BUG-2264)', async ({
	page,
	fixture,
	request
}, testInfo) => {
	// The collab chain is viewport-agnostic; one project is enough and
	// running both doubles WS load on the single test instance for no
	// added coverage.
	test.skip(
		testInfo.project.name !== 'desktop-chromium',
		'version-restore collab is viewport-agnostic; one project is enough'
	);
	// Two 5s idle flushes + the WS handshake + the applier round-trip need
	// well over the 30s default on a busy CI runner.
	test.setTimeout(90_000);

	const ws = fixture.workspaceSlug;
	const headers = {
		Authorization: `Bearer ${fixture.apiToken}`,
		'Content-Type': 'application/json'
	};
	const ts = Date.now();
	const alpha = `alpha-restore-${ts}`;
	const beta = `beta-super-${ts}`;
	const gamma = `gamma-after-${ts}`;

	// Seed a doc whose initial (v1) body is `alpha` — the state we later
	// restore back to after typing over it.
	const createResp = await request.post(`/api/v1/workspaces/${ws}/collections/docs/items`, {
		headers,
		data: { title: `Version restore collab ${ts}`, content: alpha }
	});
	if (!createResp.ok())
		throw new Error(`doc create failed (${createResp.status()}): ${await createResp.text()}`);
	const { slug } = (await createResp.json()) as { id: string; slug: string };

	await browserLogin(page);
	await page.goto(`/${fixture.adminUsername}/${ws}/docs/${slug}`);

	const editor = page.locator(EDITOR_SELECTOR);
	await expect(editor).toBeVisible();
	// Y.Doc hydrated from items.content (the seeded v1).
	await expect(editor).toContainText(alpha);
	// Schema-version handshake complete — typing before this races the binding.
	await expect(page.locator(SYNCED_BADGE_SELECTOR)).toBeVisible();

	// Type `beta` — the v2 edit that supersedes v1. Its collab-snapshot
	// flush persists "alpha beta" and (first content change → no throttle)
	// creates a version whose resolved content is the pre-edit v1.
	const betaFlushed = page.waitForResponse(
		async (r) =>
			r.url().includes('source=collab-snapshot') &&
			r.request().method() === 'PATCH' &&
			r.ok() &&
			(await r.text()).includes(beta),
		{ timeout: 30_000 }
	);
	await editor.click();
	await page.keyboard.press('Control+End');
	await page.keyboard.type(' ' + beta);
	await expect(editor).toContainText(beta);
	await betaFlushed;

	// Find the version snapshot holding the pre-beta (v1) state: resolved
	// content contains alpha but not beta.
	const versionsResp = await request.get(
		`/api/v1/workspaces/${ws}/items/${slug}/versions`,
		{ headers }
	);
	expect(versionsResp.ok()).toBeTruthy();
	const versions = (await versionsResp.json()) as Array<{ id: string; content: string }>;
	const v1 = versions.find((v) => v.content.includes(alpha) && !v.content.includes(beta));
	expect(v1, 'expected a version snapshot holding the pre-edit (v1) content').toBeTruthy();

	// Restore v1 while the collab room is LIVE. The POST blocks until the
	// connected client applies the restored markdown into its Y.Doc via the
	// applier protocol (the BUG-2264 fix). Under the bug this wrote
	// items.content directly and the still-live Y.Doc (holding beta) would
	// overwrite it on the next flush.
	const restoreResp = await request.post(
		`/api/v1/workspaces/${ws}/items/${slug}/versions/${v1!.id}/restore`,
		{ headers }
	);
	expect(restoreResp.ok()).toBeTruthy();

	// The restore endpoint must return the RESTORED body synchronously — the
	// UI adopts this response immediately. A metadata-only write on the
	// applier-ack path would echo the still-pre-restore content here.
	const restored = (await restoreResp.json()) as { content: string };
	expect(restored.content).toContain(alpha);
	expect(restored.content).not.toContain(beta);

	// The applier reconciled the live editor: v1 is back, v2 is gone. Under
	// the bug the editor's Y.Doc would still hold beta here.
	await expect(editor).toContainText(alpha);
	await expect(editor).not.toContainText(beta, { timeout: 15_000 });

	// Subsequent edit + flush. If the restore had NOT reconciled the Y.Doc,
	// this flush would persist the stale beta back into items.content.
	const gammaFlushed = page.waitForResponse(
		async (r) =>
			r.url().includes('source=collab-snapshot') &&
			r.request().method() === 'PATCH' &&
			r.ok() &&
			(await r.text()).includes(gamma),
		{ timeout: 30_000 }
	);
	await editor.click();
	await page.keyboard.press('Control+End');
	await page.keyboard.type(' ' + gamma);
	await expect(editor).toContainText(gamma);
	await gammaFlushed;

	// The persisted body reflects the restored v1 (+ the new edit) and was
	// NOT reverted to v2.
	const finalResp = await request.get(`/api/v1/workspaces/${ws}/items/${slug}`, { headers });
	expect(finalResp.ok()).toBeTruthy();
	const finalItem = (await finalResp.json()) as { content: string };
	expect(finalItem.content).toContain(alpha);
	expect(finalItem.content).toContain(gamma);
	expect(finalItem.content).not.toContain(beta);
});

/**
 * Undo-point captures an UNFLUSHED live edit (BUG-2271).
 *
 * A version restore creates a "Restored from…" undo-point version from the
 * CURRENT persisted items.content, server-side, inside the restore tx. If a
 * live collab editor holds edits still sitting in the Y.Doc/op-log within the
 * ~5s flush-debounce window (not yet PATCHed to items.content), those edits are
 * NOT in the undo-point — and, because the restore also prunes the op-log +
 * reseeds every peer (BUG-2264), they are permanently lost.
 *
 * The collab server is a dumb relay with no Yjs decoder, so it can't render the
 * live Y.Doc to markdown. The fix is client-side: the initiating client flushes
 * its live editor markdown into items.content BEFORE issuing the restore, so the
 * undo-point reflects the true pre-restore live state. ItemDetail threads a
 * `flushBeforeRestore` callback down through ItemTimeline → TimelineVersionCard,
 * which awaits it immediately before the restore POST.
 *
 * This spec drives the restore through the REAL UI button (not the API) so the
 * flush-before-restore callback is exercised:
 *
 *   seed alpha → type+flush beta (creates a version) → reload (so the timeline
 *   shows the version card) → type gamma but do NOT wait for its flush → click
 *   Restore in the UI → assert a version now holds gamma (the undo-point).
 *
 * Under the bug, clicking Restore issues no pre-flush, so the undo-point is
 * captured without gamma and gamma is lost when the op-log is pruned — no
 * version ever contains gamma and the poll goes red.
 *
 * Determinism / no false-pass (Codex P3 on #994): the poll alone could pass for
 * the WRONG reason if the ordinary 5s idle flush happened to land gamma before
 * the restore. So we ALSO capture request ordering and assert the fix's causal
 * signature directly: a `source=collab-snapshot` PATCH carrying gamma is
 * INITIATED after the "Confirm Restore" click (i.e. it's the pre-restore flush,
 * not the idle timer) AND its response arrives BEFORE the restore POST is issued
 * (i.e. the client awaited it). Under the bug the restore POST fires immediately
 * on click with no preceding gamma flush, so no gamma PATCH can complete before
 * the restore POST — the ordering assertion goes red. Reaching the click well
 * under 5s keeps the idle timer from firing before the click.
 */
test('an unflushed live edit is captured in the restore undo-point via the UI (BUG-2271)', async ({
	page,
	fixture,
	request
}, testInfo) => {
	test.skip(
		testInfo.project.name !== 'desktop-chromium',
		'version-restore collab is viewport-agnostic; one project is enough'
	);
	// A flush + reload + WS re-handshake + a second flush + restore need well
	// over the 30s default on a busy CI runner.
	test.setTimeout(90_000);

	const ws = fixture.workspaceSlug;
	const headers = {
		Authorization: `Bearer ${fixture.apiToken}`,
		'Content-Type': 'application/json'
	};
	const ts = Date.now();
	const alpha = `alpha-base-${ts}`;
	const beta = `beta-mid-${ts}`;
	const gamma = `gamma-unflushed-${ts}`;

	// Seed a doc whose v1 body is `alpha` — the state we restore back to.
	const createResp = await request.post(`/api/v1/workspaces/${ws}/collections/docs/items`, {
		headers,
		data: { title: `Restore undo-point ${ts}`, content: alpha }
	});
	if (!createResp.ok())
		throw new Error(`doc create failed (${createResp.status()}): ${await createResp.text()}`);
	const { slug } = (await createResp.json()) as { id: string; slug: string };

	await browserLogin(page);
	await page.goto(`/${fixture.adminUsername}/${ws}/docs/${slug}`);

	let editor = page.locator(EDITOR_SELECTOR);
	await expect(editor).toBeVisible();
	await expect(editor).toContainText(alpha);
	await expect(page.locator(SYNCED_BADGE_SELECTOR)).toBeVisible();

	// Type `beta` and let it flush — persists "alpha beta" and (first content
	// change → no throttle) creates the version snapshot we later restore to.
	const betaFlushed = page.waitForResponse(
		async (r) =>
			r.url().includes('source=collab-snapshot') &&
			r.request().method() === 'PATCH' &&
			r.ok() &&
			(await r.text()).includes(beta),
		{ timeout: 30_000 }
	);
	await editor.click();
	await page.keyboard.press('Control+End');
	await page.keyboard.type(' ' + beta);
	await expect(editor).toContainText(beta);
	await betaFlushed;

	// Reload so the timeline renders the version card. Content saves deliberately
	// do NOT auto-refresh the timeline (ItemTimeline only refreshes on
	// comment/reaction events); a natural reload surfaces the new version entry.
	await page.reload();
	editor = page.locator(EDITOR_SELECTOR);
	await expect(editor).toBeVisible();
	await expect(editor).toContainText(beta);
	await expect(page.locator(SYNCED_BADGE_SELECTOR)).toBeVisible();

	// Capture request ordering so we can prove the PRE-RESTORE flush (not the idle
	// timer) is what landed gamma (Codex P3). Record, with timestamps: when a
	// gamma-carrying collab-snapshot PATCH is INITIATED and when its RESPONSE
	// arrives, and when the restore POST is INITIATED. Registered before typing so
	// nothing is missed; the gamma filter ignores the earlier beta flush.
	const order: { kind: 'gamma-flush-start' | 'gamma-flush-done' | 'restore-start'; t: number }[] = [];
	const isGammaFlush = (r: PWRequest) =>
		r.url().includes('source=collab-snapshot') &&
		r.method() === 'PATCH' &&
		(r.postData() ?? '').includes(gamma);
	const isRestorePost = (r: PWRequest) =>
		/\/versions\/[^/]+\/restore$/.test(r.url()) && r.method() === 'POST';
	page.on('request', (r) => {
		if (isGammaFlush(r)) order.push({ kind: 'gamma-flush-start', t: Date.now() });
		else if (isRestorePost(r)) order.push({ kind: 'restore-start', t: Date.now() });
	});
	page.on('response', (resp) => {
		if (isGammaFlush(resp.request())) order.push({ kind: 'gamma-flush-done', t: Date.now() });
	});

	// Type `gamma` but do NOT wait for its idle flush — it stays in the live
	// Y.Doc, unpersisted to items.content (the BUG-2271 window). We reach the
	// Restore click well under 5s so the idle timer can't pre-persist it.
	await editor.click();
	await page.keyboard.press('Control+End');
	await page.keyboard.type(' ' + gamma);
	await expect(editor).toContainText(gamma);

	// Sanity: items.content does NOT yet hold gamma (still the unflushed window).
	const preRestore = await request.get(`/api/v1/workspaces/${ws}/items/${slug}`, { headers });
	expect(preRestore.ok()).toBeTruthy();
	expect((await preRestore.json()).content as string).not.toContain(gamma);

	// Drive the restore through the UI. confirmRestore must FIRST flush the live
	// editor (gamma) into items.content, THEN POST the restore — so the server's
	// undo-point (captured from items.content in the restore tx) includes gamma.
	const restoreDone = page.waitForResponse(
		(r) =>
			/\/versions\/[^/]+\/restore$/.test(r.url()) &&
			r.request().method() === 'POST' &&
			r.ok(),
		{ timeout: 30_000 }
	);
	const card = page.locator('#item-timeline .version-card').first();
	await card.locator('.card-header').click(); // expand
	await card.getByRole('button', { name: 'Restore this version' }).click();
	// Timestamp the click so we can prove the gamma flush was initiated AFTER it
	// (the pre-restore flush) and not by the idle timer beforehand.
	const clickTime = Date.now();
	await card.getByRole('button', { name: 'Confirm Restore' }).click();
	await restoreDone;

	// Ordering proof (Codex P3): the fix's causal signature must hold.
	const gammaStart = order.find((e) => e.kind === 'gamma-flush-start');
	const gammaDone = order.find((e) => e.kind === 'gamma-flush-done');
	const restoreStart = order.find((e) => e.kind === 'restore-start');
	expect(gammaStart, 'a gamma collab-snapshot PATCH must be issued').toBeTruthy();
	expect(gammaDone, 'the gamma flush must complete').toBeTruthy();
	expect(restoreStart, 'the restore POST must be issued').toBeTruthy();
	// (a) The gamma flush was initiated AFTER the Confirm-Restore click → it's the
	//     pre-restore flush, not a stray idle-timer flush that raced ahead.
	expect(gammaStart!.t).toBeGreaterThanOrEqual(clickTime);
	// (b) The gamma flush RESPONSE arrived BEFORE the restore POST was issued →
	//     the client awaited the flush, so items.content held gamma when the
	//     restore captured its undo-point. Under the bug the restore POST fires
	//     first and this is false.
	expect(gammaDone!.t).toBeLessThanOrEqual(restoreStart!.t);

	// And the outcome: the undo-point version created by the restore holds the
	// pre-restore items.content — which, thanks to the pre-restore flush, includes
	// gamma. Under the bug no version ever holds gamma (it was lost with the op-log).
	await expect
		.poll(
			async () => {
				const vr = await request.get(
					`/api/v1/workspaces/${ws}/items/${slug}/versions`,
					{ headers }
				);
				if (!vr.ok()) return false;
				const versions = (await vr.json()) as Array<{ content: string }>;
				return versions.some((v) => v.content.includes(gamma));
			},
			{ timeout: 15_000 }
		)
		.toBe(true);
});
