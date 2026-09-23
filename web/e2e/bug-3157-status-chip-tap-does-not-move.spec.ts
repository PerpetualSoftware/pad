import { test, expect } from './fixtures';
import { browserLogin } from './lib/collab-helpers';
import type { APIRequestContext, Page } from '@playwright/test';
import type { SuiteFixture } from './fixtures';

/**
 * BUG-3157 — on the mobile board, a tap on a card's status chip moved the card
 * a lane. The chip was a one-tap CYCLE control: it advanced the status to the
 * next option immediately, and on a board grouped by status the next option IS
 * the next lane. Dave had not known the chip was a control at all.
 *
 * Ruling (Dave, day 77): the chip opens a status picker on every device, and
 * nothing changes until an option is chosen.
 *
 * The assertion is the OUTCOME on the server, read back after the tap, not the
 * UI: the defect is a write nobody asked for. The picker assertion that
 * follows is the precondition that makes "nothing changed" mean something — it
 * proves the tap landed on the chip rather than somewhere inert.
 */

function authHeaders(fixture: SuiteFixture) {
	return { Authorization: `Bearer ${fixture.apiToken}`, 'Content-Type': 'application/json' };
}

function itemCard(page: Page, title: string) {
	return page
		.locator('.board-view .item-card')
		.filter({ has: page.locator('.card-title', { hasText: title }) });
}

async function storedStatus(request: APIRequestContext, fixture: SuiteFixture, slug: string) {
	const res = await request.get(`/api/v1/workspaces/${fixture.workspaceSlug}/items/${slug}`, {
		headers: authHeaders(fixture),
	});
	expect(res.ok(), await res.text()).toBeTruthy();
	const item = await res.json();
	return JSON.parse(item.fields || '{}').status as string;
}

test('BUG-3157: a tap on a board card status chip opens a picker and changes nothing', async ({
	page,
	fixture,
	request,
}, testInfo) => {
	test.skip(testInfo.project.name !== 'mobile-chromium', 'the report is a touch tap on the mobile board');

	await browserLogin(page);
	const title = `B3157 chip tap ${Date.now()}`;
	const created = await request.post(
		`/api/v1/workspaces/${fixture.workspaceSlug}/collections/tasks/items`,
		{ headers: authHeaders(fixture), data: { title, fields: JSON.stringify({ status: 'open' }), content: '' } },
	);
	expect(created.ok(), await created.text()).toBeTruthy();
	const item = await created.json();

	await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/tasks`);
	const card = itemCard(page, title);
	await expect(card).toBeVisible();
	const chip = card.locator('button', { hasText: /^\s*open\s*$/i });
	await expect(chip).toBeVisible();

	await chip.tap();

	// Give a write the time to land and be read back. There is no event to wait
	// for on the fixed build, because nothing is written, so the wait is bounded
	// by time; the picker assertion below is what makes the silence meaningful.
	await page.waitForTimeout(1500);
	const afterTap = await storedStatus(request, fixture, item.slug);
	expect(afterTap, 'a single tap on the status chip changed the stored status').toBe('open');

	const picker = page.getByRole('menu', { name: /status/i }).or(page.getByRole('dialog', { name: /status/i }));
	await expect(picker, 'the tap should open a status picker').toBeVisible();

	await picker.getByRole('menuitemradio', { name: /done/i }).or(picker.getByRole('option', { name: /done/i })).first().tap();
	await expect.poll(() => storedStatus(request, fixture, item.slug)).toBe('done');
});
