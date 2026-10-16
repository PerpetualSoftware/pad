import { test, expect } from './fixtures';
import { browserLogin, SYNCED_BADGE_SELECTOR, SYNC_TIMEOUT } from './lib/collab-helpers';
import { authJson } from './lib/attachment-viewer';

/**
 * TASK-3607: restoring a kept draft after someone else saved the item applies
 * only the draft's own changes (base → draft, anchored on the base's text,
 * through the patch endpoint), so the newer version survives. When the same
 * text changed on both sides, nothing is written: the notice shows the diff
 * and leads with Copy.
 *
 * The draft is seeded as the crash keeper writes it (TASK-3606 proves the
 * keeper), with the base it was edited from. The page holds the item's room,
 * so the patch goes through its patch-capable editor, the path a real
 * restore takes.
 */
const BODY = 'Paragraph one.\n\nParagraph two.\n\nParagraph three.';

type Fixture = { workspaceSlug: string; adminUsername: string };

async function setUp(request: import('@playwright/test').APIRequestContext, fixture: Fixture, title: string) {
	const headers = authJson(fixture as never);
	const created = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/collections/docs/items`, {
		headers,
		data: { title, content: BODY },
	});
	expect(created.ok(), await created.text()).toBeTruthy();
	const { id, ref } = (await created.json()) as { id: string; ref: string };
	const base = (await (await request.get(`/api/v1/workspaces/${fixture.workspaceSlug}/items/${id}`, { headers })).json()) as {
		content: string;
		seq: number;
	};
	const me = (await (await request.get('/api/v1/auth/me', { headers })).json()) as { id: string };
	return { id, ref, base, key: `pad:refused-raw-draft:${me.id}:${id}`, headers };
}

async function collaboratorWrites(request: import('@playwright/test').APIRequestContext, fixture: Fixture, id: string, headers: Record<string, string>, content: string) {
	const res = await request.patch(`/api/v1/workspaces/${fixture.workspaceSlug}/items/${id}`, { headers, data: { content } });
	expect(res.ok(), await res.text()).toBeTruthy();
}

async function openWithDraft(
	browser: import('@playwright/test').Browser,
	fixture: Fixture,
	ref: string,
	key: string,
	draft: { markdown: string; savedAt: number; baseSeq: number; baseBody: string; baseStale?: true },
) {
	const ctx = await browser.newContext();
	await ctx.addInitScript(([k, v]) => localStorage.setItem(k, v), [key, JSON.stringify(draft)] as const);
	const page = await ctx.newPage();
	await browserLogin(page);
	await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/docs?item=${ref}`);
	await expect(page.locator(SYNCED_BADGE_SELECTOR)).toBeVisible({ timeout: SYNC_TIMEOUT });
	return { ctx, page };
}

test('a collaborator changed paragraph one, the draft changed paragraph three: Restore keeps both', async ({ browser, fixture, request }, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'one project is enough for a restore flow');
	test.setTimeout(120_000);
	const { id, ref, base, key, headers } = await setUp(request, fixture, `TASK-3607 both ${Date.now()}`);
	await collaboratorWrites(request, fixture, id, headers, base.content.replace('Paragraph one.', 'Paragraph one, theirs.'));
	const draft = { markdown: base.content.replace('Paragraph three.', 'Paragraph three, mine.'), savedAt: Date.now(), baseSeq: base.seq, baseBody: base.content };

	const { ctx, page } = await openWithDraft(browser, fixture, ref, key, draft);
	const notice = page.getByTestId('refused-raw-draft');
	await expect(notice).toContainText('restoring applies only your changes');
	await notice.getByRole('button', { name: 'Restore it' }).click();

	await expect(page.getByTestId('restored-draft-result')).toContainText('Paragraph three, mine.');
	await expect
		.poll(
			async () => ((await (await request.get(`/api/v1/workspaces/${fixture.workspaceSlug}/items/${id}`, { headers })).json()) as { content: string }).content,
			{ timeout: SYNC_TIMEOUT, message: 'the row holds both versions once the editor flushes' },
		)
		.toMatch(/Paragraph one, theirs\.[\s\S]*Paragraph three, mine\./);
	await expect.poll(() => page.evaluate((k) => localStorage.getItem(k), key)).toBeNull();
	await ctx.close();
});

test('both sides changed the same paragraph: nothing is written and the notice shows the diff with Copy', async ({ browser, fixture, request }, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'one project is enough for a restore flow');
	test.setTimeout(120_000);
	const { id, ref, base, key, headers } = await setUp(request, fixture, `TASK-3607 overlap ${Date.now()}`);
	const theirs = base.content.replace('Paragraph three.', 'Paragraph three, theirs.');
	await collaboratorWrites(request, fixture, id, headers, theirs);
	const draft = { markdown: base.content.replace('Paragraph three.', 'Paragraph three, mine.'), savedAt: Date.now(), baseSeq: base.seq, baseBody: base.content };

	const { ctx, page } = await openWithDraft(browser, fixture, ref, key, draft);
	const notice = page.getByTestId('refused-raw-draft');
	await notice.getByRole('button', { name: 'Restore it' }).click();

	await expect(notice.getByTestId('refused-raw-draft-conflict')).toBeVisible();
	await expect(notice).toContainText('Paragraph three, mine.');
	await expect(notice.getByRole('button', { name: 'Restore it' })).toHaveCount(0);
	await expect(notice.getByRole('button', { name: 'Copy it' })).toBeVisible();
	const stored = (await (await request.get(`/api/v1/workspaces/${fixture.workspaceSlug}/items/${id}`, { headers })).json()) as { content: string };
	expect(stored.content).toContain('Paragraph three, theirs.');
	expect(stored.content).not.toContain('mine');
	expect(await page.evaluate((k) => localStorage.getItem(k), key), 'the draft is kept').not.toBeNull();
	await ctx.close();
});

// TASK-3856: a draft whose base is flagged stale (a remote update or this tab's
// own send outran it, or no confirmed snapshot) can never patch, so the notice
// says so up front and offers no Restore.
test('a stale-base draft on a changed item says it cannot be applied and offers only Copy', async ({ browser, fixture, request }, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'one project is enough for a restore flow');
	test.setTimeout(120_000);
	const { id, ref, base, key, headers } = await setUp(request, fixture, `TASK-3856 stale ${Date.now()}`);
	await collaboratorWrites(request, fixture, id, headers, base.content.replace('Paragraph one.', 'Paragraph one, theirs.'));
	const draft = {
		markdown: base.content.replace('Paragraph three.', 'Paragraph three, mine.'),
		savedAt: Date.now(),
		baseSeq: base.seq,
		baseBody: base.content,
		baseStale: true as const,
	};

	const { ctx, page } = await openWithDraft(browser, fixture, ref, key, draft);
	const notice = page.getByTestId('refused-raw-draft');
	await expect(notice.getByTestId('refused-raw-draft-stale')).toContainText("can't be applied by themselves");
	await expect(notice).not.toContainText('restoring applies only your changes');
	await expect(notice.getByRole('button', { name: 'Restore it' })).toHaveCount(0);
	await expect(notice.getByRole('button', { name: 'Copy it' })).toBeVisible();
	const stored = (await (await request.get(`/api/v1/workspaces/${fixture.workspaceSlug}/items/${id}`, { headers })).json()) as { content: string };
	expect(stored.content).toContain('Paragraph one, theirs.');
	expect(stored.content).not.toContain('mine');
	expect(await page.evaluate((k) => localStorage.getItem(k), key), 'the draft is kept').not.toBeNull();
	await ctx.close();
});
