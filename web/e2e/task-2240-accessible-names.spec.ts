import { test, expect } from './fixtures';
import { browserLogin } from './lib/collab-helpers';
import type { SuiteFixture } from './fixtures';

/**
 * TASK-2240 (audit C111): icon-only buttons are named for assistive tech.
 * A glyph-named button (B, •, ✕) is found below only by its new name, so the
 * role-and-name query is the assertion. A TITLE-only button already has an
 * accessible name, because accname falls back to title, so for those the
 * aria-label itself is asserted: a title is not reliably announced, and is
 * not shown on touch.
 */

function authJson(fixture: SuiteFixture) {
	return { Authorization: `Bearer ${fixture.apiToken}`, 'Content-Type': 'application/json' };
}

async function createDoc(fixture: SuiteFixture, request: import('@playwright/test').APIRequestContext, title: string, content: string) {
	const resp = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/collections/docs/items`, {
		headers: authJson(fixture),
		data: { title, content }
	});
	expect(resp.ok(), await resp.text()).toBeTruthy();
	return (await resp.json()) as { slug: string; item_number: number };
}

test.describe('TASK-2240: icon-only buttons have names', () => {
	test('the mobile editor toolbar names every button and says which formats are on', async ({ page, fixture, request }, testInfo) => {
		test.skip(testInfo.project.name !== 'mobile-chromium', 'the formatting toolbar is the phone-width one');
		await browserLogin(page);
		const doc = await createDoc(fixture, request, `T2240 toolbar ${Date.now()}`, 'Plain paragraph.');
		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/docs/${doc.slug}`);
		const prose = page.locator('.ProseMirror').first();
		await expect(prose).toBeVisible();
		await prose.click();
		// The toolbar shows only while the on-screen keyboard is up, which the
		// editor reads from visualViewport. Emulate a 300px keyboard.
		await page.evaluate(() => {
			const vv = window.visualViewport!;
			Object.defineProperty(vv, 'height', { configurable: true, get: () => window.innerHeight - 300 });
			vv.dispatchEvent(new Event('resize'));
		});
		const toolbar = page.getByRole('toolbar', { name: 'Formatting' });
		await expect(toolbar).toBeVisible();
		for (const name of [
			'Insert block', 'Bold', 'Italic', 'Strikethrough', 'Heading 2', 'Heading 3',
			'Bulleted list', 'Numbered list', 'Task list', 'Outdent', 'Indent', 'Code block', 'Quote', 'Attach file'
		]) {
			await expect(toolbar.getByRole('button', { name, exact: true }), name).toHaveCount(1);
		}
		for (const name of ['Insert block', 'Outdent', 'Indent', 'Attach file']) {
			await expect(toolbar.getByRole('button', { name, exact: true }), name).toHaveAttribute('aria-label', name);
		}
		const bold = toolbar.getByRole('button', { name: 'Bold', exact: true });
		await expect(bold).toHaveAttribute('aria-pressed', 'false');
		await bold.click();
		await expect(bold).toHaveAttribute('aria-pressed', 'true');
	});

	test('dialog closes and the link popover are named', async ({ page, fixture, request }, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'one desktop browser is enough');
		await page.setViewportSize({ width: 1280, height: 900 });
		await browserLogin(page);

		// Roles page: the new-item dialog and the role dialog.
		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/roles`);
		await page.locator('.new-item-btn').click();
		const newItem = page.getByRole('dialog', { name: /New/ });
		await newItem.getByRole('button', { name: 'Close', exact: true }).click();
		await expect(newItem).toHaveCount(0);
		await page.locator('.add-role-btn').or(page.getByRole('button', { name: 'Create your first role' })).first().click();
		const roleDialog = page.getByRole('dialog', { name: 'New Role' });
		await roleDialog.getByRole('button', { name: 'Close', exact: true }).click();
		await expect(roleDialog).toHaveCount(0);

		// The link popover: caret inside a link.
		const doc = await createDoc(fixture, request, `T2240 link ${Date.now()}`, 'See [the docs](https://example.com/docs) here.');
		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/docs/${doc.slug}`);
		const link = page.locator('.ProseMirror a', { hasText: 'the docs' }).first();
		await expect(link).toBeVisible();
		const box = (await link.boundingBox())!;
		await page.mouse.click(box.x + box.width / 2, box.y + box.height / 2);
		for (const name of ['Open link', 'Edit link', 'Remove link']) {
			const btn = page.getByRole('button', { name, exact: true });
			await expect(btn, name).toBeVisible();
			await expect(btn, name).toHaveAttribute('aria-label', name);
		}

		// The create-workspace modal's close.
		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}`);
		await page.getByTitle('Find or create a workspace').first().click();
		await page.getByRole('option', { name: /new workspace/i }).click();
		const create = page.getByRole('dialog', { name: /new workspace/i });
		await expect(create).toBeVisible();
		await create.getByRole('button', { name: 'Close', exact: true }).click();
		await expect(create).toHaveCount(0);
	});
});
