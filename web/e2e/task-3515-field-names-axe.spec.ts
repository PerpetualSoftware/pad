import AxeBuilder from '@axe-core/playwright';
import type { Page } from '@playwright/test';
import { test, expect } from './fixtures';

/**
 * TASK-3515: about 30 text fields were named only by their placeholder, which
 * screen readers do not reliably announce and which disappears once the field
 * has text. Each now has an accessible name; the static floor
 * (src/lib/a11y/selectsHaveNames.test.ts) keeps every file that way. This spec
 * runs axe's `label` rule on the rendered surfaces where most of them live, so
 * a name that is present in source but lost in rendering still fails.
 */

async function labelViolations(page: Page, include: string) {
	const result = await new AxeBuilder({ page }).include(include).withRules(['label']).analyze();
	return result.violations.flatMap((v) => v.nodes.map((n) => `${v.id}: ${n.target.join(' ')}`));
}

test.describe('TASK-3515: text fields are named, not just placeholdered', () => {
	test('workspace settings: the member invite and the delete confirmation', async ({ page, fixture }) => {
		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/settings#members`);
		await expect(page.getByRole('textbox', { name: 'Email address to invite' })).toBeVisible();
		expect(await labelViolations(page, '.settings')).toEqual([]);

		await page.getByRole('tab', { name: /Danger Zone/ }).click();
		await page.getByRole('button', { name: 'Delete workspace' }).click();
		await expect(page.getByRole('textbox', { name: 'Workspace slug, to confirm deletion' })).toBeVisible();
		expect(await labelViolations(page, '.settings')).toEqual([]);
	});

	test('the playbooks list, with its import control', async ({ page, fixture }) => {
		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/playbooks`);
		await expect(page.getByLabel('Import a playbook file')).toBeAttached();
		expect(await labelViolations(page, 'main')).toEqual([]);
	});

	test('the new-convention form', async ({ page, fixture }) => {
		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/conventions`);
		await page.getByRole('button', { name: '+ New Convention' }).click();
		await expect(page.getByRole('textbox', { name: 'Convention title' })).toBeVisible();
		expect(await labelViolations(page, 'main')).toEqual([]);
	});

	test('account settings', async ({ page }) => {
		await page.goto('/console/settings');
		await expect(page.getByRole('textbox', { name: 'Token name' })).toBeVisible();
		expect(await labelViolations(page, 'main')).toEqual([]);
	});
});
