import { expect, type Request } from '@playwright/test';
import { test, quietCrossActorToasts } from './fixtures';

/**
 * BUG-2539 — a bulk archive landing in the same wall-clock second as the page's
 * sync cursor was invisible to `/changes`, leaving the item rendered LIVE
 * indefinitely (see internal/store/items.go::ItemsModifiedSince).
 *
 * The deterministic pin for the cursor arithmetic is the store-level
 * `TestItemsModifiedSince_SameSecondCursor`. This spec covers the user-visible
 * end of the same defect: the archived banner appears in the page that was
 * already open, without a reload.
 *
 * Three things must hold together, or the banner is satisfiable by a mechanism
 * that is not the one under test:
 *
 *   1. the page must have loaded a LIVE row, on a request it issued BEFORE the
 *      archive — otherwise it simply rendered an already-archived item;
 *   2. the deletion must arrive on a `/changes` that is NOT the page's first
 *      one — the first is the cursor seed `setWorkspace` fires on mount, which
 *      can carry the deletion once the fix is in and proves nothing about the
 *      incremental path;
 *   3. only then does the banner count.
 *
 * Both (1) and (2) are decided by REQUEST ORDER, not by response timing.
 * Response-arrival ordering is genuinely racy here: the server publishes the
 * SSE event before the bulk handler finishes writing its own HTTP response, so
 * the incremental `/changes` can resolve before the archive call returns
 * (Codex P2 on the first attempt at this oracle, which timed responses against
 * archive completion and could both flake and false-pass).
 *
 * Ground truth for archived-ness is the server's `deleted_at`, never the UI:
 * asserting on the UI alone cannot distinguish "not archived" from "archived
 * but not shown", which is the failure mode under test.
 *
 * Two legs. 450ms after navigation start is the offset that reproduced 7/8
 * times before the fix (the archive lands just after the SSE connects and
 * inside the cursor's own second); 1200ms is the control that always behaved.
 * Both are deterministic AFTER the fix — the comparison no longer depends on
 * where inside a second the mutation fell.
 */

// Offsets are measured from NAVIGATION START, not first paint — anchoring to
// paint pushes the archive clear of the window and the bug does not reproduce.
const DELAYS = [450, 1200];

DELAYS.forEach((delay, idx) => {
	test(`BUG-2539: bulk archive ${delay}ms after navigation start reaches the open item`, async ({
		page,
		context,
		request,
		fixture
	}) => {
		await quietCrossActorToasts(context);

		// Own workspace: the shared suite workspace carries cross-spec mutation
		// traffic that muddies a cursor-sensitive leg.
		const slug = `b2539-${idx}-${Date.now().toString(36)}`;
		const auth = { Authorization: `Bearer ${fixture.apiToken}` };
		const wsResp = await request.post('/api/v1/workspaces', {
			headers: auth,
			data: { name: `BUG-2539 ${delay}`, slug, template: 'startup' }
		});
		expect(wsResp.ok(), await wsResp.text()).toBeTruthy();
		const ws = (await wsResp.json()) as { slug: string };

		try {
			const itemResp = await request.post(
				`/api/v1/workspaces/${ws.slug}/collections/tasks/items`,
				{ headers: auth, data: { title: `sync-window probe ${delay}` } }
			);
			expect(itemResp.ok(), await itemResp.text()).toBeTruthy();
			const item = (await itemResp.json()) as { id: string; slug: string };

			// Request-side bookkeeping. `archiveSentAt` is stamped before the POST
			// is issued, so "issued before the archive" is decidable without
			// depending on when anything resolved.
			let archiveSentAt = Number.POSITIVE_INFINITY;
			const changesOrder = new Map<Request, number>();
			const itemGetIssuedAt = new Map<Request, number>();
			let changesSeen = 0;
			const itemGetRe = /\/api\/v1\/workspaces\/[^/]+\/items\/[^/?]+$/;

			page.on('request', (r) => {
				if (r.url().includes('/changes')) {
					changesOrder.set(r, changesSeen++);
				} else if (itemGetRe.test(r.url()) && r.method() === 'GET') {
					itemGetIssuedAt.set(r, Date.now());
				}
			});

			// (1) the page's own pre-archive item load saw a LIVE row
			let loadedLiveBeforeArchive = false;
			// (2) a NON-SEED /changes carried the deletion
			let incrementalCarriedDeletion = false;
			// Response handlers are async; collect them so the assertions below
			// can await the ones already in flight instead of racing them.
			const inFlight: Promise<void>[] = [];
			page.on('response', (resp) => {
				inFlight.push(
					(async () => {
						try {
							if (resp.url().includes('/changes')) {
								const order = changesOrder.get(resp.request());
								const body = (await resp.json()) as { deleted?: string[] };
								if (order !== undefined && order > 0 && body.deleted?.includes(item.id)) {
									incrementalCarriedDeletion = true;
								}
							} else if (itemGetRe.test(resp.url()) && resp.request().method() === 'GET') {
								const issued = itemGetIssuedAt.get(resp.request());
								const body = (await resp.json()) as { id?: string; deleted_at?: string | null };
								if (body.id === item.id && !body.deleted_at && issued !== undefined && issued < archiveSentAt) {
									loadedLiveBeforeArchive = true;
								}
							}
						} catch {
							/* non-JSON body — ignore */
						}
					})()
				);
			});

			const itemUrl = `/${fixture.adminUsername}/${ws.slug}/tasks/${item.id}`;
			// Fire the archive on a timer started at navigation start WITHOUT
			// awaiting the page — awaiting first paint moves the archive out of
			// the window under test.
			const archivePromise = (async () => {
				await new Promise((r) => setTimeout(r, delay));
				archiveSentAt = Date.now();
				const resp = await request.post(`/api/v1/workspaces/${ws.slug}/items/bulk`, {
					headers: auth,
					data: { op: 'archive', ids: [item.id] }
				});
				return { ok: resp.ok(), body: await resp.text() };
			})();
			await page.goto(itemUrl, { waitUntil: 'domcontentloaded' });
			const bulk = await archivePromise;
			expect(bulk.ok, bulk.body).toBeTruthy();

			// Premise: the archive really landed server-side.
			const check = await request.get(`/api/v1/workspaces/${ws.slug}/items/${item.slug}`, {
				headers: auth
			});
			expect(check.ok(), await check.text()).toBeTruthy();
			const checked = (await check.json()) as { deleted_at: string | null };
			expect(checked.deleted_at, 'archive must land server-side').not.toBeNull();

			// The open page must learn about it without a reload.
			await expect(page.locator('.archived-banner')).toBeVisible({ timeout: 10_000 });
			await Promise.all(inFlight);

			expect(
				loadedLiveBeforeArchive,
				'the page must have loaded a LIVE row before the archive for a sync to be what corrected it'
			).toBeTruthy();
			expect(
				incrementalCarriedDeletion,
				'the deletion must arrive on a /changes AFTER the page\'s cursor-seeding one — not the seed, and not a reload'
			).toBeTruthy();
		} finally {
			const del = await request.delete(`/api/v1/workspaces/${ws.slug}`, { headers: auth });
			expect(del.ok(), `scratch workspace cleanup failed: ${await del.text()}`).toBeTruthy();
		}
	});
});
