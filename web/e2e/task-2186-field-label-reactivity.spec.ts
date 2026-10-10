import { test, expect } from './fixtures';
import type { Page } from '@playwright/test';
import { browserLogin } from './lib/collab-helpers';

// TASK-2186 (the 2026-07-20 audit, cluster C1): typing the first character of
// a NEW field's label used to wedge the Create and Edit collection modals. The
// key stopped auto-syncing, "+ Add field" went dead, and nothing closed the
// dialog. Each leg types a label, then exercises all three.

async function exerciseNewField(page: Page, dialog: ReturnType<Page['getByRole']>) {
	const errors: string[] = [];
	page.on('pageerror', (e) => errors.push(e.message));

	await dialog.getByRole('button', { name: '+ Add field' }).click();
	const label = dialog.getByPlaceholder('Field name').last();
	await label.click();
	await label.pressSequentially('Stage', { delay: 30 });

	// Auto-sync: the key follows the whole label, not just its first letter.
	await expect(dialog.getByLabel('Field key').last()).toHaveValue('stage');

	// Add-field still works.
	const before = await dialog.getByPlaceholder('Field name').count();
	await dialog.getByRole('button', { name: '+ Add field' }).click();
	await expect(dialog.getByPlaceholder('Field name')).toHaveCount(before + 1);

	// And the dialog still closes. The form now holds an edit, so Escape asks
	// first (TASK-2191), through the app's confirm dialog (TASK-3543);
	// choosing Discard closes it.
	await page.keyboard.press('Escape');
	const ask = page.getByRole('dialog', { name: /^Discard / });
	await expect(ask, 'Escape on an edited form asks before discarding').toBeVisible();
	await expect(dialog, 'the form stays open while the question is asked').toBeVisible();
	await ask.getByRole('button', { name: 'Discard', exact: true }).click();
	await expect(ask).toBeHidden();
	await expect(dialog).toBeHidden();
	expect(errors, 'page errors').toEqual([]);
}

test.describe('TASK-2186: a new field label keeps the collection modals alive', () => {
	test.beforeEach(({}, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'the modals behave the same on mobile');
	});

	test('Create collection modal', async ({ page, fixture }) => {
		await browserLogin(page);
		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}`);
		await page.getByTitle('New collection').click();
		const dialog = page.getByRole('dialog', { name: 'New Collection', exact: true });
		await expect(dialog).toBeVisible();
		await dialog.getByRole('button', { name: /Blank/ }).click();
		await exerciseNewField(page, dialog);
	});

	test('Edit collection modal', async ({ page, fixture }) => {
		await browserLogin(page);
		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/tasks`);
		await page.getByRole('button', { name: 'Collection menu' }).click();
		await page.getByRole('menuitem', { name: /Edit collection/ }).click();
		const dialog = page.getByRole('dialog', { name: 'Edit Collection', exact: true });
		await expect(dialog).toBeVisible();
		await dialog.getByRole('button', { name: 'Fields', exact: true }).click();
		await exerciseNewField(page, dialog);
	});
});
