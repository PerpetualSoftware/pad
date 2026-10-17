/**
 * TASK-3866: a key made Read only in console settings is listed as Read
 * only, and the key the form reveals can read the workspace and is refused
 * a write. The Go test TestTASK3866_ReadOnlyKeyRefusesWritesOverRESTAndMCP
 * covers the same key over remote MCP; this spec proves the form sends what
 * that test mints.
 */
import { request as playwrightRequest } from '@playwright/test';
import { test, expect } from './fixtures';

test('a Read only key made in settings is listed as Read only and cannot write', async ({ page, fixture }, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'one project is enough for a form-to-server contract');

	const name = `task-3866 read-only ${Date.now()}`;
	const admin = await playwrightRequest.newContext({
		baseURL: fixture.baseURL,
		extraHTTPHeaders: { Authorization: `Bearer ${fixture.adminSessionToken}` }
	});
	try {
		await page.goto('/console/settings#api-tokens');
		const section = page.locator('#api-tokens');
		await section.getByRole('textbox', { name: 'Token name' }).fill(name);
		// Read and write is the default; Read only is a deliberate choice.
		await expect(section.getByRole('radio', { name: /Read and write/ })).toBeChecked();
		await section.getByRole('radio', { name: /Read only/ }).check();
		await section.getByRole('button', { name: 'Create', exact: true }).click();

		const secret = (await section.locator('.token-value').textContent())?.trim() ?? '';
		expect(secret).toMatch(/^pad_/);
		const row = section.locator('.token-row', { hasText: name });
		await expect(row.getByTestId('token-access')).toHaveText('Read only');
		// The form resets to the default for the next key.
		await expect(section.getByRole('radio', { name: /Read and write/ })).toBeChecked();

		const key = await playwrightRequest.newContext({
			baseURL: fixture.baseURL,
			extraHTTPHeaders: { Authorization: `Bearer ${secret}` }
		});
		try {
			const items = `/api/v1/workspaces/${fixture.workspaceSlug}/collections/tasks/items`;
			const read = await key.get(items);
			expect(read.status(), await read.text()).toBe(200);
			const write = await key.post(items, { data: { title: 'written by a read-only key' } });
			expect(write.status(), await write.text()).toBe(403);
		} finally {
			await key.dispose();
		}
	} finally {
		const list = await admin.get('/api/v1/auth/tokens');
		for (const t of (await list.json()) as { id: string; name: string }[]) {
			if (t.name === name) await admin.delete(`/api/v1/auth/tokens/${t.id}`);
		}
		await admin.dispose();
	}
});
