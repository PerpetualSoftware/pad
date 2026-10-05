import { expect, request as pwRequest, test, type APIRequestContext } from '@playwright/test';
import { accessSync, constants, statSync } from 'node:fs';
import type https from 'node:https';
import {
	appCall,
	cleanupDirs,
	closeServer,
	freePort,
	makeCerts,
	opensslAvailable,
	signatureVerifies,
	startApp,
	startFront,
	startPad,
	stopPad,
	waitFor,
	type AppServer,
	type Certs
} from './lib/real-install';

/**
 * TASK-3415 — an installed app, for real, end to end.
 *
 * task-3413-apps-settings.spec.ts walks the Apps UI with the workspace's
 * /apps routes answered by page.route, because the shared e2e server is plain
 * http and apps need an https issuer and an https app origin. This leg builds
 * that world (see lib/real-install.ts) and drives every step against real
 * servers: install from the UI, the app's redeem and token, its writes, a
 * signed webhook, and disable.
 *
 * The app reaches pad ONLY through the https front, and every URL pad hands
 * it is asserted to be https (lead note on TASK-3415).
 *
 * It needs the `-tags e2etest` pad build (PAD_E2E_BINARY) and openssl. A
 * machine without them skips it; CI fails instead, so a broken setup cannot
 * pass as a skip.
 */

const BINARY = process.env.PAD_E2E_BINARY ?? '';
function missing(): string {
	if (!BINARY) return 'PAD_E2E_BINARY (a `go build -tags e2etest` pad) is not set';
	try {
		accessSync(BINARY, constants.X_OK);
		if (!statSync(BINARY).isFile()) throw new Error('not a file');
	} catch {
		return `PAD_E2E_BINARY=${BINARY} is not an executable file`;
	}
	return opensslAvailable() ? '' : 'openssl is not on PATH';
}
const MISSING = missing();

const ADMIN = { email: 'real-install-admin@example.com', name: 'Rina Admin', password: 'correct-horse-battery-staple-3415' };
const COMPANION = 'e2e-tickets';
const TITLE = 'E2E Real App';

// One 1x1 PNG.
const PNG = Buffer.from(
	'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg==',
	'base64'
);

function manifest(origin: string) {
	return {
		id: 'acme/e2e-real',
		version: '1.0.0',
		min_contract: { apps: 1, events: 1 },
		title: TITLE,
		publisher: 'Acme',
		base_url: origin,
		redirect_uris: [`${origin}/oauth/callback`],
		scopes: { service: { access: 'write' }, delegated: { access: 'read' } },
		events: [
			{ name: 'item.created', collections: ['tickets'] },
			{ name: 'comment.created', collections: ['tickets'] }
		],
		webhook_url: `${origin}/hooks`,
		companion_pack: {
			collections: [
				{
					key: 'tickets',
					slug: COMPANION,
					name: 'E2E tickets',
					schema: { fields: [{ key: 'status', label: 'Status', type: 'select', options: ['open', 'solved'] }] }
				}
			]
		}
	};
}

// The leg waits on a real manifest fetch, three webhook deliveries and a
// no-delivery window; the suite's 30 s default would cut it short.
test.describe.configure({ mode: 'serial', timeout: 150_000 });

test.describe('TASK-3415: a real install, end to end', () => {
	let certs: Certs;
	let front: https.Server | undefined;
	let app: AppServer | undefined;
	let pad: ReturnType<typeof startPad> | undefined;
	let browserContext: import('@playwright/test').BrowserContext | undefined;
	let frontOrigin = '';
	let owner: APIRequestContext;
	let csrf: Record<string, string> = {};
	let wsID = '';
	let wsSlug = '';
	let username = '';
	let pat = '';

	test.beforeAll(async ({}, info) => {
		if (info.project.name !== 'desktop-chromium') return;
		if (MISSING) {
			if (process.env.CI) throw new Error(`TASK-3415 real-install leg cannot run in CI: ${MISSING}`);
			return;
		}
		certs = makeCerts();
		// The front and the app bind port 0 and report theirs. pad takes its
		// port as a setting, so it is chosen first and can be taken in between
		// by another process: an early exit retries on a new port.
		app = await startApp(certs, manifest);
		owner = await pwRequest.newContext({ ignoreHTTPSErrors: true });
		for (let attempt = 1; ; attempt++) {
			const padPort = await freePort();
			const f = await startFront(certs, padPort);
			frontOrigin = `https://127.0.0.1:${f.port}`;
			pad = startPad({ binary: BINARY, padPort, frontOrigin, caPath: certs.caPath });
			try {
				await waitFor('pad through the front', async () => {
					if (pad!.failure()) throw new Error(pad!.failure());
					return (await owner.get(`${frontOrigin}/api/v1/health`).catch(() => null))?.ok();
				}, 30_000);
				front = f.server;
				break;
			} catch (err) {
				await stopPad(pad.child);
				await closeServer(f.server);
				cleanupDirs(pad.dataDir);
				if (attempt === 3) throw err;
			}
		}
		await owner.dispose();
		owner = await pwRequest.newContext({ baseURL: frontOrigin, ignoreHTTPSErrors: true });

		const boot = await owner.post('/api/v1/auth/bootstrap', { data: ADMIN });
		expect(boot.ok(), await boot.text()).toBeTruthy();
		const login = await owner.post('/api/v1/auth/login', { data: { email: ADMIN.email, password: ADMIN.password } });
		expect(login.ok(), await login.text()).toBeTruthy();
		const cookie = (await owner.storageState()).cookies.find((c) => c.name.endsWith('pad_csrf'));
		expect(cookie, 'a CSRF cookie at login').toBeTruthy();
		csrf = { 'X-CSRF-Token': cookie!.value };

		const ws = await owner.post('/api/v1/workspaces', { headers: csrf, data: { name: 'Real Install', template: 'startup' } });
		expect(ws.ok(), await ws.text()).toBeTruthy();
		({ id: wsID, slug: wsSlug } = await ws.json());
		username = (await (await owner.get('/api/v1/auth/me')).json()).username;
		const tok = await owner.post('/api/v1/auth/tokens', { headers: csrf, data: { name: 'real-install', workspace_id: wsID, expires_in: 1 } });
		expect(tok.ok(), await tok.text()).toBeTruthy();
		pat = (await tok.json()).token;

		// The instance admin turns apps on and lets pad reach the test app's
		// private origin, to fetch its manifest and to deliver its webhooks.
		const apps = await owner.put('/api/v1/admin/apps', {
			headers: csrf,
			data: { enabled: true, private_origins: [{ origin: app!.origin, allowed: ['127.0.0.1'], fetch: true, webhook: true }] }
		});
		expect(apps.ok(), await apps.text()).toBeTruthy();
		expect((await apps.json()).available, 'apps available behind an https issuer').toBe(true);
		// MCP on too, so pad publishes its authorization-server metadata: the
		// URLs in it are ones pad hands any client, apps included.
		const mcp = await owner.put('/api/v1/admin/mcp', { headers: csrf, data: { enabled: true } });
		expect(mcp.ok(), await mcp.text()).toBeTruthy();
	});

	test.afterAll(async () => {
		await browserContext?.close().catch(() => {});
		await owner?.dispose();
		await stopPad(pad?.child);
		await closeServer(front);
		await closeServer(app?.server);
		cleanupDirs(...([certs?.dir, pad?.dataDir].filter(Boolean) as string[]));
	});

	test('install, redeem, write, webhook, disable', async ({ browser }, info) => {
		test.skip(info.project.name !== 'desktop-chromium', 'one world per run: the desktop project drives it');
		test.skip(!!MISSING, MISSING);
		const api = `${frontOrigin}/api/app/v1`;
		const theApp = app!;
		const httpsOnly = (what: string, url: unknown) =>
			expect(String(url), `${what} is a URL pad hands the app`).toMatch(new RegExp(`^${frontOrigin.replace(/\./g, '\\.')}(/|$)`));

		// 0. What pad advertises is https, through the front, and the app
		// takes its token endpoint from there rather than knowing it.
		const meta = await appCall(certs.ca, 'GET', `${frontOrigin}/.well-known/oauth-authorization-server`);
		expect(meta.status, meta.text).toBe(200);
		const discovered = meta.json();
		for (const key of Object.keys(discovered)) {
			if (typeof discovered[key] === 'string' && /^https?:/.test(discovered[key])) httpsOnly(`metadata ${key}`, discovered[key]);
		}
		httpsOnly('issuer', discovered.issuer);
		httpsOnly('token_endpoint', discovered.token_endpoint);

		// 1. The owner installs it in the real UI.
		const context = (browserContext = await browser.newContext({ baseURL: frontOrigin, ignoreHTTPSErrors: true }));
		await context.setExtraHTTPHeaders({ Authorization: `Bearer ${pat}` });
		const page = await context.newPage();
		await page.goto(`/${username}/${wsSlug}/settings#apps`);
		await expect(page.getByRole('tab', { name: /Apps/ })).toHaveAttribute('aria-selected', 'true');
		await page.getByRole('button', { name: 'Install an app' }).click();
		await page.getByLabel('App URL').fill(theApp.origin);
		await page.getByRole('button', { name: 'Review' }).click();
		await expect(page.getByTestId('app-not-reviewed')).toBeVisible({ timeout: 30_000 });
		await page.getByRole('button', { name: `Install ${TITLE}` }).click();
		const code = (await page.getByTestId('app-install-code').textContent())?.trim() ?? '';
		expect(code).toMatch(/^padic_/);
		await page.getByRole('button', { name: 'Done' }).click();

		// 2. The app redeems the code: credentials, the hook's secret, and its
		// workspace (BUG-3416).
		const red = await appCall(certs.ca, 'POST', `${api}/install/redeem`, {
			headers: { 'Content-Type': 'application/json' },
			body: JSON.stringify({ code })
		});
		expect(red.status, red.text).toBe(200);
		const creds = red.json();
		expect(creds.workspace_id).toBe(wsID);
		expect(creds.workspace_slug).toBe(wsSlug);
		expect(creds.webhook_secret, 'the hook secret').toBeTruthy();

		// 3. A service token from the https issuer, for the https audience.
		const mint = () =>
			appCall(certs.ca, 'POST', discovered.token_endpoint, {
				headers: {
					'Content-Type': 'application/x-www-form-urlencoded',
					Authorization: 'Basic ' + Buffer.from(`${encodeURIComponent(creds.client_id)}:${encodeURIComponent(creds.client_secret)}`).toString('base64')
				},
				body: new URLSearchParams({ grant_type: 'client_credentials', resource: `${api}` }).toString()
			});
		const minted = await mint();
		expect(minted.status, minted.text).toBe(200);
		const bearer = { Authorization: `Bearer ${minted.json().access_token}` };

		// 4. The token's binding names the workspace too (BUG-3416).
		const me = await appCall(certs.ca, 'GET', `${api}/me`, { headers: bearer });
		expect(me.status, me.text).toBe(200);
		expect(me.json()).toMatchObject({ install_id: creds.install_id, workspace_id: wsID, auth_kind: 'service', is_app: true });

		// 5. The app writes: an item, a comment, an upload.
		const scoped = `${api}/workspaces/${wsID}`;
		const created = await appCall(certs.ca, 'POST', `${scoped}/collections/${COMPANION}/items`, {
			headers: { ...bearer, 'Content-Type': 'application/json' },
			body: JSON.stringify({ title: 'Opened by the app', content: 'From the portal.', fields: { status: 'open' } })
		});
		expect(created.status, created.text).toBe(201);
		const item = created.json();
		expect(item.via_app).toBe(creds.install_id);
		const comment = await appCall(certs.ca, 'POST', `${scoped}/items/${item.id}/comments`, {
			headers: { ...bearer, 'Content-Type': 'application/json' },
			body: JSON.stringify({ body: 'Looking into it.' })
		});
		expect(comment.status, comment.text).toBe(201);
		expect(comment.json().author_kind).toBe('app');
		const upload = await appCall(certs.ca, 'POST', `${scoped}/items/${item.id}/attachments?filename=shot.png`, {
			headers: { ...bearer, 'Content-Type': 'image/png' },
			body: PNG
		});
		expect(upload.status, upload.text).toBe(201);
		expect(upload.json()).toMatchObject({ item_id: item.id, mime_type: 'image/png', size: PNG.length });

		// 6. A person's write in the companion reaches the app as a signed
		// webhook naming the workspace; the app's own writes do too.
		const human = await owner.post(`/api/v1/workspaces/${wsSlug}/collections/${COMPANION}/items`, {
			headers: csrf,
			data: { title: 'Filed by a person', fields: '{"status":"open"}' }
		});
		expect(human.ok(), await human.text()).toBeTruthy();
		const byTitle = (title: string) => theApp.hooks.find((h) => h.body.event === 'item.created' && h.body.title === title);
		const personHook = await waitFor('the person-written item.created delivery', () => byTitle('Filed by a person'));
		expect(signatureVerifies(creds.webhook_secret, personHook), 'signature v2 verifies with the redeemed secret').toBe(true);
		expect(personHook.headers['x-pad-install']).toBe(creds.install_id);
		expect(personHook.body.workspace_id).toBe(wsID);
		expect(personHook.body.via_app, 'a person wrote it').toBeUndefined();
		const appHook = await waitFor('the app-written item.created delivery', () => byTitle('Opened by the app'));
		expect(appHook.body.via_app).toBe(creds.install_id);
		await waitFor('the comment.created delivery', () => theApp.hooks.find((h) => h.body.event === 'comment.created'));

		// 7. The owner disables it in the UI: the token dies, no new one is
		// issued, and deliveries stop.
		await page.getByRole('button', { name: new RegExp(TITLE) }).click();
		await page.getByRole('button', { name: 'Disable' }).click();
		await page.getByRole('group', { name: 'Disable this app' }).getByRole('button', { name: 'Disable' }).click();
		await expect(page.getByRole('button', { name: 'Re-enable' })).toBeVisible();

		const after = await appCall(certs.ca, 'GET', `${api}/me`, { headers: bearer });
		expect(after.status, after.text).toBe(401);
		const challenge = String(after.headers['www-authenticate'] ?? '');
		expect(challenge).toContain('invalid_token');
		// Any URL the challenge names is one pad hands the app.
		for (const url of challenge.match(/https?:\/\/[^"\s,]+/g) ?? []) httpsOnly('a WWW-Authenticate URL', url);
		const remint = await mint();
		expect(remint.status, 'a disabled install mints no token').not.toBe(200);

		const before = theApp.hooks.length;
		const late = await owner.post(`/api/v1/workspaces/${wsSlug}/collections/${COMPANION}/items`, {
			headers: csrf,
			data: { title: 'Filed after disable', fields: '{"status":"open"}' }
		});
		expect(late.ok(), await late.text()).toBeTruthy();
		// Twenty drain ticks: a delivery would have landed long before.
		await new Promise((r) => setTimeout(r, 4_000));
		expect(theApp.hooks.slice(before).map((h) => h.body.title), 'no delivery to a disabled install').toEqual([]);

	});
});
