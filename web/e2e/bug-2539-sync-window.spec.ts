import { expect } from '@playwright/test';
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
 * Two legs. 450ms after navigation start is the offset that reproduced 7/8
 * times before the fix (the archive lands just after the SSE connects and
 * inside the cursor's own second); 1200ms is the control that always behaved.
 * Both are deterministic AFTER the fix — the comparison no longer depends on
 * where inside a second the mutation fell.
 *
 * Ground truth for archived-ness is the server's `deleted_at`, never the UI:
 * asserting on the UI alone cannot distinguish "not archived" from "archived
 * but not shown", which is the failure mode under test.
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

		const itemResp = await request.post(
			`/api/v1/workspaces/${ws.slug}/collections/tasks/items`,
			{ headers: auth, data: { title: `sync-window probe ${delay}` } }
		);
		expect(itemResp.ok(), await itemResp.text()).toBeTruthy();
		const item = (await itemResp.json()) as { id: string; slug: string };

		// Record whether the delta the page fetched actually carried the
		// deletion. The banner assertion alone would also be satisfied by an
		// unrelated mechanism (a full reload renders it too), so this is the
		// leg that ties the banner to the sync path under test.
		let deltaCarriedDeletion = false;
		page.on('response', async (resp) => {
			if (!resp.url().includes('/changes')) return;
			try {
				const body = (await resp.json()) as { deleted?: string[] };
				if (body.deleted?.includes(item.id)) deltaCarriedDeletion = true;
			} catch {
				/* non-JSON body — ignore */
			}
		});

		const itemUrl = `/${fixture.adminUsername}/${ws.slug}/tasks/${item.id}`;
		// Fire the archive on a timer started at navigation start WITHOUT
		// awaiting the page — awaiting first paint moves the archive out of the
		// window under test.
		const navStart = Date.now();
		const archivePromise = (async () => {
			await new Promise((r) => setTimeout(r, delay));
			const resp = await request.post(`/api/v1/workspaces/${ws.slug}/items/bulk`, {
				headers: auth,
				data: { op: 'archive', ids: [item.id] }
			});
			return { ok: resp.ok(), body: await resp.text(), at: Date.now() - navStart };
		})();
		await page.goto(itemUrl, { waitUntil: 'domcontentloaded' });
		const bulk = await archivePromise;
		expect(bulk.ok, bulk.body).toBeTruthy();

		// Premise: the archive really landed server-side.
		const check = await request.get(`/api/v1/workspaces/${ws.slug}/items/${item.slug}`, {
			headers: auth
		});
		const checked = (await check.json()) as { deleted_at: string | null };
		expect(checked.deleted_at, 'archive must land server-side').not.toBeNull();

		// The open page must learn about it without a reload.
		await expect(page.locator('.archived-banner')).toBeVisible({ timeout: 10_000 });
		expect(
			deltaCarriedDeletion,
			'the banner must follow a /changes delta carrying the deletion, not an unrelated reload'
		).toBeTruthy();
	});
});
