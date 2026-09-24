import { expect, type APIRequestContext, type Locator, type Page } from '@playwright/test';
import type { SuiteFixture } from '../fixtures';
import { ADMIN_EMAIL, ADMIN_PASSWORD } from '../global-setup';

/**
 * Shared scaffolding for the collab / editor e2e specs
 * (collab-persistence.spec.ts, pane-collab-teardown.spec.ts).
 *
 * Why an in-page login instead of the fixture's Bearer token: the collab
 * WebSocket handshake can't carry an `Authorization` header (browsers
 * don't let JS set headers on a WS upgrade), so the server authenticates
 * it from the session cookie. The suite's default token auth therefore
 * can't reach the WS. We establish a same-browser session cookie via an
 * in-page login — the cookie is minted under the browser's own
 * User-Agent, so the UA-bound session survives the WS upgrade (unlike a
 * node-minted session; see fixtures.ts).
 */
export async function browserLogin(page: Page): Promise<void> {
	// Land on a same-origin page first so the fetch + Set-Cookie land in
	// the browser's cookie jar (and under the browser's UA).
	await page.goto('/login');
	const status = await page.evaluate(
		async ({ email, password }) => {
			const r = await fetch('/api/v1/auth/login', {
				method: 'POST',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify({ email, password }),
			});
			return r.status;
		},
		{ email: ADMIN_EMAIL, password: ADMIN_PASSWORD },
	);
	if (status !== 200) throw new Error(`in-page login failed with status ${status}`);
}

const enc = (obj: Record<string, unknown>) => JSON.stringify(obj);

/**
 * Seed an empty doc item and return its identity. Empty content means the
 * editor starts on a single empty paragraph, so text we later type
 * becomes the item's entire body — a clean anchor for content assertions.
 *
 * `fields` seeds the item's structured `fields` JSON (e.g. `{ status:
 * 'draft', category: 'general' }`) — defaults to `{}` (schema defaults
 * apply server-side) for every pre-existing caller.
 */
export async function seedDoc(
	fixture: SuiteFixture,
	request: APIRequestContext,
	titlePrefix = 'Collab persistence',
	fields: Record<string, unknown> = {},
): Promise<{ id: string; slug: string }> {
	const ws = fixture.workspaceSlug;
	const headers = {
		Authorization: `Bearer ${fixture.apiToken}`,
		'Content-Type': 'application/json',
	};
	const resp = await request.post(`/api/v1/workspaces/${ws}/collections/docs/items`, {
		headers,
		data: { title: `${titlePrefix} ${Date.now()}`, fields: enc(fields), content: '' },
	});
	if (!resp.ok()) throw new Error(`doc create failed (${resp.status()}): ${await resp.text()}`);
	return (await resp.json()) as { id: string; slug: string };
}

/** The live-editor ProseMirror surface — shared selector across specs. */
export const EDITOR_SELECTOR = '.editor-content .ProseMirror';
/** The "Synced" collab-state badge — present once the Y.Doc binding is live. */
export const SYNCED_BADGE_SELECTOR = '.collab-state-synced';

/**
 * The suite's budget for a collab handshake (socket, sync, reconnect backoff).
 * One definition: six specs each declared their own `SYNC_TIMEOUT = 20_000`
 * before BUG-3152 moved it here.
 */
export const SYNC_TIMEOUT = 20_000;

/**
 * How long a spec waits for the collab editor's FIRST mount after a goto or a
 * reload (BUG-3152). It is SYNC_TIMEOUT, since a first mount includes that
 * handshake.
 *
 * The first mount sits behind a chain of sequential requests (session, the
 * item, its collection, then the collab socket and its sync), and under box
 * contention (load 17-23 on 8 cores, full local runs at 4 workers) one request
 * in that chain stalled 1.4-2.2s, so the default 5s expired with the socket
 * still connecting (version-restore-collab:209 twice, collab-persistence:41
 * once, in 8 runs). Measured, not assumed: that stall is NOT a product defect.
 * The same reads under 40 concurrent writers in isolation stayed under 90ms
 * (BUG-3152 checkpoint 7).
 *
 * MOUNT LATENCY UNDER LOAD IS NOT WHAT THESE SPECS TEST. Use this only for the
 * first-mount wait. Assertions about content or ordering keep their own
 * budgets, so this widens nothing a spec is actually checking.
 */
export const EDITOR_MOUNT_TIMEOUT = SYNC_TIMEOUT;

/** Wait for the collab editor's first mount; returns its locator. */
export async function expectEditorMounted(page: Page): Promise<Locator> {
	const editor = page.locator(EDITOR_SELECTOR);
	await expect(editor).toBeVisible({ timeout: EDITOR_MOUNT_TIMEOUT });
	return editor;
}
