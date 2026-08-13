import { expect, type APIRequestContext, type Page, type Request } from '@playwright/test';
import { test, quietCrossActorToasts, type SuiteFixture } from './fixtures';

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
 * The legs are named after the MECHANISM, and — this is the part that took
 * three tries to get right — each one PROVES it hit that mechanism using
 * server-attested values, then retries if it did not:
 *
 *   - the cursor second is the `server_time` of the page's FIRST `/changes`
 *     (the seed `setWorkspace` fires on mount, which is what `lastSyncTime`
 *     becomes);
 *   - the archive second is the `deleted_at` the server stored.
 *
 * The same-second leg requires those to be equal, the boundary leg requires
 * them to differ, and a misaligned attempt is retried on a fresh workspace
 * rather than asserted on. Without that check the reproducing leg can drift
 * across a second boundary on a slow machine, quietly become a second control,
 * and pass against the very query it is supposed to convict.
 *
 * Preconditions before archiving, so the leg exercises the sync path at all:
 * the page has READ a live row (a GET whose body carries no `deleted_at`) and
 * its EventSource is subscribed (the /events response headers have arrived —
 * the handler subscribes before writing them). An earlier revision fired at a
 * fixed offset from navigation start instead; under parallel workers a slow
 * load put the page's read AFTER the archive, so it rendered an already-
 * archived row and no sync was involved.
 *
 * The oracle then requires:
 *   1. the page read a live row — otherwise no sync was involved;
 *   2. the deletion arrived on a `/changes` that is neither the seed (ordinal
 *      0) nor issued before the archive. Both are checked so a RETRIED seed
 *      cannot pass on ordinal alone. The URL match is anchored so
 *      `/items-changes` — a different, seq-based endpoint — cannot consume
 *      ordinals;
 *   3. the banner rendered.
 *
 * Ground truth for archived-ness is the server's `deleted_at`, never the UI:
 * asserting on the UI alone cannot distinguish "not archived" from "archived
 * but not shown", which is the failure mode under test.
 */

const CHANGES_RE = /\/api\/v1\/workspaces\/[^/]+\/changes(\?|$)/;
const ITEM_GET_RE = /\/api\/v1\/workspaces\/[^/]+\/items\/[^/?]+$/;
const ALIGNMENT_ATTEMPTS = 6;

const LEGS = [
	{ name: 'in the same second as the page cursor (the reproducing case)', crossSecondBoundary: false },
	{ name: 'after the next second boundary (control)', crossSecondBoundary: true }
] as const;

/** RFC3339 whole-second string, the shape the server stores and returns. */
function secondOf(unixMs: number): string {
	return new Date(Math.floor(unixMs / 1000) * 1000).toISOString().replace('.000Z', 'Z');
}

type Attempt =
	| { outcome: 'misaligned'; cursorSecond: string | null; archiveSecond: string | null }
	| { outcome: 'checked' };

LEGS.forEach((leg, idx) => {
	test(`BUG-2539: a bulk archive ${leg.name} reaches the open item`, async ({
		page,
		context,
		request,
		fixture
	}) => {
		await quietCrossActorToasts(context);

		let last: Attempt = { outcome: 'misaligned', cursorSecond: null, archiveSecond: null };
		for (let attempt = 1; attempt <= ALIGNMENT_ATTEMPTS; attempt++) {
			last = await runLeg({ page, request, fixture, idx, attempt, crossSecondBoundary: leg.crossSecondBoundary });
			if (last.outcome === 'checked') return;
		}
		throw new Error(
			`could not land the archive ${leg.crossSecondBoundary ? 'past' : 'inside'} the cursor's second ` +
				`after ${ALIGNMENT_ATTEMPTS} attempts (last: cursor=${last.cursorSecond} archive=${last.archiveSecond})`
		);
	});
});

async function runLeg(opts: {
	page: Page;
	request: APIRequestContext;
	fixture: SuiteFixture;
	idx: number;
	attempt: number;
	crossSecondBoundary: boolean;
}): Promise<Attempt> {
	const { page, request, fixture, idx, attempt, crossSecondBoundary } = opts;
	const auth = { Authorization: `Bearer ${fixture.apiToken}` };
	// Assigned from the slug we ASKED for, before any parse, so a malformed
	// success response cannot leak the workspace. Overwritten with the server's
	// slug (it uniquifies on collision) as soon as that is known.
	let createdSlug: string | null = null;

	try {
		// Own workspace: the shared suite workspace carries cross-spec mutation
		// traffic that muddies a cursor-sensitive leg.
		const slug = `b2539-${idx}-${attempt}-${Date.now().toString(36)}`;
		createdSlug = slug;
		const wsResp = await request.post('/api/v1/workspaces', {
			headers: auth,
			data: { name: `BUG-2539 ${idx}`, slug, template: 'startup' }
		});
		expect(wsResp.ok(), await wsResp.text()).toBeTruthy();
		const ws = (await wsResp.json()) as { slug: string };
		createdSlug = ws.slug;

		const itemResp = await request.post(`/api/v1/workspaces/${ws.slug}/collections/tasks/items`, {
			headers: auth,
			data: { title: `sync-window probe ${idx}` }
		});
		expect(itemResp.ok(), await itemResp.text()).toBeTruthy();
		const item = (await itemResp.json()) as { id: string; slug: string };

		// `archiveSentAt` is stamped before the POST is issued, so "issued after
		// the archive went out" needs nothing to have resolved.
		let archiveSentAt = Number.POSITIVE_INFINITY;
		let seedServerTime: number | null = null;
		const changesOrder = new Map<Request, number>();
		const changesIssuedAt = new Map<Request, number>();
		let changesSeen = 0;

		const onRequest = (r: Request) => {
			if (CHANGES_RE.test(r.url())) {
				changesOrder.set(r, changesSeen++);
				changesIssuedAt.set(r, Date.now());
			}
		};
		page.on('request', onRequest);

		let readLiveRow = false;
		let incrementalCarriedDeletion = false;
		const inFlight: Promise<void>[] = [];
		const onResponse = (resp: import('@playwright/test').Response) => {
			inFlight.push(
				(async () => {
					try {
						if (CHANGES_RE.test(resp.url())) {
							const order = changesOrder.get(resp.request());
							const issued = changesIssuedAt.get(resp.request());
							const body = (await resp.json()) as { deleted?: string[]; server_time?: number };
							// The seed's server_time is what the client keeps as
							// lastSyncTime — the cursor the failing sync used.
							if (order === 0 && typeof body.server_time === 'number') {
								seedServerTime = body.server_time;
							}
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
							// A live row omits deleted_at entirely (omitempty), so
							// this is a falsy check, not a null check.
							const body = (await resp.json()) as { id?: string; deleted_at?: string | null };
							if (body.id === item.id && !body.deleted_at) readLiveRow = true;
						}
					} catch {
						/* non-JSON body — ignore */
					}
				})()
			);
		};
		page.on('response', onResponse);

		try {
			// The SSE handler subscribes BEFORE writing its response headers, so
			// this resolving means the server can deliver the archive event here.
			const sseSubscribed = page.waitForResponse((r) => r.url().includes('/api/v1/events'));
			const liveRowRead = page.waitForResponse(async (r) => {
				if (!ITEM_GET_RE.test(r.url()) || r.request().method() !== 'GET') return false;
				try {
					const body = (await r.json()) as { id?: string; deleted_at?: string | null };
					return body.id === item.id && !body.deleted_at;
				} catch {
					return false;
				}
			});

			await page.goto(`/${fixture.adminUsername}/${ws.slug}/tasks/${item.id}`, {
				waitUntil: 'domcontentloaded'
			});
			await Promise.all([sseSubscribed, liveRowRead]);

			if (crossSecondBoundary) {
				// Push past the second the cursor sits in, plus a margin so a
				// clock tick cannot land the archive back inside it.
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

			// Did this attempt actually hit the mechanism it is named after?
			// Both sides are server values: the cursor the client kept, and the
			// timestamp the server stored.
			const cursorSecond = seedServerTime === null ? null : secondOf(seedServerTime);
			const archiveSecond = checked.deleted_at;
			const sameSecond = cursorSecond !== null && cursorSecond === archiveSecond;
			if (sameSecond === crossSecondBoundary) {
				return { outcome: 'misaligned', cursorSecond, archiveSecond };
			}

			// The open page must learn about it without a reload.
			await expect(page.locator('.archived-banner')).toBeVisible({ timeout: 10_000 });
			// Drain until no new handler was queued while awaiting.
			for (let drained = 0; drained < inFlight.length; ) {
				drained = inFlight.length;
				await Promise.all(inFlight);
			}

			// Messages carry the recorded state: this leg flaked twice in ~70
			// runs of an earlier revision and the artifacts were cleared by the
			// next run before they could be read, so a recurrence has to explain
			// itself from the failure text alone.
			const diag = JSON.stringify({
				attempt,
				cursorSecond,
				archiveSecond,
				changesSeen,
				changesAfterArchive: [...changesIssuedAt.values()].filter((t) => t >= archiveSentAt).length,
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
			return { outcome: 'checked' };
		} finally {
			page.off('request', onRequest);
			page.off('response', onResponse);
		}
	} finally {
		if (createdSlug) {
			// Swallowed on purpose: throwing from here would replace the real
			// failure with a cleanup one.
			await request
				.delete(`/api/v1/workspaces/${createdSlug}`, { headers: auth })
				.catch(() => undefined);
		}
	}
}
