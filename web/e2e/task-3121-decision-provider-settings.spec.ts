import { expect } from '@playwright/test';
import { test } from './fixtures';

/**
 * TASK-3121 — the Decision provider section on console/admin/settings.
 *
 * What only a browser can show: the key the admin typed is sent once and
 * never comes back. The input is emptied after save and stays empty after a
 * reload, and neither the PUT response nor the page-load GET carries any of
 * its bytes. The server tests pin the API; this pins that the PAGE does not
 * hold the key or re-send it.
 *
 * DECISIONS ARE NEVER ENABLED HERE. The e2e server is shared by every spec,
 * and an enabled provider would owe a job for every item other specs write,
 * then call typesafe with a fake key from the tick. Dashboards would go
 * degraded under unrelated specs. The leg saves a key and a model with the
 * toggle OFF, and the finally block removes both.
 */

const KEY = `tsk_e2e_${Math.random().toString(36).slice(2)}${Date.now().toString(36)}`;
const MODEL = 'jev-e2e-pin';

test('TASK-3121: a saved decision key is write-only in the page', async ({ page, fixture, request }, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'one writer to the shared instance setting');

	const auth = { Authorization: `Bearer ${fixture.apiToken}` };
	// Precondition: this leg assumes the environment sets none of the fields
	// (otherwise the inputs are disabled and the save is refused), and that
	// nothing enabled decisions on the shared server.
	const before = await request.get(`${fixture.baseURL}/api/v1/admin/decision-provider`, { headers: auth });
	expect(before.ok()).toBe(true);
	const initial = await before.json();
	expect(initial.env).toEqual({ provider: false, api_key: false, model: false });
	expect(initial.effective.enabled).toBe(false);

	try {
		await page.goto('/console/admin/settings');
		const section = page.getByTestId('decision-provider-section');
		await expect(section).toBeVisible();
		await expect(section.getByTestId('decision-privacy-line')).toHaveText(
			'When enabled, item titles, bodies and comments are sent to the provider.'
		);
		await expect(section.getByTestId('decision-enabled')).not.toBeChecked();
		await expect(section.locator('#decision-provider')).toHaveValue('typesafe');

		const keyInput = section.locator('#decision-api-key');
		await keyInput.fill(KEY);
		await section.locator('#decision-model').fill(MODEL);

		const putResp = page.waitForResponse(
			(r) => r.url().endsWith('/api/v1/admin/decision-provider') && r.request().method() === 'PUT'
		);
		await section.getByTestId('decision-save').click();
		const put = await putResp;
		expect(put.status()).toBe(200);
		// The request carried the key (so the save is real)...
		expect(put.request().postDataJSON()).toMatchObject({
			api_key: KEY,
			model: MODEL,
			enabled: false,
			provider: 'typesafe'
		});
		// ...and the response carries none of it.
		const putBody = await put.text();
		expect(putBody).not.toContain(KEY.slice(-6));
		expect(JSON.parse(putBody).stored.api_key_set).toBe(true);

		await expect(section.getByTestId('decision-status')).toHaveText('Saved');
		await expect(keyInput).toHaveValue('');
		await expect(keyInput).toHaveAttribute('placeholder', 'A key is saved — type to replace it');

		// Reload: the model persisted, the key did not come back into the page.
		const getResp = page.waitForResponse(
			(r) => r.url().endsWith('/api/v1/admin/decision-provider') && r.request().method() === 'GET'
		);
		await page.reload();
		const get = await getResp;
		expect(await get.text()).not.toContain(KEY.slice(-6));
		await expect(section.locator('#decision-model')).toHaveValue(MODEL);
		await expect(keyInput).toHaveValue('');
		await expect(keyInput).toHaveAttribute('placeholder', 'A key is saved — type to replace it');
		await expect(section.getByTestId('decision-effective')).toHaveText('Off.');

		// A save with the key input untouched must not send api_key: the page
		// holds no key to send, and a sent "" would be a 400.
		const put2Resp = page.waitForResponse(
			(r) => r.url().endsWith('/api/v1/admin/decision-provider') && r.request().method() === 'PUT'
		);
		await section.getByTestId('decision-save').click();
		const put2 = await put2Resp;
		expect(put2.status()).toBe(200);
		expect(put2.request().postDataJSON()).not.toHaveProperty('api_key');
		expect((await put2.json()).stored.api_key_set).toBe(true);
	} finally {
		const cleanup = await request.put(`${fixture.baseURL}/api/v1/admin/decision-provider`, {
			headers: auth,
			data: { clear_api_key: true, model: '' }
		});
		expect(cleanup.ok()).toBe(true);
	}
});
