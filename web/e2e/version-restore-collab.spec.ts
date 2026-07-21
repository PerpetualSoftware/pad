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
 * the restore. So we deterministically SUPPRESS the idle flush — a route gate
 * ABORTS every `source=collab-snapshot` PATCH until we explicitly allow it,
 * immediately before the Confirm-Restore click — so the ONLY gamma collab-snapshot
 * PATCH that can land is the pre-restore one. That makes both assertions airtight:
 * (1) items.content can only acquire gamma via the pre-restore flush, so the
 * undo-point holding gamma proves the fix; and (2) a monotonic event sequence
 * (array index, not ms timestamps) shows the successful gamma flush response is
 * dispatched STRICTLY before the restore POST is issued (the client awaited it).
 * Under the bug the restore POST fires with no preceding gamma flush, so both go
 * red.
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

	// P3 (Codex): deterministically SUPPRESS the ordinary 5s idle collab-snapshot
	// flush so the ONLY gamma-carrying collab-snapshot PATCH that can land is the
	// pre-restore one — making both the outcome and the ordering assertions
	// airtight (no reliance on "reach the click under 5s"). A route gate ABORTS
	// collab-snapshot PATCHes until we explicitly allow them, immediately before
	// the Confirm-Restore click. The earlier beta flush already completed before
	// this gate is installed, so it's unaffected; hydration after the reload does
	// no edit, so it issues no flush to abort.
	let allowSnapshotFlush = false;
	await page.route(/source=collab-snapshot/, async (route) => {
		if (route.request().method() === 'PATCH' && !allowSnapshotFlush) {
			await route.abort();
			return;
		}
		await route.continue();
	});

	// Record a MONOTONIC event sequence (array index, NOT ms Date.now()) so the
	// ordering compare is exact and tie-free. Only a SUCCESSFUL (2xx) gamma
	// collab-snapshot response and the restore POST request enter the sequence;
	// aborted idle attempts fire 'requestfailed', never 'response', so they can't
	// pollute it. The gamma filter also ignores the earlier beta flush.
	const seq: string[] = [];
	const isGammaFlush = (r: PWRequest) =>
		r.url().includes('source=collab-snapshot') &&
		r.method() === 'PATCH' &&
		(r.postData() ?? '').includes(gamma);
	const isRestorePost = (r: PWRequest) =>
		/\/versions\/[^/]+\/restore$/.test(r.url()) && r.method() === 'POST';
	page.on('response', (resp) => {
		if (isGammaFlush(resp.request()) && resp.ok()) seq.push('gamma-flush-ok');
	});
	page.on('request', (r) => {
		if (isRestorePost(r)) seq.push('restore-start');
	});

	// Type `gamma` — its idle flush is now blocked by the gate, so it stays in the
	// live Y.Doc, unpersisted to items.content (the BUG-2271 window).
	await editor.click();
	await page.keyboard.press('Control+End');
	await page.keyboard.type(' ' + gamma);
	await expect(editor).toContainText(gamma);

	// Sanity: items.content does NOT hold gamma. With the gate aborting every
	// collab-snapshot PATCH, gamma can reach items.content ONLY via the pre-restore
	// flush we allow below — so the undo-point holding gamma is airtight proof that
	// the pre-restore flush (not an idle flush) captured it.
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
	// Allow collab-snapshot PATCHes ONLY from here, so the first (and only) gamma
	// PATCH that lands is the pre-restore flush the confirm click triggers.
	allowSnapshotFlush = true;
	await card.getByRole('button', { name: 'Confirm Restore' }).click();
	await restoreDone;

	// Ordering proof (Codex P3): a SUCCESSFUL gamma collab-snapshot flush occurred
	// AND its response was dispatched STRICTLY BEFORE the restore POST was issued
	// (monotonic index — the client awaited the flush before POSTing the restore).
	// Under the bug the restore POST fires with no preceding gamma flush, so
	// 'gamma-flush-ok' is absent (or after) → red.
	const gammaIdx = seq.indexOf('gamma-flush-ok');
	const restoreIdx = seq.indexOf('restore-start');
	expect(gammaIdx, 'a successful pre-restore gamma flush must occur').toBeGreaterThanOrEqual(0);
	expect(restoreIdx, 'the restore POST must be issued').toBeGreaterThanOrEqual(0);
	expect(gammaIdx).toBeLessThan(restoreIdx);

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
