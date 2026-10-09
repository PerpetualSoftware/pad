import { test, expect } from './fixtures';
import { browserLogin } from './lib/collab-helpers';
import { authJson } from './lib/attachment-viewer';
import type { APIRequestContext } from '@playwright/test';
import type { SuiteFixture } from './fixtures';

/**
 * TASK-2217 (audit C36): "TASK-8 blocks this" no longer means opening TASK-8
 * and linking back. Add Relationship offers the inverse directions, says which
 * way the link points, and an inverse choice stores the same link type with
 * the PICKED item as its source.
 */

async function createTask(fixture: SuiteFixture, request: APIRequestContext, title: string) {
	const resp = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/collections/tasks/items`, {
		headers: authJson(fixture),
		data: { title, fields: JSON.stringify({ status: 'open' }) }
	});
	expect(resp.ok(), await resp.text()).toBeTruthy();
	return (await resp.json()) as { id: string; slug: string };
}

test.describe('TASK-2217: Add Relationship in both directions', () => {
	test.beforeEach(async ({ page }, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'item page UI; one desktop browser is enough');
		await page.setViewportSize({ width: 1280, height: 900 });
		await browserLogin(page);
	});

	test('"Blocked by" stores a blocks link FROM the picked item', async ({ page, fixture, request }) => {
		const stamp = Date.now();
		const here = await createTask(fixture, request, `Blocked one ${stamp}`);
		const blocker = await createTask(fixture, request, `Blocker ${stamp}`);

		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/tasks/${here.slug}`);
		await page.getByRole('tab', { name: /Relationships/ }).first().click();
		await page.locator('.add-relationship-btn').click();
		await page.getByLabel('Link type').selectOption('blocked_by');
		await expect(page.locator('#add-link-reads')).toHaveText(/^the item you pick blocks TASK-\d+$/);

		await page.getByLabel('Search items to link').fill(`Blocker ${stamp}`);
		await page.getByRole('option', { name: new RegExp(`Blocker ${stamp}`) }).first().click();
		await expect(page.getByText('Relationship added')).toBeVisible();

		const links = await request.get(`/api/v1/workspaces/${fixture.workspaceSlug}/items/${here.slug}/links`, {
			headers: authJson(fixture)
		});
		const all = (await links.json()) as Array<{ source_id: string; target_id: string; link_type: string }>;
		const link = all.find((l) => l.link_type === 'blocks');
		expect(link, JSON.stringify(all)).toBeTruthy();
		expect(link!.source_id).toBe(blocker.id);
		expect(link!.target_id).toBe(here.id);
	});
});
