import { test, expect } from './fixtures';
import { createChatUser, loginViaForm } from './lib/chat-users';

/**
 * TASK-4315 PR 4: a person with no conversations and no agents sees the empty
 * state in the desktop Chat dock, not a blank panel. (The surface was built in
 * 3b; this pins it in a real browser.)
 */
test('the desktop dock shows the empty state for a user with no conversations', async ({ browser, fixture }, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'desktop only');
	const user = await createChatUser('empty');
	const ctx = await browser.newContext({ baseURL: fixture.baseURL });
	try {
		const p = await ctx.newPage();
		await p.setViewportSize({ width: 1600, height: 900 });
		await loginViaForm(p, user.email);
		await p.goto('/');
		await p.locator('.console-nav .nav-links a[href="/messages"]').click();
		const region = p.getByRole('complementary', { name: 'Chat' });
		await expect(region).toBeVisible();
		await expect(region.getByRole('heading', { name: /Start a conversation|No one to message yet/ })).toBeVisible();
	} finally {
		await ctx.close();
	}
});
