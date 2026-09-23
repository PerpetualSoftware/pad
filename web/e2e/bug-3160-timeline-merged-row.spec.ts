import { test, expect } from './fixtures';
import { browserLogin } from './lib/collab-helpers';
import { deleteCollection } from './lib/attachment-viewer';
import type { APIRequestContext } from '@playwright/test';
import type { SuiteFixture } from './fixtures';

/**
 * BUG-3160 — an open item timeline never showed an activity row the debounce
 * merge had changed. `mergeIntoUnlinkedActivity` folds a second update by the
 * same actor (within ActivityDebounceCooldown, 5 minutes) into the existing
 * "updated" row and restamps it; the timeline refreshed only on comment and
 * reaction events, never item_updated, so the merged change stayed invisible
 * until a reload.
 *
 * The fix admits item_updated through a THROTTLED head re-read (at most one per
 * 10s per mounted timeline, lead-ruled), so the assertion below allows 15s.
 */

function authHeaders(fixture: SuiteFixture) {
	return { Authorization: `Bearer ${fixture.apiToken}`, 'Content-Type': 'application/json' };
}

async function patchNote(request: APIRequestContext, fixture: SuiteFixture, slug: string, note: string) {
	const res = await request.patch(`/api/v1/workspaces/${fixture.workspaceSlug}/items/${slug}`, {
		headers: authHeaders(fixture),
		data: { fields_patch: { note } },
	});
	expect(res.ok(), await res.text()).toBeTruthy();
}

test('BUG-3160: an open timeline shows a change the debounce merge folded into an existing row', async ({
	page,
	fixture,
	request,
}, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'one browser is enough for a refresh behaviour');
	test.setTimeout(60_000);
	await page.setViewportSize({ width: 1400, height: 900 });
	await browserLogin(page);

	const stamp = Date.now();
	const schema = JSON.stringify({ fields: [{ key: 'note', label: 'Note', type: 'text' }] });
	const collRes = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/collections`, {
		headers: authHeaders(fixture),
		data: { name: `B3160 ${stamp}`, prefix: `BF${String(stamp).slice(-4)}`, schema },
	});
	expect(collRes.ok(), await collRes.text()).toBeTruthy();
	const coll = await collRes.json();
	try {
		const itemRes = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/collections/${coll.slug}/items`, {
			headers: authHeaders(fixture),
			data: { title: `B3160 item ${stamp}`, fields: JSON.stringify({ note: 'start' }) },
		});
		expect(itemRes.ok(), await itemRes.text()).toBeTruthy();
		const item = await itemRes.json();

		const first = `first-${stamp}`;
		const second = `second-${stamp}`;
		await patchNote(request, fixture, item.slug, first); // opens the "updated" row

		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/${coll.slug}/${item.slug}`);
		await page.getByRole('tab', { name: 'Activity' }).or(page.getByRole('button', { name: 'Activity' })).first().click();
		// PRECONDITION: the timeline is loaded and shows the row as it stood.
		await expect(page.getByText(first).first()).toBeVisible();

		// The second update, same actor and source, inside the 5-minute window:
		// the server MERGES it into the row above rather than adding a row.
		await patchNote(request, fixture, item.slug, second);

		// PRECONDITION on the server: the merge really happened, so the thing
		// asserted below is the client's refresh and not a missing write.
		const act = await request.get(`/api/v1/workspaces/${fixture.workspaceSlug}/items/${item.slug}/activity`, {
			headers: authHeaders(fixture),
		});
		expect(act.ok()).toBeTruthy();
		const rows = JSON.stringify(await act.json());
		expect(rows, 'the server did not record the second change').toContain(second);

		await expect(
			page.getByText(second).first(),
			'the open timeline never showed the merged change (it needs a reload)',
		).toBeVisible({ timeout: 15_000 });
	} finally {
		await deleteCollection(fixture, request, coll.slug);
	}
});
