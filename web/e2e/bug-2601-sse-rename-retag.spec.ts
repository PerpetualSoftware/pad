import { expect } from '@playwright/test';
import { test } from './fixtures';

/**
 * BUG-2601 (retag half) — the LIVE SSE rename path. BUG-2272 re-targets
 * the route, but cached localIndex rows keep the OLD `collection_slug`
 * (rows are only re-stamped when the item itself changes, and a rename
 * touches no items), so the healed route rendered an EMPTY board while
 * the sidebar still counted the items. Discovered by this spec's
 * pre-fix run during BUG-2601; fixed by `localIndex.retagCollection`
 * called from the workspace layout's global `collection_updated`
 * subscriber. The sibling spec (bug-2601-sync-rename-heal) covers the
 * missed-SSE path, which retags at the sync-heal site instead.
 */
test('BUG-2601: live SSE rename re-targets the route AND the board still renders its items', async ({ page, request, fixture }) => {
	const auth = { Authorization: `Bearer ${fixture.apiToken}` };
	const slug = `b2601p-${Date.now().toString(36)}`;
	const wsResp = await request.post('/api/v1/workspaces', {
		headers: auth,
		data: { name: 'BUG-2601 probe', slug, template: 'startup' },
	});
	expect(wsResp.ok(), await wsResp.text()).toBeTruthy();
	const ws = (await wsResp.json()) as { slug: string };
	try {
		const itemResp = await request.post(`/api/v1/workspaces/${ws.slug}/collections/tasks/items`, {
			headers: auth,
			data: { title: 'probe item' },
		});
		expect(itemResp.ok()).toBeTruthy();

		await page.goto(`/${fixture.adminUsername}/${ws.slug}/tasks`);
		await expect(page.getByText('probe item').first()).toBeVisible();

		const renameResp = await request.patch(`/api/v1/workspaces/${ws.slug}/collections/tasks`, {
			headers: auth,
			data: { name: 'Chores' },
		});
		expect(renameResp.ok()).toBeTruthy();
		const renamed = (await renameResp.json()) as { slug: string };

		await page.waitForURL((u) => new URL(u).pathname.endsWith(`/${renamed.slug}`), {
			timeout: 10_000,
		});
		await expect(page.getByText('probe item').first()).toBeVisible({ timeout: 5000 });
	} finally {
		await request.delete(`/api/v1/workspaces/${ws.slug}`, { headers: auth }).catch(() => {});
	}
});
