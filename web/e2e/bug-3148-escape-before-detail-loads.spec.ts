import { test, expect } from './fixtures';
import { browserLogin, seedDoc } from './lib/collab-helpers';

/**
 * BUG-3148 (pane-a11y-focus :200 on #2046): ESC must close the pane from the
 * moment the pane is on screen, not only once its ItemDetail has loaded.
 *
 * ItemDetail is imported on demand (TASK-2226), so a cold open shows the
 * PaneHost shell (`.item-pane`) before ItemDetail exists. The pane-close ESC
 * layer was registered by ItemDetail, so in that window the escape stack held
 * only the list-focus layer and ESC did nothing. Measured with the spec's own
 * steps at 4 workers: 14 of 20 runs had the list layer on top and failed to
 * close. It is registered by the shell now.
 *
 * Deterministic: the ItemDetail chunk is held (found by a string only it
 * contains) until after the ESC, so the window is guaranteed rather than
 * raced. Without the fix the pane stays open.
 */
const DETAIL_CHUNK_MARKER = 'External edit applied';

test('ESC closes a pane whose ItemDetail has not loaded yet', async ({ page, fixture, request }, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'a desktop pane; one project is enough');
	let release!: () => void;
	const held = new Promise<void>((r) => (release = r));
	let heldChunks = 0;
	await page.route('**/_app/immutable/chunks/*.js', async (route) => {
		const resp = await route.fetch();
		const body = await resp.text();
		if (body.includes(DETAIL_CHUNK_MARKER)) {
			heldChunks++;
			await held;
		}
		await route.fulfill({ response: resp, body });
	});

	await page.setViewportSize({ width: 1200, height: 900 });
	await browserLogin(page);
	const { slug } = await seedDoc(fixture, request, 'B3148 early esc');
	await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/docs?item=${slug}`);

	const pane = page.locator('.item-pane');
	await expect(pane).toBeVisible();
	// The premise: the shell is up and its ItemDetail is not.
	await expect.poll(() => heldChunks, { message: 'premise: the ItemDetail chunk is being held' }).toBeGreaterThan(0);
	await expect(pane.locator('.item-content, .item-title, [data-testid="item-title"]')).toHaveCount(0);

	await page.keyboard.press('Escape');
	await expect.poll(() => new URL(page.url()).searchParams.get('item'), { timeout: 5_000 }).toBeNull();
	await expect(pane).toBeHidden();
	release();
});
