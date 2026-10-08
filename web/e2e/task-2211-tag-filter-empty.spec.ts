import { test, expect } from './fixtures';
import { browserLogin } from './lib/collab-helpers';
import { deleteCollection } from './lib/attachment-viewer';

/**
 * TASK-2211 (audit C6): a tag filter that matches nothing rendered the
 * brand-new-collection state ("No tasks yet…" + "+ Create Task") under a header
 * counting the collection's items, because the "No matches" branch did not
 * count the tag filter, and its Clear filters left the tags in place, so it
 * could lead into that dead end. Now: "No matches", and Clear filters clears
 * the tags too.
 */
test('a tag filter matching nothing says No matches, and Clear filters clears it', async ({ page, fixture, request }, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'filter state is viewport-agnostic');
	const headers = { Authorization: `Bearer ${fixture.apiToken}`, 'Content-Type': 'application/json' };
	const stamp = Date.now();
	const coll = await (
		await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/collections`, {
			headers,
			data: { name: `T2211 ${stamp}`, prefix: `TT${String(stamp).slice(-4)}` }
		})
	).json();
	const title = `t2211 present ${stamp}`;
	const created = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/collections/${coll.slug}/items`, {
		headers,
		data: { title, content: '', tags: '["real-tag"]' }
	});
	expect(created.ok(), await created.text()).toBeTruthy();
	try {
		await browserLogin(page);
		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/${coll.slug}?tags=zzz-no-such-tag`);
		await expect(page.getByRole('heading', { name: 'No matches' })).toBeVisible({ timeout: 15_000 });
		await expect(page.getByText(/Create your first/)).toHaveCount(0);

		await page.getByRole('button', { name: 'Clear filters' }).click();
		await expect(page.getByText(title)).toBeVisible();
		await expect.poll(() => new URL(page.url()).searchParams.get('tags')).toBeNull();
	} finally {
		await deleteCollection(fixture, request, coll.slug);
	}
});
