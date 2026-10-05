import { expect, type Page, type Route } from '@playwright/test';
import { test } from './fixtures';

/**
 * TASK-3413 (SPEC-6 U9a) — Settings → Apps.
 *
 * WHAT IS REAL AND WHAT IS NOT. The e2e server listens on plain http, and
 * apps need an https OAuth issuer, so on this server apps are UNAVAILABLE by
 * design. The first leg is therefore real end to end: the owner opens the tab
 * and the real server says apps are off. A real install would also need an
 * https app origin the server trusts, which the harness does not provide, so
 * the second leg answers the workspace's /apps routes with `page.route` and
 * walks the real UI in a real browser through review, install-code handoff,
 * disable and re-enable. The server half of those doors is covered by the Go
 * tests (TestTask3413a_*, TestAppInstall*, the U8c lifecycle tests).
 */

async function openApps(page: Page, username: string, workspace: string) {
	await page.goto(`/${username}/${workspace}/settings#apps`);
	await expect(page.getByRole('tab', { name: /Apps/ })).toHaveAttribute('aria-selected', 'true');
}

test('TASK-3413: apps off on this server says so, against the real server', async ({ page, fixture }) => {
	await openApps(page, fixture.adminUsername, fixture.workspaceSlug);
	const off = page.getByTestId('apps-unavailable');
	await expect(off).toBeVisible();
	await expect(off).toContainText('Ask your admin');
	await expect(page.getByRole('button', { name: 'Install an app' })).toHaveCount(0);
});

test('TASK-3413: install, hand off the code, disable and re-enable (app API answered by page.route)', async ({
	page,
	fixture
}) => {
	const ws = fixture.workspaceSlug;
	let state: 'active' | 'inactive' = 'active';
	let installed = false;
	const confirmed: unknown[] = [];
	const install = () => ({
		install_id: 'inst-e2e',
		app_name: 'Support Portal',
		origin: 'https://portal.example',
		version: '1.0.0',
		state,
		created_at: '2026-10-05T10:00:00Z',
		updated_at: '2026-10-05T10:00:00Z',
		webhook: { url: 'https://portal.example/hooks', status: 'active', undelivered_dropped: 2 }
	});
	const json = (route: Route, body: unknown, status = 200) =>
		route.fulfill({ status, contentType: 'application/json', body: JSON.stringify(body) });

	await page.route(`**/api/v1/workspaces/${ws}/apps**`, async (route) => {
		const req = route.request();
		const path = new URL(req.url()).pathname.replace(`/api/v1/workspaces/${ws}/apps`, '');
		if (req.method() === 'GET' && path === '') {
			return json(route, { available: true, cloud: false, installs: installed ? [install()] : [] });
		}
		if (req.method() === 'POST' && path === '/install/preview') {
			return json(route, {
				pending_id: 'pend-e2e',
				expires_at: '2026-10-05T11:00:00Z',
				origin: 'https://portal.example',
				manifest_url: 'https://portal.example/.well-known/pad-app.json',
				manifest_sha256: 'sha-e2e',
				app_id: 'portal',
				version: '1.0.0',
				title: 'Support Portal',
				publisher: 'Portal Co',
				reviewed_by_pad: false,
				notice: 'Not reviewed by Pad. You are installing code published at this origin.',
				service_access: 'write',
				delegated_access: 'read',
				reads_system_collections: "The app can read this workspace's Conventions and Playbooks.",
				collections: [{ key: 't', slug: 'tickets', name: 'Tickets', schema: {}, adopt: false }],
				events: [],
				item_actions: [],
				artifacts: [],
				redirect_uris: []
			});
		}
		if (req.method() === 'POST' && path === '/install/pending/pend-e2e/confirm') {
			confirmed.push(req.postDataJSON());
			installed = true;
			return json(
				route,
				{ install_id: 'inst-e2e', install_code: 'E2E-CODE-1', expires_at: '2026-10-05T10:10:00Z', notice: 'Give this install code to the app.', items: [] },
				201
			);
		}
		if (req.method() === 'POST' && path === '/inst-e2e/disable') {
			state = 'inactive';
			return json(route, { install_id: 'inst-e2e', state });
		}
		if (req.method() === 'POST' && path === '/inst-e2e/enable') {
			state = 'active';
			return json(route, { install_id: 'inst-e2e', state });
		}
		return json(route, { error: { code: 'not_found', message: `unrouted ${req.method()} ${path}` } }, 404);
	});

	await openApps(page, fixture.adminUsername, ws);
	await expect(page.getByText('No apps installed.')).toBeVisible();

	await page.getByRole('button', { name: 'Install an app' }).click();
	await page.getByLabel('App URL').fill('https://portal.example');
	await page.getByRole('button', { name: 'Review' }).click();
	await expect(page.getByTestId('app-not-reviewed')).toContainText('Not reviewed by Pad');
	await expect(page.getByTestId('app-reads-system')).toContainText('Conventions and Playbooks');

	await page.getByRole('button', { name: 'Install Support Portal' }).click();
	await expect(page.getByTestId('app-install-code')).toHaveText('E2E-CODE-1');
	expect(confirmed).toEqual([{ manifest_sha256: 'sha-e2e' }]);
	await page.getByRole('button', { name: 'Done' }).click();

	await page.getByRole('button', { name: /Support Portal/ }).click();
	await expect(page.getByTestId('app-undelivered-dropped')).toContainText('2 deliveries dropped');
	await page.getByRole('button', { name: 'Disable' }).click();
	await expect(page.getByRole('group', { name: 'Disable this app' })).toContainText('signed out');
	await page.getByRole('group', { name: 'Disable this app' }).getByRole('button', { name: 'Disable' }).click();
	await expect(page.getByRole('button', { name: 'Re-enable' })).toBeVisible();
	await page.getByRole('button', { name: 'Re-enable' }).click();
	await page.getByRole('group', { name: 'Re-enable this app' }).getByRole('button', { name: 'Re-enable' }).click();
	await expect(page.getByRole('button', { name: 'Disable' })).toBeVisible();
});
