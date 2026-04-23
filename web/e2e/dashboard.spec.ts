import { expect, test } from './fixtures';
import { WORKSPACE_NAME } from './global-setup';

/**
 * Dashboard smoke test.
 *
 * Confirms that:
 *   - A logged-in user lands on a workspace route without redirecting
 *     to login (session from the auth fixture is honored).
 *   - The seeded workspace's name shows up on the page.
 *
 * If this test goes red, something fundamental in the auth → route →
 * render path is broken; any deeper flow tests would be meaningless
 * until it's green again.
 *
 * Runs in both project viewports; the dashboard should render in both.
 */

test('authed user sees the seeded workspace dashboard', async ({ page, fixture }) => {
	await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}`);

	// We shouldn't have been bounced to an inline "Sign in" form.
	await expect(page.getByRole('button', { name: /^sign in$/i })).toHaveCount(0);

	// Workspace name should be visible somewhere — top bar, title, or
	// dashboard header all render it.
	await expect(page.getByText(WORKSPACE_NAME, { exact: false }).first()).toBeVisible();
});
