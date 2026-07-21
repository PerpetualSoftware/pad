import { test, expect } from './fixtures';
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
