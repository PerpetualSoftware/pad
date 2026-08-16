import { expect } from '@playwright/test';
import { test } from './fixtures';

/**
 * BUG-2601 — a collection rename only travels over the `collection_updated`
 * SSE. A client that misses that event (replay-buffer gap, disconnect) is
 * stranded on the dead slug: delta-sync reconciles ITEM changes only, and
 * `/changes` says nothing about renames — a rename-only gap even reports
 * `caught_up` — so every slug-keyed fetch 404s until a manual navigation.
 *
 * The fix reconciles the route slug against the live collections list by
 * STABLE collection id on every sync pass (resolveSyncRenameTarget +
 * reconcileRouteCollectionSlug in the collection route), mirroring the
 * BUG-2272 SSE/reorder-404 heals.
 *
 * The missed-SSE condition is made DETERMINISTIC here by aborting the
 * page's `/api/v1/events` requests — the EventSource never connects, so
 * the rename event cannot arrive by the primary path.
 *
 * Vacuity guard: before triggering the sync pass, the spec asserts the
 * route is STILL on the dead slug after a settle — proving no other
 * mechanism (an SSE leak, a router side-effect) healed it, so the heal
 * observed afterwards is attributable to the sync pass. The heal is then
 * triggered by a real tab-resume (hidden → 2s+ → visible), the exact
 * signal the sync service acts on in production.
 *
 * The rename-detection oracle is server-attested: the PATCH response's
 * regenerated slug is what the URL must converge to.
 */

test('BUG-2601: a missed collection-rename SSE is healed by the next sync pass', async ({
	page,
	request,
	fixture,
}) => {
	const auth = { Authorization: `Bearer ${fixture.apiToken}` };
	let createdSlug: string | null = null;

	try {
		// Own workspace — the shared suite workspace carries cross-spec SSE
		// and mutation traffic that could mask or confound the strand.
		// Worker-unique: desktop + mobile projects run this spec in
		// parallel, and Date.now() alone can collide across workers
		// (codex round 1 P2).
		const slug = `b2601-${test.info().workerIndex}-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 6)}`;
		createdSlug = slug;
		const wsResp = await request.post('/api/v1/workspaces', {
			headers: auth,
			data: { name: 'BUG-2601 heal', slug, template: 'startup' },
		});
		expect(wsResp.ok(), await wsResp.text()).toBeTruthy();
		const ws = (await wsResp.json()) as { slug: string };
		createdSlug = ws.slug;

		// A probe item so "the page still works on the new slug" is
		// observable content, not just a URL.
		const itemResp = await request.post(
			`/api/v1/workspaces/${ws.slug}/collections/tasks/items`,
			{ headers: auth, data: { title: 'rename-heal probe' } },
		);
		expect(itemResp.ok(), await itemResp.text()).toBeTruthy();

		// THE MISSED-SSE CONDITION: the EventSource never connects, so the
		// collection_updated rename event cannot arrive on the primary path.
		await page.route('**/api/v1/events**', (route) => route.abort());

		await page.goto(`/${fixture.adminUsername}/${ws.slug}/tasks`);
		await expect(page.getByText('rename-heal probe').first()).toBeVisible();

		// Rename server-side. A name change regenerates the slug
		// (store.UpdateCollection), which is what kills the route's slug.
		const renameResp = await request.patch(
			`/api/v1/workspaces/${ws.slug}/collections/tasks`,
			{ headers: auth, data: { name: 'Chores' } },
		);
		expect(renameResp.ok(), await renameResp.text()).toBeTruthy();
		const renamed = (await renameResp.json()) as { slug: string };
		expect(renamed.slug).not.toBe('tasks');

		// VACUITY GUARD: with SSE dead, nothing re-targets the route on its
		// own. If this fails, the "heal" below would not be the sync pass's.
		await page.waitForTimeout(700);
		expect(new URL(page.url()).pathname.endsWith('/tasks')).toBeTruthy();

		// Tab-resume: hidden long enough to clear the sync service's
		// MIN_ABSENCE_MS (2s), then visible — the production trigger.
		await page.evaluate(() => {
			Object.defineProperty(document, 'hidden', { configurable: true, get: () => true });
			document.dispatchEvent(new Event('visibilitychange'));
		});
		await page.waitForTimeout(2200);
		await page.evaluate(() => {
			Object.defineProperty(document, 'hidden', { configurable: true, get: () => false });
			document.dispatchEvent(new Event('visibilitychange'));
		});

		// THE HEAL: the sync pass reconciles the dead slug by stable id and
		// re-targets the route to the server-attested new slug.
		await page.waitForURL((u) => new URL(u).pathname.endsWith(`/${renamed.slug}`), {
			timeout: 15_000,
		});
		// And the page is functional there — the probe item renders.
		await expect(page.getByText('rename-heal probe').first()).toBeVisible();
	} finally {
		if (createdSlug) {
			await request
				.delete(`/api/v1/workspaces/${createdSlug}`, { headers: auth })
				.catch(() => {});
		}
	}
});
