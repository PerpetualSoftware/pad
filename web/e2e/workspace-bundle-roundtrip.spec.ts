import { writeFile } from 'node:fs/promises';
import { join } from 'node:path';
import { tmpdir } from 'node:os';
import { test, expect, type SuiteFixture } from './fixtures';

/**
 * Workspace bundle round-trip e2e (TASK-894 under PLAN-890).
 *
 * Closes the loop on the web-UI / CLI uniformity work: an export
 * downloaded from the settings page must be importable through the
 * Create Workspace dialog without a CLI hop, and the imported
 * workspace must carry items + attachments + content references
 * intact. Catches any regression that splits the export and import
 * formats again.
 *
 * Flow:
 *   1. API-seed the source workspace with a task + a PNG attachment
 *      embedded in an item's markdown content (covers the
 *      pad-attachment:UUID rewrite path that the round-trip
 *      depends on).
 *   2. Verify the settings-page export link is wired correctly
 *      (href + download attribute) — this is the user-visible
 *      contract of the export half. Then fetch the bundle bytes
 *      via the auth'd API request (browser-driven downloads via
 *      <a download> don't carry the Bearer header through
 *      Playwright's download fork, and re-implementing that auth
 *      handshake in the spec would test the test, not the feature).
 *   3. Drive the import through the UI: open Create Workspace
 *      modal → switch to Import tab → setInputFiles on the hidden
 *      file input → submit → wait for navigation to the new
 *      workspace.
 *   4. Assert via the API that the new workspace contains the
 *      seeded task with the rewritten attachment reference and
 *      that the rehydrated attachment is downloadable with bytes
 *      matching the original.
 *
 * Why API-seed instead of UI-seed:
 *   The test focus is the export/import surface, not item
 *   creation. API seeding keeps the spec under 30s and immune to
 *   editor-UI churn.
 *
 * Why a real PNG:
 *   The bundle import path re-validates MIME against the actual
 *   bytes (handlers_import_bundle.go:358). A placeholder buffer
 *   would be rejected as "not an allowed type". The bytes here are
 *   the same minimal 1x1 PNG used by the Go-side attachment tests
 *   (handlers_attachments_test.go:realPNG).
 */

// 1x1 transparent PNG. Identical to the byte sequence in
// internal/server/handlers_attachments_test.go realPNG() — kept in
// sync intentionally so the e2e exercises the same MIME-validation
// path the Go tests do.
const REAL_PNG = Buffer.from([
	0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, 0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4,
	0x89, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x44, 0x41, 0x54, 0x78, 0x9c, 0x63, 0x00, 0x01, 0x00, 0x00,
	0x05, 0x00, 0x01, 0x0d, 0x0a, 0x2d, 0xb4, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4e, 0x44, 0xae,
	0x42, 0x60, 0x82
]);

interface RequestCtx {
	post(
		url: string,
		options: { headers?: Record<string, string>; data?: unknown; multipart?: unknown }
	): Promise<{ ok(): boolean; status(): number; text(): Promise<string>; json(): Promise<unknown> }>;
	get(
		url: string,
		options?: { headers?: Record<string, string> }
	): Promise<{
		ok(): boolean;
		status(): number;
		text(): Promise<string>;
		json(): Promise<unknown>;
		body(): Promise<Buffer>;
	}>;
}

async function seedSourceWorkspace(
	fixture: SuiteFixture,
	request: RequestCtx
): Promise<{ attachmentID: string; itemSlug: string }> {
	const ws = fixture.workspaceSlug;
	const authHeader = { Authorization: `Bearer ${fixture.apiToken}` };
	const jsonHeaders = { ...authHeader, 'Content-Type': 'application/json' };

	// Upload an attachment via multipart so the source workspace has
	// a real blob to round-trip.
	const uploadResp = await request.post(`/api/v1/workspaces/${ws}/attachments`, {
		headers: authHeader,
		multipart: {
			file: { name: 'roundtrip-logo.png', mimeType: 'image/png', buffer: REAL_PNG }
		}
	});
	if (!uploadResp.ok()) {
		throw new Error(`upload attachment failed (${uploadResp.status()}): ${await uploadResp.text()}`);
	}
	const upload = (await uploadResp.json()) as { id: string };

	// Item that embeds the attachment in markdown — this exercises
	// the pad-attachment:OLD → pad-attachment:NEW rewrite that the
	// import path performs after rehydration.
	const itemTitle = `Round-trip test ${Date.now()}`;
	const itemContent = `# Round-trip\n\n![logo](pad-attachment:${upload.id})\n`;
	const itemResp = await request.post(`/api/v1/workspaces/${ws}/collections/docs/items`, {
		headers: jsonHeaders,
		data: { title: itemTitle, content: itemContent }
	});
	if (!itemResp.ok()) {
		throw new Error(`item create failed (${itemResp.status()}): ${await itemResp.text()}`);
	}
	const item = (await itemResp.json()) as { slug: string };

	return { attachmentID: upload.id, itemSlug: item.slug };
}

test('workspace export → import round-trip via web UI', async ({ page, fixture, request }, testInfo) => {
	// Run desktop-chromium only. The round-trip flow exercises the
	// same modal, API path, and assertions on both viewports —
	// there's no mobile-specific code to cover. Running both
	// projects in parallel trips the server's general-API rate
	// limit (~10 req/sec) because the seed + export + import +
	// verify sequence makes ~30 calls per worker against the same
	// Pad instance. Pinning to one project keeps the spec
	// deterministic without lowering the production rate limit or
	// adding test-only env knobs to the binary.
	test.skip(
		testInfo.project.name !== 'desktop-chromium',
		'round-trip is viewport-agnostic; one project is enough'
	);

	// The full round-trip touches the settings page, a download
	// stream, the create-workspace modal, and the new workspace's
	// API. The default 30s easily covers it on a developer laptop;
	// CI agents and Linux containers benefit from a bit more
	// headroom.
	test.setTimeout(60_000);

	// 1. Seed source workspace.
	const seeded = await seedSourceWorkspace(fixture, request);

	// 2. Verify the settings-page export link is wired correctly.
	// This pins the user-visible contract of the export half: a
	// link with the right href + download attribute.
	await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/settings`);
	await page.waitForLoadState('domcontentloaded');

	const downloadLink = page.getByRole('link', { name: /download \.tar\.gz/i });
	await expect(downloadLink).toBeVisible();
	await expect(downloadLink).toHaveAttribute('href', /\?format=tar$/);
	await expect(downloadLink).toHaveAttribute('download', /\.tar\.gz$/);

	// Fetch the actual bundle bytes via the auth'd API request.
	// Playwright's download fork from <a download> doesn't carry
	// extraHTTPHeaders (Bearer token), so the browser-driven path
	// would 401. The link's correctness is already pinned above;
	// what we still need for the import half is real bundle bytes
	// that the server's bundle handler produced.
	const bundleResp = await request.get(
		`/api/v1/workspaces/${fixture.workspaceSlug}/export?format=tar`,
		{ headers: { Authorization: `Bearer ${fixture.apiToken}` } }
	);
	if (!bundleResp.ok()) {
		throw new Error(`bundle export failed (${bundleResp.status()}): ${await bundleResp.text()}`);
	}
	const bundleBytes = await bundleResp.body();
	const bundlePath = join(tmpdir(), `pad-roundtrip-${Date.now()}.tar.gz`);
	await writeFile(bundlePath, bundleBytes);

	// 3. Open the Create Workspace modal and switch to Import.
	// On desktop the TopBar exposes a "+" button (title="New workspace");
	// on mobile that button is hidden behind the layout's collapse.
	// Try the TopBar trigger first and fall back to opening the
	// WorkspaceSwitcher dropdown/sheet and clicking its
	// "+ New Workspace" entry. Either path sets the same
	// uiStore.createWorkspaceOpen flag and renders the same modal.
	await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}`);
	await page.waitForLoadState('domcontentloaded');

	// Wait for the workspace shell to actually hydrate before probing for
	// the topbar trigger. The shell is fully client-rendered (adapter-static
	// has no SSR for app routes), and `domcontentloaded` fires before the
	// SvelteKit app has rehydrated the workspace store. A naked sync
	// `isVisible()` check below would race that hydration on slower CI
	// runners and fall through to the mobile branch on a desktop project,
	// which then times out waiting for an element that doesn't exist on
	// desktop. Anchor the wait on the workspace heading instead — it's
	// rendered by the dashboard route the moment hydration completes.
	await expect(page.getByRole('heading', { name: /E2E Workspace/i })).toBeVisible();

	const topbarTrigger = page.getByTitle('New workspace');
	if (await topbarTrigger.first().isVisible().catch(() => false)) {
		await topbarTrigger.first().click();
	} else {
		// Mobile path: tap the workspace switcher's current-workspace
		// button to open the picker, then click "+ New Workspace".
		// "E2E Workspace" is the seeded display name (WORKSPACE_NAME
		// in global-setup.ts); the switcher button uses it as its
		// accessible label.
		await page.getByRole('button', { name: /e2e workspace/i }).first().click();
		await page.getByRole('button', { name: /\+ new workspace/i }).click();
	}

	await expect(page.getByRole('dialog')).toBeVisible();

	// Modal opens in 'create' mode; switch to import.
	const importTab = page.getByRole('button', { name: /^import$/i });
	await importTab.click();

	// Set an explicit name so the test doesn't depend on the
	// auto-fill regex (which is exercised by other coverage). The
	// server uniquifies on collision so even reruns won't conflict.
	const importName = `roundtrip-${Date.now()}`;
	await page.getByLabel(/^name$/i).fill(importName);

	// The file input is display:none so we can't click-then-pick;
	// setInputFiles works on hidden inputs.
	await page.locator('input[type="file"]').setInputFiles(bundlePath);

	// 4. Submit. The modal closes on success and the page navigates
	// to the new workspace's dashboard. Wait for the URL to land on
	// a path that contains the explicit importName — a more lax
	// pattern (any /foo/bar URL) would match the source URL we're
	// already on and resolve without actually waiting.
	await Promise.all([
		page.waitForURL(new RegExp(`/${importName}(?:[/?#]|$)`), { timeout: 30_000 }),
		page.getByRole('button', { name: /^import workspace$/i }).click()
	]);

	// Read back the new workspace's slug from the URL — the server
	// may have uniquified it.
	const newSlug = page.url().replace(/[?#].*/, '').split('/').filter(Boolean).pop() ?? '';
	expect(newSlug).toContain(importName);

	// 5. Verify the imported workspace via the API.
	const auth = { Authorization: `Bearer ${fixture.apiToken}` };

	// 5a. The seeded item must exist in the new workspace's docs
	// collection. The slug is preserved by ImportWorkspace so we can
	// look it up directly.
	const itemResp = await request.get(`/api/v1/workspaces/${newSlug}/items/${seeded.itemSlug}`, {
		headers: auth
	});
	if (!itemResp.ok()) {
		throw new Error(
			`imported item lookup failed (${itemResp.status()}): ${await itemResp.text()}`
		);
	}
	const importedItem = (await itemResp.json()) as { content: string };

	// 5b. The original attachment id must NOT appear in the imported
	// content (rewrite ran), and the new content must reference SOME
	// pad-attachment: UUID (the rewritten one).
	expect(importedItem.content).not.toContain(`pad-attachment:${seeded.attachmentID}`);
	expect(importedItem.content).toMatch(/pad-attachment:[0-9a-f-]{36}/);

	// 5c. The rehydrated attachment row must be downloadable and
	// the bytes must match the original PNG. This is the strongest
	// possible round-trip assertion — every layer (export tar →
	// import dispatch → rehydrate → storage backend → download)
	// must work or the bytes won't match.
	const attListResp = await request.get(`/api/v1/workspaces/${newSlug}/attachments`, {
		headers: auth
	});
	if (!attListResp.ok()) {
		throw new Error(
			`imported attachments list failed (${attListResp.status()}): ${await attListResp.text()}`
		);
	}
	const attList = (await attListResp.json()) as { attachments: { id: string; filename: string }[] };
	expect(attList.attachments.length).toBeGreaterThan(0);
	const newAtt = attList.attachments.find((a) => a.filename === 'roundtrip-logo.png');
	expect(newAtt, 'imported attachment with the seeded filename should exist').toBeTruthy();
	expect(newAtt!.id).not.toBe(seeded.attachmentID);

	const blobResp = await request.get(
		`/api/v1/workspaces/${newSlug}/attachments/${newAtt!.id}`,
		{ headers: auth }
	);
	if (!blobResp.ok()) {
		throw new Error(`download imported blob failed (${blobResp.status()})`);
	}
	const downloadedBytes = await blobResp.body();
	expect(downloadedBytes.length).toBe(REAL_PNG.length);
	expect(downloadedBytes.equals(REAL_PNG)).toBe(true);
});
