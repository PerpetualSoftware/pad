import { test, expect } from './fixtures';
import { browserLogin } from './lib/collab-helpers';
import { createWorkspace, deleteWorkspace } from './lib/attachment-viewer';

/**
 * TASK-2202: when the server cannot be reached, the app says so, the error is
 * announced assertively, and it stays long enough to read.
 *
 * The page loads normally; then every API request is aborted the way an
 * offline network aborts it (`internetdisconnected`), and the command palette
 * fires a search. The search's own UI may handle its failure however it likes;
 * the "can't reach the server" toast comes from the API client's seam, so it
 * appears whatever the call site does.
 */

test.describe('network unreachable toast (TASK-2202)', () => {
	test.beforeEach(({}, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'one browser is enough');
	});

	test('an unreachable server raises an assertive error toast that outlasts the old 3s', async ({ page, fixture, request }) => {
		const { slug: ws } = await createWorkspace(fixture, request, 'Toast outage');
		try {
			await browserLogin(page);
			await page.goto(`/${fixture.adminUsername}/${ws}`);
			const announcer = page.locator('[data-toast-region="assertive"]');
			await expect(announcer, 'the assertive region exists before any toast').toHaveCount(1);
			await expect(announcer).toHaveAttribute('aria-live', 'assertive');
			await expect(announcer.locator('.toast')).toHaveCount(0);

			await page.route('**/api/v1/**', (route) => route.abort('internetdisconnected'));
			await page.keyboard.press('ControlOrMeta+k');
			await page.keyboard.type('anything');

			const toast = page.locator('.toast-error', { hasText: "Can't reach the server" });
			await expect(toast).toBeVisible();
			await expect(announcer).toContainText("Can't reach the server");

			// Errors linger: still up well past the old 3s default.
			await page.waitForTimeout(4000);
			await expect(toast).toBeVisible();
		} finally {
			await page.unroute('**/api/v1/**');
			await deleteWorkspace(fixture, request, ws);
		}
	});
});
