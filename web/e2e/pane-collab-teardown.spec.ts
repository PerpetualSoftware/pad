import { test, expect } from './fixtures';
import { browserLogin, seedDoc, EDITOR_SELECTOR, SYNCED_BADGE_SELECTOR } from './lib/collab-helpers';

/**
 * Split-pane collab teardown e2e (PLAN-2105 Phase 3 / TASK-2117).
 *
 * The Asana-style split pane mounts the shared <ItemDetail> collab editor
 * in a right-docked pane on the collection page and switches items by
 * changing the `?item=` query param — reusing the mounted component
 * instead of a full-page navigation. That reuse creates teardown paths
 * the full-page route never exercised. This spec drives them through the
 * real embedded UI and proves the edit reaches items.content:
 *
 *   1. FULL pane-close (unmount): typing then closing the pane must
 *      mirror the edit into items.content via the teardown flush in the
 *      collab $effect cleanup as <ItemDetail> unmounts.
 *   2. Item-switch (A->B re-key): switching must flush the outgoing item's
 *      edit AND tear down the old provider's WebSocket (no leak).
 *   3. Raw-markdown switch: switching mid raw-save-debounce must flush the
 *      outgoing raw edit (loadData keepalive flush before clearPending).
 *
 * NOT FALSE-PASSING — the key design constraint. These edits are ALSO
 * persisted by timer-based backups (the 5s collab idle flush; the 1.2s
 * raw-save debounce), so a naive test passes even if the teardown flush
 * is removed — a backup timer fires later and masks the regression (Codex
 * review, TASK-2117; verified by mutation testing). Two mechanisms keep
 * these honest:
 *
 *   - Item-switch + raw-switch: loadData() CANCELS the outgoing item's
 *     idle timer / raw debounce on switch, so the teardown/keepalive flush
 *     is the ONLY path that can persist the outgoing edit. Revert the
 *     flush and the item never persists → the test's content poll fails.
 *   - Pane-close: loadData does NOT run (no new item), so the 5s idle
 *     timer stays armed and would fire ~5s later. We therefore assert the
 *     collab-snapshot PATCH is DISPATCHED within 3s of the EDIT (anchored
 *     before the close click, so a stall can't shift the reference) — the
 *     teardown flush fires synchronously on close, whereas the idle timer
 *     can't dispatch until ~5s after the edit. Revert the teardown flush
 *     and the only PATCH is the late idle one → the 3s bound fails.
 *
 * The item-switch and raw-switch tests additionally assert the switch
 * happened within the timer window (edit-anchored), so a pathological
 * stall that let the backup timer fire first yields an HONEST failure
 * rather than a silent false-pass.
 *
 * We register the marker-scoped PATCH wait BEFORE typing so the content
 * assertion pins to the flush that wrote OUR text. Explicit 20s waits on
 * "Synced" cover a slow handshake + reconnect backoff (default expect
 * timeout is 5s).
 */

const COLLAB_WS_RE = /\/api\/v1\/collab\//;
const COLLAB_SNAPSHOT_RE = /source=collab-snapshot/;
const SYNC_TIMEOUT = 20_000;
// The teardown flush dispatches its PATCH synchronously on close; the 5s
// idle backup can't dispatch until ~5s after the edit. 3s cleanly
// separates them (a local keepalive PATCH dispatches in well under 1s).
const TEARDOWN_DISPATCH_BUDGET = 3_000;

test('closing the split pane flushes the edit to items.content on unmount (not the 5s idle backup)', async ({
	page,
	fixture,
	request,
}, testInfo) => {
	// The collab chain is viewport-agnostic; one project is enough and
	// running both doubles WS load on the single test instance.
	test.skip(
		testInfo.project.name !== 'desktop-chromium',
		'collab teardown is viewport-agnostic; one project is enough',
	);
	test.setTimeout(60_000);

	const { slug } = await seedDoc(fixture, request, 'Pane close teardown');
	const marker = `pane-close-${Date.now()}`;

	await browserLogin(page);
	await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/docs?item=${slug}`);

	const editor = page.locator(EDITOR_SELECTOR);
	await expect(editor).toBeVisible({ timeout: SYNC_TIMEOUT });
	await expect(page.locator(SYNCED_BADGE_SELECTOR)).toBeVisible({ timeout: SYNC_TIMEOUT });

	// Content check: a collab-snapshot PATCH whose response body carries
	// OUR marker (proves items.content actually got the typed text).
	const flushed = page.waitForResponse(
		async (r) =>
			COLLAB_SNAPSHOT_RE.test(r.url()) &&
			r.request().method() === 'PATCH' &&
			r.ok() &&
			(await r.text()).includes(marker),
		{ timeout: 30_000 },
	);

	await editor.click();
	await page.keyboard.type(marker);
	// Anchor timing to the EDIT, not the close click. The idle backup arms
	// on the last keystroke, so `editAt` is captured HERE — immediately
	// after typing and BEFORE the awaited toContainText — so a stall in that
	// assertion can't push the reference forward and let the ~5s idle look
	// prompt. From this anchor a reverted teardown flush can only dispatch
	// at ~editAt+5s, which exceeds the 3s budget below.
	const editAt = Date.now();
	await expect(editor).toContainText(marker);

	// The collab-snapshot PATCH carrying OUR marker must DISPATCH promptly
	// after the edit (the teardown flush fires synchronously on close), not
	// ~5s later (the idle backup). Marker-scoped so a stray/other flush
	// can't satisfy it.
	const flushDispatched = page.waitForRequest(
		(r) =>
			COLLAB_SNAPSHOT_RE.test(r.url()) &&
			r.method() === 'PATCH' &&
			(r.postData()?.includes(marker) ?? false),
		{ timeout: 30_000 },
	);

	await page.locator('[aria-label="Close pane"]').first().click();
	await flushDispatched;
	const dispatchMs = Date.now() - editAt;
	expect(
		dispatchMs,
		`teardown flush should dispatch on close (${dispatchMs}ms after edit), not via the ~5s idle backup`,
	).toBeLessThan(TEARDOWN_DISPATCH_BUDGET);

	await expect(editor).toHaveCount(0);
	await flushed;

	// Belt-and-suspenders: read items.content back and confirm the marker.
	const item = await request.get(
		`/api/v1/workspaces/${fixture.workspaceSlug}/items/${slug}`,
		{ headers: { Authorization: `Bearer ${fixture.apiToken}` } },
	);
	expect(item.ok()).toBe(true);
	expect(await item.text()).toContain(marker);
});

test('switching items in the pane tears down the old collab socket and flushes the outgoing edit', async ({
	page,
	fixture,
	request,
}, testInfo) => {
	test.skip(
		testInfo.project.name !== 'desktop-chromium',
		'collab teardown is viewport-agnostic; one project is enough',
	);
	test.setTimeout(60_000);

	// Two docs: A opened in the pane, B switched-to by a list row-click.
	const a = await seedDoc(fixture, request, 'Pane switch A');
	const b = await seedDoc(fixture, request, 'Pane switch B');
	const markerA = `pane-switch-a-${Date.now()}`;

	// Track live collab WebSockets. Provider.destroy() on switch must close
	// the outgoing socket so they never accumulate.
	const liveSockets = new Set<unknown>();
	page.on('websocket', (ws) => {
		if (!COLLAB_WS_RE.test(ws.url())) return;
		liveSockets.add(ws);
		ws.on('close', () => liveSockets.delete(ws));
	});

	await browserLogin(page);
	await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/docs?item=${a.slug}`);

	const editor = page.locator(EDITOR_SELECTOR);
	await expect(editor).toBeVisible({ timeout: SYNC_TIMEOUT });
	await expect(page.locator(SYNCED_BADGE_SELECTOR)).toBeVisible({ timeout: SYNC_TIMEOUT });
	await expect.poll(() => liveSockets.size).toBe(1);

	// A's edit flush wait (marker-scoped). loadData() cancels A's idle timer
	// on the switch, so the switch's cleanup flush is the ONLY path that can
	// persist this — no idle backup to mask a reverted flush.
	const flushedA = page.waitForResponse(
		async (r) =>
			COLLAB_SNAPSHOT_RE.test(r.url()) &&
			r.request().method() === 'PATCH' &&
			r.ok() &&
			(await r.text()).includes(markerA),
		{ timeout: 30_000 },
	);

	await editor.click();
	await page.keyboard.type(markerA);
	// Anchor to the EDIT (the keystroke that armed A's 5s idle timer),
	// captured BEFORE the awaited toContainText so a stall there can't
	// consume the idle window while the measured switch interval still
	// looks prompt (same reasoning as the pane-close test).
	const editAt = Date.now();
	await expect(editor).toContainText(markerA);

	// Switch A->B IN-PANE via a list row-click (client-side re-target of
	// ?item=, no full navigation). `?item=` re-targets to B's issue id
	// (itemUrlId), not its slug, so we assert the param changed AWAY from A.
	const itemParam = () => new URL(page.url()).searchParams.get('item');
	await page.locator('a.item-card', { hasText: 'Pane switch B' }).first().click();

	// Isolation premise: the switch must beat A's 5s idle timer, so that
	// loadData()'s cancel leaves the cleanup flush as the SOLE path that can
	// persist A. If a pathological runner stall let the idle fire first, the
	// test can't isolate the fix — fail honestly rather than false-pass.
	expect(
		Date.now() - editAt,
		'switch must happen before the 5s idle timer to isolate the cleanup flush',
	).toBeLessThan(4_500);

	// The switch must have flushed A's edit before swapping to B.
	await flushedA;

	// B's editor re-synced, the pane re-targeted off A, and the socket
	// count settled back to exactly one — A's provider socket was
	// destroyed, not leaked.
	await expect(page.locator(SYNCED_BADGE_SELECTOR)).toBeVisible({ timeout: SYNC_TIMEOUT });
	await expect.poll(itemParam).not.toBe(a.slug);
	expect(itemParam()).toBeTruthy();
	await expect.poll(() => liveSockets.size, { timeout: 10_000 }).toBe(1);

	// Closing the pane returns the socket count to baseline (no leak).
	await page.locator('[aria-label="Close pane"]').first().click();
	await expect(editor).toHaveCount(0);
	await expect.poll(() => liveSockets.size, { timeout: 10_000 }).toBe(0);

	const readContent = async (slug: string) => {
		const r = await request.get(
			`/api/v1/workspaces/${fixture.workspaceSlug}/items/${slug}`,
			{ headers: { Authorization: `Bearer ${fixture.apiToken}` } },
		);
		expect(r.ok()).toBe(true);
		return r.text();
	};

	// A's edit is durable in items.content...
	expect(await readContent(a.slug)).toContain(markerA);
	// ...and it did NOT leak into B (the no-{#key} cross-write class this
	// pane design is prone to — A's flush must target A, never the swapped-in B).
	expect(await readContent(b.slug)).not.toContain(markerA);
});

test('switching items mid raw-markdown debounce flushes the outgoing raw edit (no data loss)', async ({
	page,
	fixture,
	request,
}, testInfo) => {
	test.skip(
		testInfo.project.name !== 'desktop-chromium',
		'collab teardown is viewport-agnostic; one project is enough',
	);
	test.setTimeout(60_000);

	// In RAW markdown mode there is NO collab cleanup flush (collabKey is
	// null), so switching A->B relies on loadData flushing the raw saver
	// (keepalive) before clearPending(). loadData also CANCELS the 1.2s raw
	// debounce on the switch, so that keepalive flush is the ONLY path that
	// can persist A — no debounce backup to mask a reverted flush.
	const a = await seedDoc(fixture, request, 'Raw switch A');
	const b = await seedDoc(fixture, request, 'Raw switch B');
	const markerA = `raw-switch-a-${Date.now()}`;

	await browserLogin(page);
	await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/docs?item=${a.slug}`);

	await expect(page.locator(EDITOR_SELECTOR)).toBeVisible({ timeout: SYNC_TIMEOUT });
	await expect(page.locator(SYNCED_BADGE_SELECTOR)).toBeVisible({ timeout: SYNC_TIMEOUT });

	// Toggle A into raw markdown mode and type — arms the 1.2s raw-save
	// debounce (which the switch will cancel).
	await page.locator('button[title="Raw markdown editor"]').first().click();
	const raw = page.locator('textarea.raw-textarea');
	await expect(raw).toBeVisible();
	await raw.fill(markerA);
	const fillAt = Date.now();

	// Switch A->B immediately. loadData cancels the debounce and the
	// keepalive flush must persist A's edit.
	await page.locator('a.item-card', { hasText: 'Raw switch B' }).first().click();

	// Isolation premise: the switch must beat the 1.2s raw debounce, so the
	// keepalive flush is the SOLE path that can persist A. If a pathological
	// runner stall let the debounce fire first, the test can't isolate the
	// fix — fail honestly rather than false-pass.
	expect(
		Date.now() - fillAt,
		'switch must happen before the 1.2s raw debounce to isolate the keepalive flush',
	).toBeLessThan(1_000);
	await expect.poll(() => new URL(page.url()).searchParams.get('item')).not.toBe(a.slug);

	const readContent = async (slug: string) => {
		const r = await request.get(
			`/api/v1/workspaces/${fixture.workspaceSlug}/items/${slug}`,
			{ headers: { Authorization: `Bearer ${fixture.apiToken}` } },
		);
		expect(r.ok()).toBe(true);
		return r.text();
	};

	// A's raw edit survived the switch (keepalive PATCH is fire-and-forget,
	// so poll until it lands)...
	await expect
		.poll(async () => (await readContent(a.slug)).includes(markerA), { timeout: 15_000 })
		.toBe(true);
	// ...and did not cross-write into B.
	expect(await readContent(b.slug)).not.toContain(markerA);
});
