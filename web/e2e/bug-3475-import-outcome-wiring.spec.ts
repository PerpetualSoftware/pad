import { test, expect } from './fixtures';
import type { Page } from '@playwright/test';

/**
 * BUG-3475 — when an import's upload dies, the dialog asks the server what
 * became of it and says so. The unit and component tests cover the phases
 * against doubles; this proves the REAL network path does it: the upload's
 * request carries an import key, a failed upload leads to
 * GET /workspaces/import-status for THAT key, and the answer reaches the
 * screen.
 *
 * The upload is aborted at the network (route.abort), and import-status is
 * answered with a canned outcome: a real stall takes the server's 60s window
 * and Playwright cannot hold a body mid-upload.
 */

const IMPORT = /\/api\/v1\/workspaces\/import(\?|$)/;
const STATUS = /\/api\/v1\/workspaces\/import-status\?/;

async function openImport(page: Page, fixture: { adminUsername: string; workspaceSlug: string }) {
	await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}`);
	await expect(page.getByRole('heading', { name: /E2E Workspace/i })).toBeVisible();
	await page.getByTitle('Find or create a workspace').first().click();
	await page.getByRole('option', { name: /new workspace/i }).click();
	const dialog = page.getByRole('dialog', { name: /new workspace/i });
	await expect(dialog).toBeVisible();
	await dialog.getByRole('button', { name: /^import$/i }).click();
	await page.locator('input[type="file"]').setInputFiles({
		name: 'bundle.tar.gz',
		mimeType: 'application/gzip',
		buffer: Buffer.alloc(4096, 7)
	});
	return dialog;
}

/** Abort every upload at the network; answer import-status with `outcome`. Returns the keys seen. */
async function stage(page: Page, outcome: Record<string, string>) {
	const seen = { uploadKeys: [] as string[], statusKeys: [] as string[] };
	await page.route(IMPORT, (route) => {
		seen.uploadKeys.push(new URL(route.request().url()).searchParams.get('import_key') ?? '');
		return route.abort('failed');
	});
	await page.route(STATUS, (route) => {
		seen.statusKeys.push(new URL(route.request().url()).searchParams.get('key') ?? '');
		return route.fulfill({ json: outcome });
	});
	return seen;
}

test.describe('BUG-3475: a failed upload is resolved through import-status', () => {
	test.beforeEach(({}, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'wiring check; one project is enough');
		test.setTimeout(60_000);
	});

	test('a lost answer for a FINISHED import says so and opens it', async ({ page, fixture }) => {
		const seen = await stage(page, {
			state: 'complete',
			workspace_slug: fixture.workspaceSlug,
			workspace_name: 'E2E Workspace',
			owner_username: fixture.adminUsername
		});
		const dialog = await openImport(page, fixture);
		await dialog.getByRole('button', { name: /^import workspace$/i }).click();

		await expect(dialog.getByRole('alert')).toContainText('The import finished: "E2E Workspace" is ready');
		expect(seen.uploadKeys).toHaveLength(1);
		expect(seen.uploadKeys[0]).toMatch(/^[0-9a-f]{32}$/);
		expect(seen.statusKeys[0]).toBe(seen.uploadKeys[0]);

		await dialog.getByRole('button', { name: /^open workspace$/i }).click();
		await expect(dialog).toBeHidden();
		await expect(page).toHaveURL(new RegExp(`/${fixture.adminUsername}/${fixture.workspaceSlug}(?:[/?#]|$)`));
	});

	test('an outcome the server cannot confirm says to check the workspace list, never "nothing was kept"', async ({ page, fixture }) => {
		const seen = await stage(page, { state: 'unknown' });
		const dialog = await openImport(page, fixture);
		await dialog.getByRole('button', { name: /^import workspace$/i }).click();

		const alert = dialog.getByRole('alert');
		await expect(alert).toContainText('check your workspace list');
		await expect(alert).not.toContainText('Nothing was kept');
		await expect(dialog.getByRole('button', { name: /^try again$/i })).toBeEnabled();
		expect(seen.statusKeys[0]).toBe(seen.uploadKeys[0]);
	});
});
