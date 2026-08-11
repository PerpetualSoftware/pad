import { test as base, type BrowserContext } from '@playwright/test';
import { readFileSync } from 'node:fs';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const HERE = dirname(fileURLToPath(import.meta.url));

/**
 * Shared test fixtures.
 *
 * Specs import `test` from here (not `@playwright/test`) so every
 * BrowserContext is pre-authenticated with the admin's API token and
 * every test gets the resolved suite fixture (baseURL, workspace slug,
 * derived admin username).
 *
 * We use a Bearer token rather than a session cookie because the Pad
 * server binds sessions to the originating User-Agent (see
 * internal/server/middleware_auth.go). A session minted in node.js and
 * replayed from a headless Chromium context would be silently rejected;
 * tokens have no such binding.
 */

export interface SuiteFixture {
	baseURL: string;
	workspaceSlug: string;
	adminEmail: string;
	adminUsername: string;
	apiToken: string;
}

function loadSuiteFixture(): SuiteFixture {
	const path = resolve(HERE, '.auth', 'fixture.json');
	return JSON.parse(readFileSync(path, 'utf8')) as SuiteFixture;
}

async function applyTokenAuth(context: BrowserContext, fixture: SuiteFixture) {
	await context.setExtraHTTPHeaders({
		Authorization: `Bearer ${fixture.apiToken}`
	});
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

// Extended test — use `import { test } from './fixtures'` in specs.
// The `context` fixture override applies the Bearer header once per
// context so downstream `page` navigation is authenticated.
export const test = base.extend<{ fixture: SuiteFixture }>({
	context: async ({ context }, run) => {
		const fixture = loadSuiteFixture();
		await applyTokenAuth(context, fixture);
		await quietCrossActorToasts(context);
		await run(context);
	},
	fixture: async ({}, run) => {
		await run(loadSuiteFixture());
	}
});

export { expect } from '@playwright/test';
