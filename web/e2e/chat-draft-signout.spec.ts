import { test, expect } from '@playwright/test';
import { suiteFixture } from './fixtures';
import { createChatUser, createConversation, loginViaForm } from './lib/chat-users';

/**
 * TASK-4315 PR 3b: an unsent Chat draft belongs to the account that typed it.
 * User A types a draft in a conversation and signs out; user B signs in IN THE
 * SAME TAB (sessionStorage survives the sign-out navigation, which is the whole
 * hazard). B must see no trace of A's draft, and A's stored records are purged.
 * Desktop only: one tab-lifetime store, and the auth endpoints are rate-limited.
 */

const DRAFT_PREFIX = 'pad:chat-draft:';
const PENDING_PREFIX = 'pad:chat-pending:';

const chatKeys = (page: import('@playwright/test').Page) =>
	page.evaluate(
		([d, p]) => Object.keys(sessionStorage).filter((k) => k.startsWith(d) || k.startsWith(p)),
		[DRAFT_PREFIX, PENDING_PREFIX]
	);

test('a draft typed by A is gone for B in the same tab, and purged from storage', async ({ browser }, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'desktop only');
	const { baseURL } = suiteFixture();
	const a = await createChatUser('draftA');
	const b = await createChatUser('draftB');
	const nA = await createConversation(a, `e2e-draft-${Date.now().toString(36)}`);
	// Numbers are per account, so both are conversation 1: the same key shape for two users.
	const nB = await createConversation(b, `e2e-draft-${Date.now().toString(36)}b`);

	const context = await browser.newContext({ baseURL });
	try {
		const page = await context.newPage();
		await page.setViewportSize({ width: 1600, height: 900 });

		await loginViaForm(page, a.email);
		await page.goto(`/messages/${nA}`);
		const box = page.getByRole('textbox', { name: 'Message' });
		await box.fill('A private unsent thought');
		// The draft is saved on a debounce; wait for it by its storage key.
		await expect.poll(async () => (await chatKeys(page)).some((k) => k.startsWith(DRAFT_PREFIX))).toBe(true);
		const aKeys = await chatKeys(page);
		expect(aKeys.length).toBeGreaterThan(0);

		// Sign out through the user menu: the SPA navigation keeps the tab (and its storage).
		await page.locator('[aria-label="User menu"]').click();
		await page.getByRole('menuitem', { name: 'Sign out' }).click();
		await page.waitForURL(/\/login(\?|$)/, { timeout: 15_000 });
		expect(await chatKeys(page), 'A\'s records are purged at sign-out').toEqual([]);

		// B signs in in the same tab and opens B's own conversation.
		await loginViaForm(page, b.email);
		await page.goto(`/messages/${nB}`);
		await expect(page.getByRole('textbox', { name: 'Message' })).toBeVisible();
		await expect(page.getByRole('textbox', { name: 'Message' })).toHaveValue('');
		const seen = await page.evaluate(
			([d, p]) =>
				Object.entries(sessionStorage)
					.filter(([k]) => k.startsWith(d) || k.startsWith(p))
					.map(([k, v]) => `${k}=${v}`)
					.join('\n'),
			[DRAFT_PREFIX, PENDING_PREFIX]
		);
		expect(seen).not.toContain('A private unsent thought');
	} finally {
		await context.close();
	}

	// A signs back in later: the draft is not there.
	const again = await browser.newContext({ baseURL });
	try {
		const page = await again.newPage();
		await loginViaForm(page, a.email);
		await page.goto(`/messages/${nA}`);
		await expect(page.getByRole('textbox', { name: 'Message' })).toHaveValue('');
	} finally {
		await again.close();
	}
});
