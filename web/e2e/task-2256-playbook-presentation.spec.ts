import { test, expect } from './fixtures';
import { browserLogin } from './lib/collab-helpers';
import type { SuiteFixture } from './fixtures';

/**
 * TASK-2256 (audit C64-C67), against the real server and router:
 *  - C66: the library tab survives a refresh (it is in the URL now).
 *  - C65: the workspace's own Playbooks list says which playbooks are
 *    invokable and what they take, as the library cards do.
 *  - C64: the editor's Test invocation section says a draft run is refused.
 */

function authHeaders(fixture: SuiteFixture) {
	return { Authorization: `Bearer ${fixture.apiToken}`, 'Content-Type': 'application/json' };
}

test.describe('TASK-2256: playbooks say how they run', () => {
	test.beforeEach(async ({ page }, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'page chrome; one desktop browser is enough');
		await page.setViewportSize({ width: 1280, height: 900 });
		await browserLogin(page);
	});

	test('the library tab is kept across a refresh', async ({ page, fixture }) => {
		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/library`);
		const tabs = page.getByRole('tablist', { name: 'Library' });
		await tabs.getByRole('tab', { name: 'Playbooks' }).click();
		await expect(page).toHaveURL(/[?&]tab=playbooks/);
		await page.reload();
		await expect(page.getByRole('tablist', { name: 'Library' }).getByRole('tab', { name: 'Playbooks' })).toHaveAttribute('aria-selected', 'true');
	});

	test('a playbook card shows its slug and argument count; a draft editor warns', async ({ page, fixture, request }) => {
		const stamp = Date.now();
		const slug = `t2256-${stamp}`;
		const title = `T2256 invokable ${stamp}`;
		const created = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/collections/playbooks/items`, {
			headers: authHeaders(fixture),
			data: {
				title,
				content: '1. step',
				fields: JSON.stringify({
					status: 'draft',
					invocation_slug: slug,
					arguments: [{ name: 'target', type: 'ref', required: true }]
				})
			}
		});
		expect(created.ok(), await created.text()).toBeTruthy();

		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/playbooks`);
		const card = page.locator('.card').filter({ has: page.locator('.card-title', { hasText: title }) });
		await expect(card).toHaveCount(1);
		await expect(card.getByText(`▶ ${slug}`)).toBeVisible();
		await expect(card.getByText('1 arg', { exact: true })).toBeVisible();
		await card.locator('button.card-header').click();
		await expect(card.locator('.invoke-line')).toContainText(`/pad ${slug}`);
		await expect(card.locator('.invoke-line')).toContainText("can't be run until it is active");

		await card.getByRole('button', { name: 'Edit' }).click();
		await expect(page.getByRole('note').filter({ hasText: '--allow-draft' })).toBeVisible();
	});
});
