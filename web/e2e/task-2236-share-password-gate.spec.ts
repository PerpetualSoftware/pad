import { test, expect } from './fixtures';

/**
 * TASK-2236 (audit C52): the password gate a STRANGER meets on a share link had
 * no label, no autofocus and no autocomplete, and a wrong password changed
 * nothing a screen reader would hear. Probed then as {hasLabel: false,
 * ariaLabel: null, autocomplete: null, focused: false}.
 */
test('the share-link password gate is labelled, focused, autocompletes, and announces a wrong password', async ({ browser, fixture, request }, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'the gate is viewport-agnostic');
	const headers = { Authorization: `Bearer ${fixture.apiToken}`, 'Content-Type': 'application/json' };
	const title = `t2236 shared ${Date.now()}`;
	const item = await (
		await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/collections/tasks/items`, {
			headers,
			data: { title, fields: '{}', content: 'secret body' }
		})
	).json();
	const link = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/items/${item.slug}/share-links`, {
		headers,
		data: { password: 'open-sesame-2236' }
	});
	expect(link.ok(), await link.text()).toBeTruthy();
	const { token } = (await link.json()) as { token: string };

	// A stranger: a fresh context with no session.
	const ctx = await browser.newContext();
	const page = await ctx.newPage();
	try {
		await page.goto(`/s/${token}`);
		const input = page.getByLabel('Password', { exact: true });
		await expect(input).toBeVisible({ timeout: 15_000 });
		await expect(input).toBeFocused();
		await expect(input).toHaveAttribute('autocomplete', 'current-password');

		await input.fill('wrong-password');
		await page.getByRole('button', { name: 'View content' }).click();
		await expect(page.getByRole('alert')).toContainText(/incorrect|password/i);
		await expect(input).toHaveAttribute('aria-invalid', 'true');

		await input.fill('open-sesame-2236');
		await page.getByRole('button', { name: 'View content' }).click();
		await expect(page.getByText(title)).toBeVisible({ timeout: 15_000 });
	} finally {
		await ctx.close();
	}
});
