import { expect, type Request } from '@playwright/test';
import { test, quietCrossActorToasts } from './fixtures';

/**
 * BUG-2539 — a bulk archive landing in the same wall-clock second as the page's
 * sync cursor was invisible to `/changes`, leaving the item rendered LIVE
 * indefinitely (see internal/store/items.go::ItemsModifiedSince).
 *
 * The deterministic pin for the cursor arithmetic is the store-level
 * `TestItemsModifiedSince_SameSecondCursor`. This spec covers the user-visible
 * end: the archived banner appears in the page that was already open, without a
 * reload.
 *
 * The two legs are named after the MECHANISM, not a millisecond offset. An
 * earlier version fired the archive at a fixed delay after navigation start;
 * that reproduced on an idle machine and stopped reproducing under parallel
 * workers, because a slow load put the page's own item read AFTER the archive —
 * at which point the page renders an archived row from the start and the sync
 * path is never exercised. The legs now wait for the preconditions instead:
 *
 *   - the page has READ a live row (server-attested, `deleted_at: null`), and
 *   - its EventSource is subscribed (the /events response headers have
 *     arrived — the handler subscribes before writing them),
 *
 * and only then archive: immediately (same second as the page's sync cursor —
 * the reproducing case) or after crossing the next second boundary (the control
 * that behaved even before the fix).
 *
 * The oracle requires all three of:
 *   1. the page read a live row — otherwise no sync was involved;
 *   2. the deletion arrived on a `/changes` that is neither the cursor-seeding
 *      one (ordinal 0, fired by setWorkspace on mount) nor issued before the
 *      archive. Both checks are kept so a RETRIED seed cannot pass on ordinal
 *      alone. The URL match is anchored so `/items-changes` — a different,
 *      seq-based endpoint — cannot consume ordinals;
 *   3. the banner rendered.
 *
 * Ground truth for archived-ness is the server's `deleted_at`, never the UI:
 * asserting on the UI alone cannot distinguish "not archived" from "archived
 * but not shown", which is the failure mode under test.
 */

const CHANGES_RE = /\/api\/v1\/workspaces\/[^/]+\/changes(\?|$)/;
const ITEM_GET_RE = /\/api\/v1\/workspaces\/[^/]+\/items\/[^/?]+$/;

const LEGS = [
	{
		name: 'in the same second as the page cursor (the reproducing case)',
		crossSecondBoundary: false
	},
	{
		name: 'after the next second boundary (control)',
		crossSecondBoundary: true
	}
] as const;

LEGS.forEach((leg, idx) => {
	test(`BUG-2539: a bulk archive ${leg.name} reaches the open item`, async ({
		page,
		context,
		request,
		fixture
	}) => {
		await quietCrossActorToasts(context);

		const auth = { Authorization: `Bearer ${fixture.apiToken}` };
		let createdSlug: string | null = null;
		let cleanup: { ok: boolean; body: string } | null = null;

		try {
			// Own workspace: the shared suite workspace carries cross-spec
			// mutation traffic that muddies a cursor-sensitive leg.
			const slug = `b2539-${idx}-${Date.now().toString(36)}`;
			const wsResp = await request.post('/api/v1/workspaces', {
				headers: auth,
				data: { name: `BUG-2539 ${idx}`, slug, template: 'startup' }
			});
			expect(wsResp.ok(), await wsResp.text()).toBeTruthy();
			const ws = (await wsResp.json()) as { slug: string };
			createdSlug = ws.slug;

			const itemResp = await request.post(
				`/api/v1/workspaces/${ws.slug}/collections/tasks/items`,
				{ headers: auth, data: { title: `sync-window probe ${idx}` } }
			);
			expect(itemResp.ok(), await itemResp.text()).toBeTruthy();
			const item = (await itemResp.json()) as { id: string; slug: string };

			// `archiveSentAt` is stamped before the POST is issued, so "issued
			// after the archive went out" needs nothing to have resolved.
			let archiveSentAt = Number.POSITIVE_INFINITY;
			const changesOrder = new Map<Request, number>();
			const changesIssuedAt = new Map<Request, number>();
			let changesSeen = 0;

			page.on('request', (r) => {
				if (CHANGES_RE.test(r.url())) {
					changesOrder.set(r, changesSeen++);
					changesIssuedAt.set(r, Date.now());
				}
			});

			let readLiveRow = false;
			let incrementalCarriedDeletion = false;
			const inFlight: Promise<void>[] = [];
			page.on('response', (resp) => {
				inFlight.push(
					(async () => {
						try {
							if (CHANGES_RE.test(resp.url())) {
								const order = changesOrder.get(resp.request());
								const issued = changesIssuedAt.get(resp.request());
								const body = (await resp.json()) as { deleted?: string[] };
								if (
									order !== undefined &&
									order > 0 &&
									issued !== undefined &&
									issued >= archiveSentAt &&
									body.deleted?.includes(item.id)
								) {
									incrementalCarriedDeletion = true;
								}
							} else if (ITEM_GET_RE.test(resp.url()) && resp.request().method() === 'GET') {
								const body = (await resp.json()) as { id?: string; deleted_at?: string | null };
								if (body.id === item.id && !body.deleted_at) readLiveRow = true;
							}
						} catch {
							/* non-JSON body — ignore */
						}
					})()
				);
			});

			// The SSE handler subscribes BEFORE writing its response headers, so
			// this resolving means the server is ready to deliver the archive
			// event to this page.
			const sseSubscribed = page.waitForResponse((r) => r.url().includes('/api/v1/events'));
			// A GET of this item whose body carries deleted_at null — the page
			// has a live row on screen.
			const liveRowRead = page.waitForResponse(async (r) => {
				if (!ITEM_GET_RE.test(r.url()) || r.request().method() !== 'GET') return false;
				try {
					const body = (await r.json()) as { id?: string; deleted_at?: string | null };
					return body.id === item.id && !body.deleted_at;
				} catch {
					return false;
				}
			});

			const itemUrl = `/${fixture.adminUsername}/${ws.slug}/tasks/${item.id}`;
			await page.goto(itemUrl, { waitUntil: 'domcontentloaded' });
			await Promise.all([sseSubscribed, liveRowRead]);

			if (leg.crossSecondBoundary) {
				// Push the archive past the second the page's cursor sits in,
				// plus a small margin so a clock tick can't land it back inside.
				await page.waitForTimeout(1000 - (Date.now() % 1000) + 120);
			}

			archiveSentAt = Date.now();
			const bulk = await request.post(`/api/v1/workspaces/${ws.slug}/items/bulk`, {
				headers: auth,
				data: { op: 'archive', ids: [item.id] }
			});
			expect(bulk.ok(), await bulk.text()).toBeTruthy();

			// Premise: the archive really landed server-side.
			const check = await request.get(`/api/v1/workspaces/${ws.slug}/items/${item.slug}`, {
				headers: auth
			});
			expect(check.ok(), await check.text()).toBeTruthy();
			const checked = (await check.json()) as { deleted_at: string | null };
			expect(checked.deleted_at, 'archive must land server-side').not.toBeNull();

			// The open page must learn about it without a reload.
			await expect(page.locator('.archived-banner')).toBeVisible({ timeout: 10_000 });
			// Drain until no new handler was queued while awaiting.
			for (let drained = 0; drained < inFlight.length; ) {
				drained = inFlight.length;
				await Promise.all(inFlight);
			}

			// Both messages carry the recorded state. This leg has flaked rarely
			// (twice in ~70 local runs) and both times the artifacts were gone
			// before they could be read, so a recurrence has to explain itself
			// from the failure text alone.
			const diag = JSON.stringify({
				changesSeen,
				changesAfterArchive: [...changesIssuedAt.values()].filter((t) => t >= archiveSentAt).length,
				archiveSentAt,
				readLiveRow,
				incrementalCarriedDeletion,
				responsesObserved: inFlight.length
			});
			expect(
				readLiveRow,
				`the page must have read a LIVE row for a sync to be what corrected it — ${diag}`
			).toBeTruthy();
			expect(
				incrementalCarriedDeletion,
				`the deletion must arrive on a /changes issued after the archive and after the page's cursor-seeding one — not the seed, and not a reload — ${diag}`
			).toBeTruthy();
		} finally {
			if (createdSlug) {
				const del = await request.delete(`/api/v1/workspaces/${createdSlug}`, { headers: auth });
				// Recorded, not asserted here: throwing from `finally` would
				// replace the real failure with a cleanup one.
				cleanup = { ok: del.ok(), body: await del.text() };
			}
		}

		expect(cleanup?.ok, `scratch workspace cleanup failed: ${cleanup?.body}`).toBeTruthy();
	});
});
