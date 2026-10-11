import { test, expect } from './fixtures';
import { browserLogin } from './lib/collab-helpers';
import { authJson } from './lib/attachment-viewer';

/**
 * TASK-3589: a saved view can be selected on a phone. The mobile "Choose view"
 * sheet listed only List, Board and Table; saved views and "All" lived only in
 * the desktop menu, so a saved workflow vanished at phone width. Selecting is
 * the scope here; managing views stays on the desktop.
 */
test('the mobile view sheet lists saved views; picking one applies its filters, "All" clears them', async ({ page, fixture, request }, testInfo) => {
	test.skip(testInfo.project.name !== 'mobile-chromium', 'the mobile sheet');
	// A word the list search matches (a bare number is not tokenized as one).
	const stamp = `zqv${Date.now().toString(36)}`;
	const ws = fixture.workspaceSlug;
	const mk = async (title: string, priority: string) => {
		const r = await request.post(`/api/v1/workspaces/${ws}/collections/tasks/items`, {
			headers: authJson(fixture),
			data: { title, fields: JSON.stringify({ status: 'open', priority }) },
		});
		expect(r.ok(), await r.text()).toBeTruthy();
	};
	await mk(`B3589 high ${stamp}`, 'high');
	await mk(`B3589 low ${stamp}`, 'low');
	const viewName = `Hot ${stamp}`;
	const v = await request.post(`/api/v1/workspaces/${ws}/collections/tasks/views`, {
		headers: authJson(fixture),
		data: { name: viewName, view_type: 'list', config: JSON.stringify({ filters: [{ field: 'priority', op: 'eq', value: 'high' }] }) },
	});
	expect(v.ok(), await v.text()).toBeTruthy();

	await browserLogin(page);
	await page.goto(`/${fixture.adminUsername}/${ws}/tasks?view=list&q=${stamp}`);
	await expect(page.getByText(`B3589 low ${stamp}`)).toBeVisible({ timeout: 15_000 });

	await page.locator('button.view-chip').click();
	const sheet = page.getByRole('dialog', { name: 'Choose view' });
	await expect(sheet.getByText('Saved views')).toBeVisible();
	await sheet.getByRole('button', { name: viewName }).click();
	await expect(sheet).toBeHidden();
	await expect(page.getByText(`B3589 low ${stamp}`)).toHaveCount(0);
	await expect(page.getByText(`B3589 high ${stamp}`)).toBeVisible();
	await expect(page).toHaveURL(/priority=high/);

	// "All" clears the view's filters again.
	await page.locator('button.view-chip').click();
	await page.getByRole('dialog', { name: 'Choose view' }).getByRole('button', { name: 'All', exact: true }).click();
	await expect(page.getByText(`B3589 low ${stamp}`)).toBeVisible();
});
