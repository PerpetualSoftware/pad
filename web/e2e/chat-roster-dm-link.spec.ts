import { test, expect } from './fixtures';

/**
 * TASK-4044 (ruling 140d6e04): the /messages roster keeps a conversation as a
 * DM by its `kind`. The server writes 'direct' (migrations/158_conversations.sql
 * CHECK kind IN ('direct','group')); a roster filter on any other literal drops
 * every conversation, and unit fixtures cannot see that because they build their
 * own kind. This spec reads the server's real value: a DM created through the
 * API must show up in the roster as a link to /messages/<number>.
 */
test('the roster shows an agent with a DM as a link to its conversation', async ({
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

	const row = page
		.getByRole('list', { name: 'Agents' })
		.locator('a.row')
		.filter({ has: page.locator('.name', { hasText: agent }) });
	await expect(row).toHaveCount(1);
	await expect(row).toHaveAttribute('href', `/messages/${conversation.number}`);
});
