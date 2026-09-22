// BUG-3124 unit B: merely VIEWING an item must not leave it reading
// `content_state: applied_pending_flush` forever.
//
// Opening an item in a tab persists op-log rows (the tab's seed update, its
// sync frames) above the flush watermark, and the view dedupe (BUG-1899) means
// no flush ever covers them — so before this change the item read "pending"
// from the first view onward, and re-opening it could not clear it. A
// caught-up tab now STAMPS the watermark (POST …/collab-watermark) when its
// flush dedupes.
//
// Instruments are read server-side, through a second page, for the reason
// bug-3030-teardown-write-count.spec.ts records: a page that navigates away
// stops reporting its requests while its keepalive request still lands.
import { test, expect } from './fixtures';
import { browserLogin, EDITOR_SELECTOR, SYNCED_BADGE_SELECTOR } from './lib/collab-helpers';

const SYNC_TIMEOUT = 20_000;
const BODY = 'Seed body for BUG-3124 — a paragraph a tab will seed from.';

async function seedWithBody(fixture, request): Promise<string> {
	const resp = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/collections/docs/items`, {
		headers: { Authorization: `Bearer ${fixture.apiToken}`, 'Content-Type': 'application/json' },
		data: { title: `BUG-3124 ${Date.now()}`, content: BODY },
	});
	if (!resp.ok()) throw new Error(`doc create failed (${resp.status()}): ${await resp.text()}`);
	return ((await resp.json()) as { slug: string }).slug;
}

async function readItem(page, fixture, slug: string) {
	const res = await page.request.get(`/api/v1/workspaces/${fixture.workspaceSlug}/items/${slug}`);
	expect(res.ok(), 'item read failed — the instrument, not the product').toBeTruthy();
	return (await res.json()) as { content_state?: string; content: string; seq: number };
}

async function readVersionCount(page, fixture, slug: string): Promise<number> {
	const res = await page.request.get(`/api/v1/workspaces/${fixture.workspaceSlug}/items/${slug}/versions`);
	expect(res.ok(), 'version read failed — the instrument, not the product').toBeTruthy();
	const body = await res.json();
	return Array.isArray(body) ? body.length : (body.versions?.length ?? 0);
}

async function openItem(page, fixture, slug: string) {
	await browserLogin(page);
	await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/docs?item=${slug}`);
	await expect(page.locator(EDITOR_SELECTOR)).toBeVisible({ timeout: SYNC_TIMEOUT });
	await expect(page.locator(SYNCED_BADGE_SELECTOR)).toBeVisible({ timeout: SYNC_TIMEOUT });
	await expect(page.locator(EDITOR_SELECTOR)).toContainText('Seed body for BUG-3124');
}

test('a view that changes nothing leaves the item clean, with no content write', async ({ page, browser, fixture, request }, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'one project is enough');
	test.setTimeout(90_000);
	const slug = await seedWithBody(fixture, request);
	const probe = await browser.newPage();
	await browserLogin(probe);
	const versionsBefore = await readVersionCount(probe, fixture, slug);

	await openItem(page, fixture, slug);

	// PREMISE: the view put rows above the watermark. Without this the clean
	// read below could mean the view never touched the op-log at all, and the
	// test would pass on a build with no stamp.
	await expect
		.poll(async () => (await readItem(probe, fixture, slug)).content_state ?? '', {
			timeout: 5_000,
			message: 'premise: opening the item must leave op-log rows above the watermark',
		})
		.toBe('applied_pending_flush');

	// THE CLAIM: while the tab stays open and idle, it stamps.
	await expect
		.poll(async () => (await readItem(probe, fixture, slug)).content_state ?? '', {
			timeout: 20_000,
			message: 'an idle, caught-up tab must stamp the watermark and clear the pending state',
		})
		.toBe('');
	const after = await readItem(probe, fixture, slug);
	expect(after.content).toBe(BODY);
	expect(await readVersionCount(probe, fixture, slug), 'the stamp must not write a content version').toBe(versionsBefore);
	await probe.close();
});

test('navigating away before the idle flush still stamps, via pagehide', async ({ page, browser, fixture, request }, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'one project is enough');
	test.setTimeout(90_000);
	const slug = await seedWithBody(fixture, request);
	await openItem(page, fixture, slug);

	const probe = await browser.newPage();
	await browserLogin(probe);
	// PREMISE, as above: the view has put rows above the watermark.
	await expect
		.poll(async () => (await readItem(probe, fixture, slug)).content_state ?? '', { timeout: 5_000 })
		.toBe('applied_pending_flush');
	// Leave well inside the 5s idle window, so only the pagehide flushNow can
	// have stamped.
	await page.goto('about:blank');

	await expect
		.poll(async () => (await readItem(probe, fixture, slug)).content_state ?? '', {
			timeout: 10_000,
			message: 'the pagehide flush dedupes and must stamp with a keepalive request',
		})
		.toBe('');
	await probe.close();
});
