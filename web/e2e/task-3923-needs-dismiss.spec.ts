import { test, expect } from './fixtures';
import { browserLogin } from './lib/collab-helpers';
import { authJson, createWorkspace, deleteWorkspace } from './lib/attachment-viewer';

/**
 * TASK-3923 (IDEA-3906 A6): the "Needs you" page end to end against the real
 * binary. An item assigned to the admin in an awaits_human collection is an
 * ASK (routing rule 1, internal/needs/needs.go Resolve), so it shows on
 * /console/needs. Dismissing it hides it for this person; the hide survives a
 * reload because it lives on the server, shows under Hidden, and Restore
 * brings it back.
 */
test.describe('needs you: dismiss and restore (TASK-3923)', () => {
	test.beforeEach(({}, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'one desktop browser is enough for the flow');
	});

	test('an ask assigned to me can be dismissed, stays gone across a reload, and is restored from Hidden', async ({
		page,
		fixture,
		request
	}) => {
		const { slug: ws } = await createWorkspace(fixture, request, 'Needs you');
		try {
			const headers = authJson(fixture);
			const me = (await (await request.get('/api/v1/auth/me', { headers })).json()) as { id: string };
			const coll = await request.post(`/api/v1/workspaces/${ws}/collections`, {
				headers,
				data: { name: 'Asks', awaits_human: true }
			});
			expect(coll.ok(), await coll.text()).toBeTruthy();
			const collSlug = ((await coll.json()) as { slug: string }).slug;
			const title = `Needs-you ask ${Date.now()}`;
			const made = await request.post(`/api/v1/workspaces/${ws}/collections/${collSlug}/items`, {
				headers,
				data: { title, assigned_user_id: me.id }
			});
			expect(made.ok(), await made.text()).toBeTruthy();

			await browserLogin(page);
			await page.goto('/console/needs');
			const rowFor = () => page.getByTestId('needs-row').filter({ hasText: title });
			await expect(rowFor()).toHaveCount(1);
			await expect(rowFor()).toContainText('An ask of you');

			await rowFor().getByRole('button', { name: 'Dismiss' }).click();
			await expect(rowFor()).toHaveCount(0);

			await page.reload();
			await expect(page.getByRole('heading', { level: 1 })).toContainText('Needs you');
			await expect(rowFor()).toHaveCount(0);

			await page.getByRole('button', { name: /^Hidden \(\d+\)$/ }).click();
			await expect(rowFor()).toHaveCount(1);
			await expect(rowFor().getByRole('button', { name: 'Dismiss' })).toHaveCount(0);

			await rowFor().getByRole('button', { name: 'Restore' }).click();
			await expect(rowFor()).toHaveCount(0);

			await page.getByRole('button', { name: 'Back to needs you' }).click();
			await expect(rowFor()).toHaveCount(1);
		} finally {
			await deleteWorkspace(fixture, request, ws);
		}
	});
});
