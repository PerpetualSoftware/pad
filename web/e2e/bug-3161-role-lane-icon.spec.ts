import { test, expect } from './fixtures';
import { browserLogin } from './lib/collab-helpers';

/**
 * BUG-3161 — a role created without an icon showed the literal text
 * '&#129302;' in its roles-board lane header: the fallback was an HTML entity
 * inside a Svelte text interpolation, which escapes it. It must show the robot
 * character.
 */
test('BUG-3161: a role with no icon shows the robot, not a literal entity', async ({ page, fixture, request }, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'one browser is enough for a text render');
	await browserLogin(page);
	const headers = { Authorization: `Bearer ${fixture.apiToken}`, 'Content-Type': 'application/json' };
	const name = `B3161 ${Date.now()}`;
	const res = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/agent-roles`, { headers, data: { name } });
	expect(res.ok(), await res.text()).toBeTruthy();
	const role = await res.json();
	expect(role.icon ?? '', 'precondition: the role has no icon').toBe('');
	try {
		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/roles`);
		const lane = page.locator('.lane').filter({ has: page.locator('.lane-name', { hasText: name }) });
		const icon = lane.locator('.lane-icon');
		await expect(icon).toBeVisible();
		await expect(icon).not.toContainText('&#');
		await expect(icon).toHaveText(String.fromCodePoint(0x1f916));
	} finally {
		await request.delete(`/api/v1/workspaces/${fixture.workspaceSlug}/agent-roles/${role.id}`, { headers });
	}
});
