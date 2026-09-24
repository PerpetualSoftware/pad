// ONE committed teardown must produce exactly ONE content write (BUG-3030).
//
// Page-scoped request counting cannot answer this. Measured: a navigate-away
// landed the content while page.on('request') observed ZERO PATCHes, because
// page.goto destroys the page and Playwright stops reporting its requests —
// the keepalive PATCH still goes out. So the count is taken SERVER-SIDE, from
// the item's `seq`, which BUG-3037 bumps on every mutation of the row. Delta
// seq is the number of writes that actually landed, and it is indifferent to
// what the client-side listener could still see.
import { test, expect } from './fixtures';
import { browserLogin, seedDoc, EDITOR_SELECTOR, SYNCED_BADGE_SELECTOR, SYNC_TIMEOUT } from './lib/collab-helpers';


// page.request shares the browser context's cookies; the bare `request`
// fixture does not, and reported "content landed = false" for a case that
// demonstrably had landed.
// VERSIONS, not seq. Measured: seq-delta for one teardown was 3 and 4, because
// seq bumps on EVERY mutation of the row and the claim is about CONTENT writes
// only — the instrument's question wider than the claim again. A content PATCH
// creates a version row, so counting versions asks the question being claimed.
// The no-edit CONTROL below is what proves the remaining count is attributable
// to the teardown flush rather than to ambient traffic.
async function readVersionCount(page, fixture, slug: string): Promise<number> {
	const res = await page.request.get(
		`/api/v1/workspaces/${fixture.workspaceSlug}/items/${slug}/versions`,
	);
	expect(res.ok(), 'version read failed — the instrument, not the product').toBeTruthy();
	const body = await res.json();
	return Array.isArray(body) ? body.length : (body.versions?.length ?? 0);
}

async function setup(page, fixture, request, marker: string, typeIt = true) {
	const { slug } = await seedDoc(fixture, request, `Teardown ${marker}`);
	await browserLogin(page);
	await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/docs?item=${slug}`);
	const editor = page.locator(EDITOR_SELECTOR);
	await expect(editor).toBeVisible({ timeout: SYNC_TIMEOUT });
	await expect(page.locator(SYNCED_BADGE_SELECTOR)).toBeVisible({ timeout: SYNC_TIMEOUT });
	if (typeIt) {
		await editor.click();
		await page.keyboard.type(marker);
		await expect(editor).toContainText(marker);
	}
	return slug;
}

test('a committed navigate-away writes the edit exactly once', async ({ page, browser, fixture, request }, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'one project is enough');
	test.setTimeout(90_000);
	const marker = `nav-${Date.now()}`;
	const slug = await setup(page, fixture, request, marker);
	const before = await readVersionCount(page, fixture, slug);

	await page.goto('about:blank');
	await new Promise((r) => setTimeout(r, 3000));

	const probe = await browser.newPage();
	await browserLogin(probe);
	const after = await readVersionCount(probe, fixture, slug);
	await probe.close();
	const writes = after - before;
	console.log(`navigate-away EDITED: versions ${before} -> ${after} (content writes = ${writes})`);
	// THE NUMBER THIS TEST EXISTS FOR. The re-arm is a setTimeout(0) inside
	// beforeunload, and on a COMMITTED navigation the sequence is
	// beforeunload -> [timer?] -> pagehide. If that timer ran first the latch
	// would re-arm and pagehide would flush the same bytes again — two writes
	// where main sent one. Measured at 1; if this ever reads 2, the re-arm must
	// key on EVIDENCE the page lived on (a once-listener for pointerdown /
	// keydown / focus) rather than on time.
	expect(writes, 'a committed navigate-away wrote more than once').toBe(1);
});

test('a committed close writes the edit exactly once', async ({ page, browser, fixture, request }, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'one project is enough');
	test.setTimeout(90_000);
	const marker = `close-${Date.now()}`;
	const slug = await setup(page, fixture, request, marker);
	const before = await readVersionCount(page, fixture, slug);

	await page.close({ runBeforeUnload: true });
	await new Promise((r) => setTimeout(r, 3000));

	const probe = await browser.newPage();
	await browserLogin(probe);
	const after = await readVersionCount(probe, fixture, slug);
	await probe.close();
	const writes = after - before;
	console.log(`close EDITED: versions ${before} -> ${after} (content writes = ${writes})`);
	expect(writes, 'a committed close wrote more than once').toBe(1);
});

// CONTROL: the same teardowns with NOTHING typed. Whatever this reports is
// ambient and must be subtracted from the legs above; if it is not 0 the edited
// numbers are not attributable to the teardown flush on their own.
test('CONTROL: navigate-away with nothing typed writes nothing', async ({ page, browser, fixture, request }, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'one project is enough');
	test.setTimeout(90_000);
	const slug = await setup(page, fixture, request, `ctl-nav-${Date.now()}`, false);
	const before = await readVersionCount(page, fixture, slug);
	await page.goto('about:blank');
	await new Promise((r) => setTimeout(r, 3000));
	const probe = await browser.newPage();
	await browserLogin(probe);
	const after = await readVersionCount(probe, fixture, slug);
	await probe.close();
	const writes = after - before;
	console.log(`CONTROL navigate-away NO EDIT: versions ${before} -> ${after} (content writes = ${writes})`);
	// Without this leg the 1s above are equally consistent with ambient traffic
	// writing once per teardown regardless of the flush.
	expect(writes, 'a teardown with nothing typed still wrote content').toBe(0);
});

test('CONTROL: close with nothing typed writes nothing', async ({ page, browser, fixture, request }, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'one project is enough');
	test.setTimeout(90_000);
	const slug = await setup(page, fixture, request, `ctl-close-${Date.now()}`, false);
	const before = await readVersionCount(page, fixture, slug);
	await page.close({ runBeforeUnload: true });
	await new Promise((r) => setTimeout(r, 3000));
	const probe = await browser.newPage();
	await browserLogin(probe);
	const after = await readVersionCount(probe, fixture, slug);
	await probe.close();
	const writes = after - before;
	console.log(`CONTROL close NO EDIT: versions ${before} -> ${after} (content writes = ${writes})`);
	expect(writes, 'a teardown with nothing typed still wrote content').toBe(0);
});
