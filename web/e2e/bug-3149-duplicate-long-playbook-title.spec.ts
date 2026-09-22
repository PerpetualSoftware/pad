import { test, expect } from './fixtures';
import { browserLogin } from './lib/collab-helpers';
import type { SuiteFixture } from './fixtures';

/**
 * BUG-3149 — duplicating a playbook whose title is 249+ characters always
 * failed: the page sent `${title} (copy)`, which the server refuses as over
 * its 255-character limit. The page now cuts the SOURCE so " (copy)" fits
 * (copyTitle, web/src/lib/items/titleLimit.ts). The unit tests vouch for the
 * helper; this leg vouches for the page calling it, against the real server.
 */

const DESKTOP = { width: 1200, height: 900 };
const LIMIT = 255;

function authHeaders(fixture: SuiteFixture) {
	return { Authorization: `Bearer ${fixture.apiToken}`, 'Content-Type': 'application/json' };
}

test.describe('BUG-3149: duplicating a long-titled playbook', () => {
	test.beforeEach(async ({ page }, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'one browser is enough for a client-side title rule');
		await page.setViewportSize(DESKTOP);
		await browserLogin(page);
	});

	test('a 250-character title duplicates, keeping " (copy)" within the limit', async ({ page, fixture, request }) => {
		const head = `B3149 ${Date.now()} `;
		const source = head + 'p'.repeat(250 - head.length);
		expect(source.length).toBe(250);
		const created = await request.post(
			`/api/v1/workspaces/${fixture.workspaceSlug}/collections/playbooks/items`,
			{ headers: authHeaders(fixture), data: { title: source, content: '1. step' } },
		);
		expect(created.ok(), await created.text()).toBeTruthy();

		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/playbooks`);
		const card = page.locator('.card').filter({ has: page.locator('.card-title', { hasText: source }) });
		await expect(card).toHaveCount(1);
		await card.locator('button.card-header').click();

		const write = page.waitForResponse(
			(r) => r.request().method() === 'POST' && /\/collections\/playbooks\/items$/.test(new URL(r.url()).pathname),
		);
		await card.getByRole('button', { name: 'Duplicate' }).click();
		const res = await write;

		const expected = source.slice(0, LIMIT - ' (copy)'.length) + ' (copy)';
		expect(expected.length).toBe(LIMIT);
		expect(res.status(), await res.text()).toBe(201);
		expect(((await res.json()) as { title: string }).title).toBe(expected);
		await expect(page.locator('.card-title', { hasText: expected })).toHaveCount(1);
	});
});
