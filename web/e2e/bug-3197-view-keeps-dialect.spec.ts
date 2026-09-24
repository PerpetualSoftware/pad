// Opening an item and editing nothing must write nothing, whatever the stored
// body's markdown dialect (BUG-3197).
//
// The editor does not reproduce every stored dialect byte for byte: `* one`
// comes back `- one`, a leading rule gains a blank line after it. The collab
// flush used to compare the editor's output only against the stored text, so
// merely OPENING such an item PATCHed the normalised body and wrote a version
// row. The count is taken server-side from the version list, as in
// bug-3030-teardown-write-count.spec.ts: page-scoped request counting misses a
// keepalive PATCH sent from a page that is being destroyed.
import { test, expect } from './fixtures';
import { browserLogin, EDITOR_SELECTOR, SYNCED_BADGE_SELECTOR, SYNC_TIMEOUT } from './lib/collab-helpers';

// Not fixed points of the editor round trip (each is measured on BUG-3197's
// trail). A canonical body is the CONTROL: it never wrote on a view.
// The last line carries the two shapes an appendTransaction plugin reshapes
// (a bare URL, a stray tag in prose), which a parse-only canonical form missed.
const NON_CANONICAL = '* one\n* two\n\nTitle\n=====\n\nan _emphasised_ word\n\nsee https://example.com/a/b and a <Table> mention';
const CANONICAL = '- one\n- two\n\n# Title\n\nan *emphasised* word';

async function seed(fixture, request, content: string): Promise<string> {
	const resp = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/collections/docs/items`, {
		headers: { Authorization: `Bearer ${fixture.apiToken}`, 'Content-Type': 'application/json' },
		data: { title: `BUG-3197 ${Date.now()}`, fields: '{}', content },
	});
	if (!resp.ok()) throw new Error(`doc create failed (${resp.status()}): ${await resp.text()}`);
	return ((await resp.json()) as { slug: string }).slug;
}

async function readItem(browser, fixture, slug: string): Promise<{ versions: number; content: string }> {
	const probe = await browser.newPage();
	await browserLogin(probe);
	const v = await probe.request.get(`/api/v1/workspaces/${fixture.workspaceSlug}/items/${slug}/versions`);
	const i = await probe.request.get(`/api/v1/workspaces/${fixture.workspaceSlug}/items/${slug}`);
	expect(v.ok() && i.ok(), 'item read failed — the instrument, not the product').toBeTruthy();
	const body = await v.json();
	const versions = Array.isArray(body) ? body.length : (body.versions?.length ?? 0);
	const content = ((await i.json()) as { content: string }).content;
	await probe.close();
	return { versions, content };
}

async function open(page, fixture, slug: string) {
	await browserLogin(page);
	await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/docs?item=${slug}`);
	await expect(page.locator(EDITOR_SELECTOR)).toBeVisible({ timeout: SYNC_TIMEOUT });
	await expect(page.locator(SYNCED_BADGE_SELECTOR)).toBeVisible({ timeout: SYNC_TIMEOUT });
}

// Past the 5 s idle flush, so the idle path has run, then a navigation, so the
// teardown path has run too.
for (const [name, body] of [
	['a non-canonical body', NON_CANONICAL],
	['CONTROL: a canonical body', CANONICAL],
] as const) {
	test(`viewing ${name} past the idle flush and leaving writes nothing`, async ({ page, browser, fixture, request }, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'one project is enough');
		test.setTimeout(90_000);
		const slug = await seed(fixture, request, body);
		const before = await readItem(browser, fixture, slug);
		await open(page, fixture, slug);
		await page.waitForTimeout(7_000);
		await page.goto('about:blank');
		await page.waitForTimeout(3_000);
		const after = await readItem(browser, fixture, slug);
		console.log(`${name} idle+leave: versions ${before.versions} -> ${after.versions}`);
		expect(after.versions - before.versions, 'a view with nothing typed wrote content').toBe(0);
		expect(after.content, 'the stored dialect was rewritten by a view').toBe(body);
	});
}

// Leaving INSIDE the idle window: only the teardown flush runs, and it is the
// first flush that needs the canonical form, at a moment the editor may already
// be destroyed.
test('viewing a non-canonical body and leaving inside the idle window writes nothing', async ({ page, browser, fixture, request }, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'one project is enough');
	test.setTimeout(90_000);
	const slug = await seed(fixture, request, NON_CANONICAL);
	const before = await readItem(browser, fixture, slug);
	await open(page, fixture, slug);
	await page.goto('about:blank');
	await page.waitForTimeout(3_000);
	const after = await readItem(browser, fixture, slug);
	console.log(`non-canonical quick leave: versions ${before.versions} -> ${after.versions}`);
	expect(after.versions - before.versions, 'a view with nothing typed wrote content').toBe(0);
	expect(after.content).toBe(NON_CANONICAL);
});

// A real edit still lands, and may normalise the whole body: that is accepted
// (the lead's ruling on BUG-3197), and pinned so it is a stated behaviour.
test('a real edit of a non-canonical body still writes, normalising the body', async ({ page, browser, fixture, request }, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'one project is enough');
	test.setTimeout(90_000);
	const slug = await seed(fixture, request, NON_CANONICAL);
	await open(page, fixture, slug);
	const marker = `edit-${Date.now()}`;
	const editor = page.locator(EDITOR_SELECTOR);
	await editor.locator('p').last().click();
	await page.keyboard.press('End');
	await page.keyboard.type(` ${marker}`);
	await expect(editor).toContainText(marker);
	await page.goto('about:blank');
	await page.waitForTimeout(3_000);
	const after = await readItem(browser, fixture, slug);
	expect(after.content).toContain(marker);
	expect(after.content).toContain('- one');
});
