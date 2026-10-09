import { test, expect } from './fixtures';
import type { Page } from '@playwright/test';
import { browserLogin, seedDoc } from './lib/collab-helpers';
import type { SuiteFixture } from './fixtures';

/**
 * TASK-2235 U2 (audit C7): the command palette and quick-add are real dialogs.
 *
 *  - Quick-add is the Modal primitive (native showModal): Tab cannot leave it,
 *    Escape closes it, and an Escape meant for its collection picker closes
 *    only the picker.
 *  - The palette keeps its own element (lead ruling): on desktop a
 *    `role=dialog` `aria-modal` surface that keeps Tab inside; on mobile in a
 *    workspace a sheet docked above the live bottom nav, which (like
 *    DockedSheet) does not claim aria-modal and cycles Tab through the
 *    palette and the nav, never the page.
 *  - The page's own Escape guards still behave with the palette open: one
 *    Escape closes the palette and NOT the item pane under it (the route
 *    guards stand down for a foreign `[role=dialog]`, which the palette now
 *    is), and the next Escape closes the pane.
 */

function docsUrl(fixture: SuiteFixture, query = ''): string {
	return `/${fixture.adminUsername}/${fixture.workspaceSlug}/docs${query}`;
}

const PALETTE = '.palette';

async function openPalette(page: Page) {
	const input = page.getByPlaceholder('Search items, collections, docs...');
	await expect(async () => {
		if (!(await input.isVisible())) await page.keyboard.press('Control+k');
		await expect(input).toBeVisible({ timeout: 1_000 });
	}).toPass({ timeout: 20_000 });
	await expect(input).toBeFocused();
	return input;
}

/** Where focus is, as the selector of its nearest named surface. */
async function focusOwner(page: Page): Promise<string> {
	return page.evaluate(() => {
		const a = document.activeElement;
		if (!a || a === document.body) return 'body';
		if (a.closest('.palette')) return 'palette';
		if (a.closest('nav.bottom-nav')) return 'nav';
		if (a.closest('dialog.quick-add-dialog')) return 'quick-add';
		return `page:${a.tagName.toLowerCase()}.${(a as HTMLElement).className}`;
	});
}

test.describe('TASK-2235 U2: palette and quick-add are dialogs (desktop)', () => {
	test.beforeEach(async ({ page }, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'desktop keyboard paths; one browser is enough');
		await page.setViewportSize({ width: 1280, height: 800 });
		await browserLogin(page);
	});

	test('Escape with the palette open over an item pane closes the palette only; the next closes the pane', async ({
		page,
		fixture,
		request
	}) => {
		await seedDoc(fixture, request, 'Palette over pane');
		await page.goto(docsUrl(fixture));
		const card = page
			.locator('.item-card')
			.filter({ has: page.locator('.card-title', { hasText: 'Palette over pane' }) });
		await card.first().click();
		await expect(page.locator('.item-pane')).toBeVisible();
		const paneRef = new URL(page.url()).searchParams.get('item');
		expect(paneRef).not.toBeNull();

		await openPalette(page);
		await page.keyboard.press('Escape');
		await expect(page.locator(PALETTE)).toHaveCount(0);
		// THE ASSERTION: the pane under the palette is still open.
		await page.waitForTimeout(300);
		await expect(page.locator('.item-pane')).toBeVisible();
		expect(new URL(page.url()).searchParams.get('item')).toBe(paneRef);

		// The same from a NON-text control in the palette. From the input the
		// route guard already stood down (text-entry targets own Escape), so
		// this is the case the palette's role="dialog" decides: before it, the
		// route saw no foreign dialog and closed the pane on the same press.
		// With an empty query the input is the palette's only tabbable control,
		// so put focus on the palette surface itself, where a click on its body
		// lands it (tabindex=-1).
		await openPalette(page);
		await page.locator(PALETTE).focus();
		expect(await focusOwner(page)).toBe('palette');
		expect(await page.evaluate(() => document.activeElement?.tagName)).not.toMatch(/^(INPUT|TEXTAREA)$/);
		await page.keyboard.press('Escape');
		await expect(page.locator(PALETTE)).toHaveCount(0);
		await page.waitForTimeout(300);
		await expect(page.locator('.item-pane')).toBeVisible();
		expect(new URL(page.url()).searchParams.get('item')).toBe(paneRef);

		// The page's guard is back in charge once the palette is gone.
		await page.keyboard.press('Escape');
		await expect.poll(() => new URL(page.url()).searchParams.get('item')).toBeNull();
	});

	test('the palette is a modal dialog: Tab stays inside, and focus returns on close', async ({ page, fixture }) => {
		await page.goto(docsUrl(fixture));
		// The sidebar's Search button: open from it with the keyboard, so it is
		// the element focus must come back to.
		const trigger = page.locator('.sidebar button.search-btn');
		await expect(trigger).toBeVisible();
		await trigger.focus();
		await page.keyboard.press('Enter');
		const input = page.getByPlaceholder('Search items, collections, docs...');
		await expect(input).toBeFocused();

		const palette = page.locator(PALETTE);
		await expect(palette).toHaveAttribute('role', 'dialog');
		await expect(palette).toHaveAttribute('aria-modal', 'true');

		for (let i = 0; i < 15; i++) {
			await page.keyboard.press(i % 4 === 3 ? 'Shift+Tab' : 'Tab');
			expect(await focusOwner(page), `after Tab ${i + 1}`).toBe('palette');
		}

		await page.keyboard.press('Escape');
		await expect(palette).toHaveCount(0);
		await expect(trigger).toBeFocused();
	});

	test('quick-add is a native modal: Tab stays inside, the picker takes its own Escape', async ({ page, fixture }) => {
		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/tasks`);
		const dialog = page.locator('dialog.quick-add-dialog');
		await expect(async () => {
			if (!(await dialog.isVisible())) await page.keyboard.press('c');
			await expect(dialog).toBeVisible({ timeout: 1_000 });
		}).toPass({ timeout: 20_000 });
		expect(await dialog.evaluate((d) => d.matches(':modal'))).toBe(true);
		await expect(page.getByRole('textbox', { name: 'New item title' })).toBeFocused();

		// A native modal dialog lets Tab step out of the DOCUMENT to the browser
		// chrome (activeElement reads <body>) and back; what it never reaches is
		// the inert page behind it, which is the claim.
		const owners = new Set<string>();
		for (let i = 0; i < 8; i++) {
			await page.keyboard.press(i % 3 === 2 ? 'Shift+Tab' : 'Tab');
			const owner = await focusOwner(page);
			expect(owner, `after Tab ${i + 1}`).toMatch(/^(quick-add|body)$/);
			owners.add(owner);
		}
		expect(owners.has('quick-add')).toBe(true);

		// An Escape aimed at the open picker closes the picker, not the dialog.
		const pill = page.getByRole('button', { name: 'Choose collection' });
		await pill.click();
		const picker = page.locator('.quick-add-picker');
		await expect(picker).toBeVisible();
		await page.keyboard.press('Escape');
		await expect(picker).toHaveCount(0);
		await expect(dialog).toBeVisible();

		// The picker's options are not clipped by the dialog box.
		await pill.click();
		await expect(picker).toBeVisible();
		// The last option paints where it is laid out: hit-testing its corner
		// finds it, which a clip by the dialog's box would prevent.
		const lastOptionHit = await picker.evaluate((p) => {
			const last = p.lastElementChild as HTMLElement;
			const r = last.getBoundingClientRect();
			const hit = document.elementFromPoint(r.left + 4, r.bottom - 4);
			return !!hit && last.contains(hit);
		});
		expect(lastOptionHit).toBe(true);
		await page.keyboard.press('Escape');
		await expect(picker).toHaveCount(0);

		await page.keyboard.press('Escape');
		await expect(dialog).toBeHidden();
	});
});

test.describe('TASK-2235 U2: the docked mobile palette (mobile)', () => {
	test.beforeEach(async ({ page }, testInfo) => {
		test.skip(testInfo.project.name !== 'mobile-chromium', 'the docked sheet exists only on mobile');
		await browserLogin(page);
	});

	test('docked above the live nav: no aria-modal, Tab cycles palette and nav, Escape from the nav closes it', async ({
		page,
		fixture
	}) => {
		await page.goto(docsUrl(fixture));
		const searchSlot = page.locator('nav.bottom-nav').getByRole('button', { name: 'Search' });
		await expect(searchSlot).toBeVisible();
		await searchSlot.click();
		const palette = page.locator(PALETTE);
		await expect(palette).toBeVisible();
		await expect(palette).toHaveAttribute('role', 'dialog');
		await expect(palette).not.toHaveAttribute('aria-modal', /.*/);

		const seen = new Set<string>();
		for (let i = 0; i < 25; i++) {
			await page.keyboard.press('Tab');
			const owner = await focusOwner(page);
			expect(owner, `after Tab ${i + 1}`).toMatch(/^(palette|nav)$/);
			seen.add(owner);
		}
		expect([...seen].sort()).toEqual(['nav', 'palette']);

		// Park focus on a nav slot, then Escape from there.
		for (let i = 0; i < 25 && (await focusOwner(page)) !== 'nav'; i++) await page.keyboard.press('Tab');
		expect(await focusOwner(page)).toBe('nav');
		await page.keyboard.press('Escape');
		await expect(palette).toHaveCount(0);
	});
});
