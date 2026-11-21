import { test, expect } from './fixtures';

test('old workspace deep links resolve to the canonical slug with query and fragment', async ({ page, request, fixture }) => {
	const headers = { Authorization: `Bearer ${fixture.apiToken}` };
	const oldSlug = `rename-deep-link-${Date.now()}`;
	const newSlug = `${oldSlug}-new`;
	const created = await request.post('/api/v1/workspaces', { headers, data: { name: 'Rename deep link', slug: oldSlug } });
	expect(created.status()).toBe(201);
	const workspace = await created.json();
	const renamed = await request.patch(`/api/v1/workspaces/${oldSlug}/slug`, {
		headers, data: { expected_slug: oldSlug, slug: newSlug }
	});
	expect(renamed.status()).toBe(200);
	try {
		await page.goto(`/${fixture.adminUsername}/${oldSlug}/settings?source=old-link#members`);
		await expect(page).toHaveURL((url) => url.pathname === `/${fixture.adminUsername}/${newSlug}/settings` && url.search === '?source=old-link' && url.hash === '#members');
		await expect(page.getByRole('heading', { name: 'Workspace settings' })).toBeVisible();
		const current = await request.get(`/api/v1/workspaces/${oldSlug}`, { headers });
		expect((await current.json()).id).toBe(workspace.id);
	} finally {
		await request.delete(`/api/v1/workspaces/${newSlug}`, { headers });
	}
});
