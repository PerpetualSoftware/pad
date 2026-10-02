/**
 * PLAN-2310 DR-8 acceptance: on a self-host served over http with MCP turned
 * on, the connect modal offers the personal API token path (and no claim
 * tab, which needs OAuth), and the URL and client config it shows actually
 * work. The spec copies both out of the modal, puts the suite's token where
 * the placeholder is, and calls /mcp with exactly that: initialize, then
 * tools/list, which must answer with the catalog.
 *
 * The e2e server is http and sets PAD_URL to its own origin
 * (playwright.config.ts), so turning the setting on is all it takes. MCP is a
 * server-wide setting, so the test turns it back off when it is done; no
 * other spec reads the connect modal or /mcp. The toggle lives INSIDE the
 * test, not in beforeAll/afterAll: those run once per worker, so a worker
 * whose test is skipped (the mobile project) would turn MCP off under the
 * desktop test mid-flight.
 */
import { request as playwrightRequest, type APIRequestContext, type Page } from '@playwright/test';
import { test, expect, type SuiteFixture } from './fixtures';

async function setMCP(admin: APIRequestContext, enabled: boolean) {
	const res = await admin.put('/api/v1/admin/mcp', { data: { enabled } });
	expect(res.status(), await res.text()).toBe(200);
	const body = (await res.json()) as { enabled: boolean; readiness: { state: string; auth_methods: string[] } };
	expect(body.enabled).toBe(enabled);
	if (enabled) {
		// The precondition the modal assertions rest on: MCP is on, over
		// http, so tokens are the only method.
		expect(body.readiness.state).toBe('on');
		expect(body.readiness.auth_methods).toEqual(['pat']);
	}
}

// A Streamable HTTP response is JSON or a one-event SSE stream.
async function rpcResult(res: import('@playwright/test').APIResponse): Promise<{ result?: unknown; error?: unknown }> {
	const text = await res.text();
	expect(res.status(), text).toBe(200);
	if ((res.headers()['content-type'] ?? '').includes('text/event-stream')) {
		const data = text
			.split('\n')
			.filter((l) => l.startsWith('data:'))
			.map((l) => l.slice(5).trim())
			.join('');
		return JSON.parse(data);
	}
	return JSON.parse(text);
}

test('http self-host: the modal token config reaches /mcp and lists tools', async ({ page, fixture }, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'one project is enough for a server contract');

	const admin = await playwrightRequest.newContext({
		baseURL: fixture.baseURL,
		extraHTTPHeaders: { Authorization: `Bearer ${fixture.adminSessionToken}` }
	});
	try {
		// Inside the try: a failed readiness assertion after the PUT
		// landed must still turn MCP back off.
		await setMCP(admin, true);
		await connectAndListTools(page, fixture);
	} finally {
		await setMCP(admin, false);
		await admin.dispose();
	}
});

async function connectAndListTools(page: Page, fixture: SuiteFixture) {
	await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}`);
	await page.getByRole('button', { name: 'User menu' }).click();
	await page.getByRole('menuitem', { name: 'Connect a project…' }).click();

	const dialog = page.getByRole('dialog');
	await expect(dialog.getByRole('heading', { level: 2 })).toContainText('Connect');
	// The token path, not the OAuth one: no claim tab, no sign-in copy.
	await expect(dialog.getByRole('tab', { name: /Connect an agent/ })).toHaveAttribute('aria-selected', 'true');
	await expect(dialog.getByRole('tab', { name: /Connect code/ })).toHaveCount(0);
	await expect(dialog).not.toContainText('Sign in with your Pad');
	await expect(dialog.getByTestId('connect-pat-settings-link')).toHaveAttribute(
		'href',
		'/console/settings#api-tokens'
	);

	const expectedURL = `${fixture.baseURL}/mcp`;
	await expect(dialog.locator('pre').first()).toHaveText(expectedURL);

	await dialog.getByRole('button', { name: 'Cursor', exact: true }).click();
	const configText = (await dialog.getByTestId('connect-pat-config').textContent()) ?? '';
	const config = JSON.parse(configText.replace('YOUR_PAD_TOKEN', fixture.apiToken)) as {
		mcpServers: { pad: { url: string; headers: Record<string, string> } };
	};
	const { url, headers } = config.mcpServers.pad;
	expect(url).toBe(expectedURL);

	// Only what the modal handed over: its URL and its headers.
	const client = await playwrightRequest.newContext();
	try {
		const base = { ...headers, 'Content-Type': 'application/json', Accept: 'application/json, text/event-stream' };
		const init = await client.post(url, {
			headers: base,
			data: {
				jsonrpc: '2.0',
				id: 1,
				method: 'initialize',
				params: { protocolVersion: '2025-03-26', capabilities: {}, clientInfo: { name: 'e2e', version: '0' } }
			}
		});
		const initBody = await rpcResult(init);
		expect(initBody.error, JSON.stringify(initBody)).toBeUndefined();
		const session = init.headers()['mcp-session-id'];
		const withSession = session ? { ...base, 'Mcp-Session-Id': session } : base;

		await client.post(url, {
			headers: withSession,
			data: { jsonrpc: '2.0', method: 'notifications/initialized' }
		});

		const list = await client.post(url, {
			headers: withSession,
			data: { jsonrpc: '2.0', id: 2, method: 'tools/list' }
		});
		const listBody = (await rpcResult(list)) as { result?: { tools?: { name: string }[] } };
		const names = (listBody.result?.tools ?? []).map((t) => t.name);
		expect(names).toContain('pad_item');
		expect(names).toContain('pad_set_workspace');
	} finally {
		await client.dispose();
	}
}
