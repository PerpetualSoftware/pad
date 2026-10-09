import { test, expect } from './fixtures';
import { browserLogin } from './lib/collab-helpers';

/**
 * TASK-2257 (audit C72): the conventions page says what its markers mean. The
 * enforcement value is visible text beside its dot, every marker names itself
 * to a screen reader, and a legend explains the terms.
 */
test.describe('TASK-2257: agent terms are explained', () => {
	test.beforeEach(async ({ page }, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'one desktop browser is enough');
		await page.setViewportSize({ width: 1280, height: 800 });
		await browserLogin(page);
	});

	test('conventions: enforcement is text, markers are named, the legend explains them', async ({ page, fixture }) => {
		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/conventions`);
		const row = page.locator('.enforcement').first();
		await expect(row).toBeVisible();
		await expect(row.locator('.enforcement-label')).toHaveText(/^Enforcement: (must|should|nice to have)$/);
		await expect(row).toHaveAttribute('title', /^Enforcement: .*How binding it is/);
		await expect(page.locator('.scope-badge').first()).toContainText('Surface:');

		const legend = page.locator('details.agent-terms');
		await expect(legend.locator('dd')).toHaveCount(3);
		await expect(legend.locator('dd').first()).toBeHidden();
		await legend.locator('summary').click();
		await expect(legend.locator('dt')).toHaveText(['Trigger', 'Surface', 'Enforcement']);
		await expect(legend.locator('dd').nth(2)).toContainText('"must" is required');
	});

	test('library: the chips say what they are', async ({ page, fixture }) => {
		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/library`);
		const trigger = page.locator('.badges [title^="Trigger:"]').first();
		await expect(trigger).toBeVisible();
		await expect(trigger).toContainText('Trigger:');
		await expect(page.locator('details.agent-terms')).toBeVisible();
	});
});
