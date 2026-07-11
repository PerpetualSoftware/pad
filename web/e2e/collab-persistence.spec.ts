import { test, expect, type SuiteFixture } from './fixtures';
import { ADMIN_EMAIL, ADMIN_PASSWORD } from './global-setup';
import type { Browser, Page } from '@playwright/test';

/**
 * Editor / collab persistence e2e (TASK-2058 under TASK-733).
 *
 * The editor path is the subsystem with the strictest cross-component
 * invariants — the Yjs schema-version handshake and the WebSocket
 * op-log round-trip — yet it had no e2e coverage. This spec drives the
 * full chain through the real embedded UI:
 *
 *   type in Tiptap → Y.Doc update → WS connect + schema_version
 *   handshake → op-log persist → collab-snapshot flush to items.content
 *   → reload re-hydrates from items.content
 *
 * If any layer of that chain regresses (a schema-version bump that
 * doesn't match the server's DefaultSchemaVersion, a broken WS auth,
 * an op-log flush that never lands), the reloaded editor won't carry
 * the typed text and this goes red.
 *
 * Determinism notes:
 *   - We wait for the "Synced" collab badge before typing so the Y.Doc
 *     binding is live — typing into an unbound editor would be a
 *     false negative.
 *   - We wait for the actual collab-snapshot PATCH response (the 5s
 *     idle flush that writes items.content) rather than sleeping, so
 *     the reload assertion can't race the persistence write.
 *
 * Why we log in through the browser instead of reusing the fixture's
 * Bearer token: the collab WebSocket handshake can't carry an
 * `Authorization` header (browsers don't let JS set headers on a WS
 * upgrade), so the server authenticates it from the session cookie.
 * The suite's default token auth therefore can't reach the WS. We
 * establish a same-browser session cookie via an in-page login — the
 * cookie is minted under the browser's own User-Agent, so the
 * UA-bound session survives the WS upgrade (unlike a node-minted
 * session; see fixtures.ts).
 */

async function browserLogin(page: Page) {
	// Land on a same-origin page first so the fetch + Set-Cookie land in
	// the browser's cookie jar (and under the browser's UA).
	await page.goto('/login');
	const status = await page.evaluate(
		async ({ email, password }) => {
			const r = await fetch('/api/v1/auth/login', {
				method: 'POST',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify({ email, password })
			});
			return r.status;
		},
		{ email: ADMIN_EMAIL, password: ADMIN_PASSWORD }
	);
	if (status !== 200) throw new Error(`in-page login failed with status ${status}`);
}

const enc = (obj: Record<string, unknown>) => JSON.stringify(obj);

async function seedDoc(
	fixture: SuiteFixture,
	request: { post: (url: string, o: unknown) => Promise<{ ok(): boolean; status(): number; text(): Promise<string>; json(): Promise<unknown> }> }
): Promise<{ slug: string }> {
	const ws = fixture.workspaceSlug;
	const headers = { Authorization: `Bearer ${fixture.apiToken}`, 'Content-Type': 'application/json' };
	// Empty content: the editor starts on a single empty paragraph, so
	// the text we type becomes the item's entire body — a clean anchor
	// for the post-reload assertion.
	const resp = await request.post(`/api/v1/workspaces/${ws}/collections/docs/items`, {
		headers,
		data: { title: `Collab persistence ${Date.now()}`, fields: enc({}), content: '' }
	});
	if (!resp.ok()) throw new Error(`doc create failed (${resp.status()}): ${await resp.text()}`);
	return (await resp.json()) as { slug: string };
}

test('typed editor content survives a reload via the collab op-log', async ({
	page,
	fixture,
	request
}, testInfo) => {
	// Viewport-agnostic: the collab chain is identical on both projects
	// and running both in parallel doubles WS load on the single test
	// instance for no added coverage. Pin to desktop.
	test.skip(
		testInfo.project.name !== 'desktop-chromium',
		'collab persistence is viewport-agnostic; one project is enough'
	);
	// The 5s idle flush plus WS handshake needs more than the 30s default
	// on a busy CI runner.
	test.setTimeout(60_000);

	const { slug } = await seedDoc(fixture, request);
	const marker = `collab-persist-${Date.now()}`;

	await browserLogin(page);
	await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/docs/${slug}`);

	// Editor mounts and the collab provider reaches "Synced" — this is
	// the schema-version handshake completing against the server's
	// DefaultSchemaVersion. Typing before this races the Y.Doc binding.
	const editor = page.locator('.editor-content .ProseMirror');
	await expect(editor).toBeVisible();
	await expect(page.locator('.collab-state-synced')).toBeVisible();

	// Register the flush wait BEFORE typing so we can't miss the response.
	// The collab-snapshot PATCH is the write that persists Y.Doc state to
	// items.content — the exact round-trip a reload reads back.
	// The flush PATCHes /items/{id}?source=collab-snapshot (by item UUID,
	// not slug) and its response echoes the persisted item, so we scope
	// the wait to a collab-snapshot PATCH whose response body carries the
	// marker. This pins the awaited event to the flush that wrote OUR
	// typed text into items.content — an early empty-doc hydration flush
	// (which dedupes to a no-op PATCH anyway) can't satisfy it, so the
	// reload assertion below can't false-pass on op-log replay alone.
	const flushed = page.waitForResponse(
		async (r) =>
			r.url().includes('source=collab-snapshot') &&
			r.request().method() === 'PATCH' &&
			r.ok() &&
			(await r.text()).includes(marker),
		{ timeout: 30_000 }
	);

	await editor.click();
	await page.keyboard.type(marker);
	await expect(editor).toContainText(marker);

	// Proves items.content now holds the marker (the response is the
	// persisted item). This is the storage round-trip, independent of
	// what the op-log replay would recover.
	await flushed;

	// Reload: a fresh page load re-hydrates the Y.Doc from items.content
	// (+ op-log replay). The typed text must come back or a layer of the
	// persistence chain is broken.
	await page.reload();
	const reloaded = page.locator('.editor-content .ProseMirror');
	await expect(reloaded).toBeVisible();
	await expect(reloaded).toContainText(marker);
});

/**
 * Two-client convergence: text typed in one browser context appears in
 * a second context viewing the same item — no reload. This exercises
 * the live relay path (Y.Doc update → server broadcast → peer apply)
 * that the persistence test above doesn't: persistence proves the
 * op-log survives a round-trip through storage; convergence proves the
 * server fans an update out to a concurrently-connected peer.
 */
async function openSyncedEditor(
	browser: Browser,
	fixture: SuiteFixture,
	path: string
): Promise<{ page: Page; close: () => Promise<void> }> {
	// Each context has its own cookie jar, so it needs its own in-page
	// login for the WS handshake. Carry the Bearer header too so ordinary
	// HTTP navigation is authed (matches the suite fixture).
	const context = await browser.newContext({
		baseURL: fixture.baseURL,
		extraHTTPHeaders: { Authorization: `Bearer ${fixture.apiToken}` }
	});
	const page = await context.newPage();
	await browserLogin(page);
	await page.goto(path);
	await expect(page.locator('.editor-content .ProseMirror')).toBeVisible();
	await expect(page.locator('.collab-state-synced')).toBeVisible();
	return { page, close: () => context.close() };
}

test('an edit in one client converges to a second client viewing the same item', async ({
	browser,
	fixture,
	request
}, testInfo) => {
	test.skip(
		testInfo.project.name !== 'desktop-chromium',
		'collab convergence is viewport-agnostic; one project is enough'
	);
	test.setTimeout(60_000);

	const { slug } = await seedDoc(fixture, request);
	const path = `/${fixture.adminUsername}/${fixture.workspaceSlug}/docs/${slug}`;
	const marker = `converge-${Date.now()}`;

	const a = await openSyncedEditor(browser, fixture, path);
	const b = await openSyncedEditor(browser, fixture, path);

	try {
		// A types; the relay must fan the op out to B's live Y.Doc without
		// either page reloading.
		const editorA = a.page.locator('.editor-content .ProseMirror');
		await editorA.click();
		await a.page.keyboard.type(marker);

		const editorB = b.page.locator('.editor-content .ProseMirror');
		await expect(editorB).toContainText(marker, { timeout: 15_000 });
	} finally {
		await a.close();
		await b.close();
	}
});
