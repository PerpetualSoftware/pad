import { test, expect } from './fixtures';
import { browserLogin, seedDoc, expectEditorMounted } from './lib/collab-helpers';

/**
 * TASK-2199: offline edits live only in this tab, so closing it loses them.
 * The offline badge used to say they were "saved locally", and closing the tab
 * raised no prompt in the collaborative editor (only raw mode had one).
 *
 * Driven for real: the item syncs, the browser goes offline, the user types,
 * then the tab closes with beforeunload honoured. The browser's own "leave
 * site?" prompt must fire. The CONTROL closes an offline tab with nothing
 * typed and must see no prompt, so the first leg cannot pass on a prompt that
 * fires for every close.
 */

test.describe('TASK-2199: closing a tab with offline edits asks first', () => {
	test.beforeEach(({}, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'the unload prompt is viewport-agnostic');
	});

	async function openOffline(page: import('@playwright/test').Page, context: import('@playwright/test').BrowserContext, fixture: import('./fixtures').SuiteFixture, request: import('@playwright/test').APIRequestContext) {
		const { slug } = await seedDoc(fixture, request, 'TASK-2199 offline');
		await browserLogin(page);
		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/docs/${slug}`);
		const editor = await expectEditorMounted(page);
		await expect(page.locator('.collab-state-synced')).toBeVisible();
		await context.setOffline(true);
		await expect(page.locator('.collab-state-offline')).toBeVisible({ timeout: 15_000 });
		// The tooltip says what really happens.
		await expect(page.locator('.collab-state-offline')).toHaveAttribute(
			'title',
			/kept in this tab and will sync when the connection returns\. Closing the tab before then loses them\./
		);
		return editor;
	}

	test('typing offline then closing raises the leave prompt', async ({ page, context, fixture, request }) => {
		test.setTimeout(60_000);
		const editor = await openOffline(page, context, fixture, request);
		await editor.click();
		await page.keyboard.type(`offline edit ${Date.now()}`);

		const dialogs: string[] = [];
		page.on('dialog', async (d) => {
			dialogs.push(d.type());
			await d.dismiss(); // "Stay"
		});
		await page.close({ runBeforeUnload: true });
		await expect.poll(() => dialogs).toEqual(['beforeunload']);
		await context.setOffline(false);
	});

	test('CONTROL: closing an offline tab with nothing typed raises no prompt', async ({ page, context, fixture, request }) => {
		test.setTimeout(60_000);
		await openOffline(page, context, fixture, request);
		const dialogs: string[] = [];
		page.on('dialog', async (d) => {
			dialogs.push(d.type());
			await d.dismiss();
		});
		await page.close({ runBeforeUnload: true });
		// Give a prompt the chance to appear before declaring there was none.
		await new Promise((r) => setTimeout(r, 1000));
		expect(dialogs).toEqual([]);
		await context.setOffline(false);
	});
});
