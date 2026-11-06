import { test, expect } from './fixtures';

/**
 * TASK-4044 (ruling 140d6e04): the inbox keeps a conversation as a DM by its
 * `kind`. The server writes 'direct' (migrations/158_conversations.sql CHECK kind
 * IN ('direct','group')); a filter on any other literal drops every conversation,
 * and unit fixtures cannot see that because they build their own kind. This spec
 * reads the server's real value: a DM created through the API must show up as ONE
 * card linking to /messages/<number>, carrying its presence, and must not also be
 * offered in the roster of agents to start a chat with (BUG-4321).
 */
test('an agent with a DM is one card linking to its conversation, not also a roster row', async ({
	page,
	request,
	fixture
}) => {
	const agent = `rosterlink-${Date.now().toString(36)}`;
	const res = await request.post('/api/v1/me/conversations', {
		headers: { Authorization: `Bearer ${fixture.apiToken}`, 'Content-Type': 'application/json' },
		data: { agent_key: agent }
	});
	expect(res.ok(), await res.text()).toBe(true);
	const { conversation } = (await res.json()) as { conversation: { number: number } };
	expect(conversation.number).toBeGreaterThan(0);

	await page.goto('/messages');

	const card = page.locator(`a[href="/messages/${conversation.number}"]`);
	await expect(card).toHaveCount(1);
	await expect(card).toContainText(agent);
	await expect(card.locator('.presence-label')).toBeVisible();
	await expect(
		page.getByRole('list', { name: 'Agents to start a chat with' }).getByText(agent)
	).toHaveCount(0);
});

/**
 * TASK-4186 PR B: a user with no agents and no conversations sees why, and how
 * to connect one, instead of a bare "No conversations yet."
 */
test('a user with no agents sees the empty-state help', async ({ page }) => {
	// The suite shares one user whose sessions and DMs other specs create, so a truly
	// fresh account cannot be had here: the two reads are answered empty instead.
	await page.route('**/api/v1/me/conversations*', (r) =>
		r.fulfill({ json: { conversations: [], unread_total: 0 } })
	);
	await page.route('**/api/v1/sessions*', (r) => r.fulfill({ json: { sessions: [], count: 0 } }));
	await page.goto('/messages');
	await expect(page.getByRole('heading', { name: 'No one to message yet' })).toBeVisible();
	await expect(page.locator('code', { hasText: '/pad:connect' })).toBeVisible();
	await expect(page.locator('code', { hasText: 'pad watch --stream --for-session' })).toBeVisible();
});
