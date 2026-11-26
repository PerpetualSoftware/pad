import { test as base, type BrowserContext } from '@playwright/test';
import { readFileSync } from 'node:fs';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { ADMIN_PASSWORD } from './global-setup';

const HERE = dirname(fileURLToPath(import.meta.url));

/**
 * Shared test fixtures.
 *
 * Specs import `test` from here (not `@playwright/test`) so every
 * BrowserContext is signed in as the admin and every test gets the
 * resolved suite fixture (baseURL, workspace slug, derived admin
 * username).
 *
 * The context signs in with a session cookie, through the same
 * /api/v1/auth/login a person uses, so the app's requests arrive the
 * way a real browser's do: no Authorization header, cookie session,
 * CSRF double-submit. The server treats a browser session differently
 * from a token caller (a collab-snapshot flush is honoured only from a
 * browser session), so a Bearer header here would test a client no
 * real browser is.
 */

export interface SuiteFixture {
	baseURL: string;
	workspaceSlug: string;
	adminEmail: string;
	adminUsername: string;
	apiToken: string;
	// The admin's own session token (padsess_), for platform-administration
	// calls: /api/v1/admin/* and account creation refuse an API token
	// (BUG-3361). Sent as a Bearer, it is not bound to a User-Agent.
	adminSessionToken: string;
}

function loadSuiteFixture(): SuiteFixture {
	const path = resolve(HERE, '.auth', 'fixture.json');
	return JSON.parse(readFileSync(path, 'utf8')) as SuiteFixture;
}

// Signs the context in as the admin. context.request shares the
// context's cookie jar, so the session and CSRF cookies the login sets
// are the ones the page then sends.
async function applySessionAuth(context: BrowserContext, fixture: SuiteFixture) {
	const res = await context.request.post(`${fixture.baseURL}/api/v1/auth/login`, {
		data: { email: fixture.adminEmail, password: ADMIN_PASSWORD }
	});
	if (!res.ok()) {
		throw new Error(`fixture login: ${res.status()} ${await res.text()}`);
	}
}

/**
 * BUG-2334: silence CROSS-ACTOR "X created: …" SSE toasts in a context.
 * Specs share one workspace, so other specs' seeding otherwise stacks
 * toasts over bottom-right UI and races unrelated clicks. Page-own toasts
 * are unaffected (see quietExternalToasts in the toast store). Applied to
 * every built-in `context`; specs that build their OWN workspace-visiting
 * contexts (browser.newContext) must call this too —
 * sse-toast-quiet.spec.ts's control leg is the one deliberate exception,
 * pinning the flag-off product behavior.
 */
export async function quietCrossActorToasts(context: BrowserContext): Promise<void> {
	await context.addInitScript(() => {
		try {
			localStorage.setItem('pad:e2e-quiet-external-toasts', '1');
		} catch {
			// storage unavailable — fall through to prod behavior
		}
	});
}

// Standalone helper for the occasional spec that wants to build its
// own isolated context (e.g. to assert unauthed behaviour).
export function suiteFixture(): SuiteFixture {
	return loadSuiteFixture();
}

// Sends the suite's API token on every request the page makes. Only for a
// spec whose reader must have a token's reach: the admin's browser session
// is an owner of every workspace, a token only of the ones it is a member of.
async function applyTokenAuth(context: BrowserContext, fixture: SuiteFixture) {
	await context.setExtraHTTPHeaders({
		Authorization: `Bearer ${fixture.apiToken}`
	});
}

// Extended test — use `import { test } from './fixtures'` in specs.
// The `context` fixture override signs the context in once, so
// downstream `page` navigation is authenticated. `pageAuth` picks how:
// 'session' (the default, a real browser) or 'token'
// (`test.use({ pageAuth: 'token' })`).
export const test = base.extend<{ fixture: SuiteFixture; pageAuth: 'session' | 'token' }>({
	pageAuth: ['session', { option: true }],
	context: async ({ context, pageAuth }, run) => {
		const fixture = loadSuiteFixture();
		if (pageAuth === 'token') {
			await applyTokenAuth(context, fixture);
		} else {
			await applySessionAuth(context, fixture);
		}
		await quietCrossActorToasts(context);
		await run(context);
	},
	fixture: async ({}, run) => {
		await run(loadSuiteFixture());
	}
});

export { expect } from '@playwright/test';
