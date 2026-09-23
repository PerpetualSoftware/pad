import { test, expect } from './fixtures';
import { browserLogin } from './lib/collab-helpers';
import { deleteCollection } from './lib/attachment-viewer';

/**
 * BUG-2858 — Safari's date picker would not go away. WebKit closes its macOS
 * date popover only when a FOCUSED segment of the input blurs, and presents an
 * iOS date picker only from focus; `showPicker()` alone never focuses. The
 * trigger now focuses the hidden date input before `showPicker()`.
 *
 * This is the page's half, in Chromium: the picker's input holds focus once
 * opened, and Escape there closes the picker only — the split pane behind it
 * stays open. The Safari behaviour itself is verified by hand on macOS and
 * iOS (checklist on BUG-2858); Playwright's WebKit is the Linux port and could
 * not stand in for either.
 */
test('BUG-2858: the date picker opens from a focused input, and Escape there leaves the pane open', async ({ page, fixture, request }, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'the split pane is a desktop layout');
	test.setTimeout(90_000);
	await page.setViewportSize({ width: 1400, height: 900 });
	await browserLogin(page);
	const h = { Authorization: `Bearer ${fixture.apiToken}`, 'Content-Type': 'application/json' };
	const stamp = Date.now();
	const schema = JSON.stringify({ fields: [{ key: 'due', label: 'Due', type: 'date' }] });
	const res = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/collections`, {
		headers: h, data: { name: `B2858 ${stamp}`, prefix: `BD${String(stamp).slice(-4)}`, schema },
	});
	expect(res.ok(), await res.text()).toBeTruthy();
	const coll = await res.json();
	try {
		const item = await (await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/collections/${coll.slug}/items`, {
			headers: h, data: { title: `B2858 item ${stamp}` },
		})).json();
		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/${coll.slug}?view=list&item=${item.ref}`);
		const pane = page.locator('.item-pane');
		await expect(pane).toBeVisible({ timeout: 15_000 });
		const trigger = pane.locator('button.date-trigger').first();
		await expect(trigger).toBeVisible({ timeout: 10_000 });

		await trigger.click();
		const activeIsDateInput = () =>
			page.evaluate(() => (document.activeElement as HTMLInputElement | null)?.type === 'date');
		await expect.poll(activeIsDateInput, { message: 'opening the picker focuses its input' }).toBe(true);

		// Chromium's own picker takes the FIRST Escape to close itself (the page
		// never sees it); the next one reaches the input, whose handler returns
		// focus to the trigger. Neither may close the pane.
		await page.keyboard.press('Escape');
		await page.waitForTimeout(300);
		await expect(pane, 'Escape closed the pane').toBeVisible();
		if (await activeIsDateInput()) {
			await page.keyboard.press('Escape');
			await page.waitForTimeout(300);
		}
		await expect.poll(activeIsDateInput, { message: 'Escape leaves the picker input' }).toBe(false);
		await expect(trigger).toBeFocused();
		await expect(pane, 'Escape closed the picker only, not the pane').toBeVisible();
		expect(new URL(page.url()).searchParams.get('item')).toBe(item.ref);
	} finally {
		await deleteCollection(fixture, request, coll.slug);
	}
});
