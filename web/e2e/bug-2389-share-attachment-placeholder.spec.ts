import { expect } from '@playwright/test';
import { test, type SuiteFixture } from './fixtures';

/**
 * BUG-2389 — the public share route rendered item content with a bare
 * marked() call, so `pad-attachment:` references became broken <img>
 * tags pointing at an unfetchable pseudo-URL. The route now renders
 * attachment-aware: until the token-scoped byte endpoint ships (2b,
 * with Dave), every reference resolves to the HONEST
 * "not available on shared pages" placeholder — never a broken image,
 * and never the false "missing or deleted" wording.
 *
 * The control build renders `<img src="pad-attachment:...">` verbatim,
 * which is what makes this spec discriminate.
 */

function authHeaders(fixture: SuiteFixture) {
	return { Authorization: `Bearer ${fixture.apiToken}` };
}

test('BUG-2389: a shared item renders attachment refs as honest placeholders, not broken images', async ({
	page,
	request,
	fixture,
}, testInfo) => {
	test.skip(
		testInfo.project.name !== 'desktop-chromium',
		'markdown rendering is viewport-agnostic; one project is enough',
	);

	const uniq = `${test.info().workerIndex}-${Date.now().toString(36)}`;
	// A real uploaded attachment, referenced from item content — the
	// honest construction: the attachment EXISTS, the share surface just
	// cannot serve its bytes.
	const boundary = '----b2389';
	const png = Buffer.from(
		'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==',
		'base64',
	);
	const uploadResp = await request.post(
		`/api/v1/workspaces/${fixture.workspaceSlug}/attachments`,
		{
			headers: { ...authHeaders(fixture), 'Content-Type': `multipart/form-data; boundary=${boundary}` },
			data: Buffer.concat([
				Buffer.from(
					`--${boundary}\r\nContent-Disposition: form-data; name="file"; filename="share-probe.png"\r\nContent-Type: image/png\r\n\r\n`,
				),
				png,
				Buffer.from(`\r\n--${boundary}--\r\n`),
			]),
		},
	);
	expect(uploadResp.ok(), await uploadResp.text()).toBeTruthy();
	const att = (await uploadResp.json()) as { id: string };

	const itemResp = await request.post(
		`/api/v1/workspaces/${fixture.workspaceSlug}/collections/tasks/items`,
		{
			headers: authHeaders(fixture),
			data: {
				title: `b2389 share probe ${uniq}`,
				content: `An embedded image:\n\n![share diagram](pad-attachment:${att.id})\n\nAnd a [file link](pad-attachment:${att.id}).`,
			},
		},
	);
	expect(itemResp.ok(), await itemResp.text()).toBeTruthy();
	const item = (await itemResp.json()) as { slug: string };

	const shareResp = await request.post(
		`/api/v1/workspaces/${fixture.workspaceSlug}/items/${item.slug}/share-links`,
		{ headers: authHeaders(fixture), data: {} },
	);
	expect(shareResp.ok(), await shareResp.text()).toBeTruthy();
	const link = (await shareResp.json()) as { token: string };
	expect(link.token).toBeTruthy();

	// Anonymous context — the share page must not depend on our auth.
	const anon = await page.context().browser()!.newContext();
	const anonPage = await anon.newPage();
	try {
		await anonPage.goto(`${fixture.baseURL}/s/${link.token}`);
		await expect(anonPage.getByText(/b2389 share probe/)).toBeVisible();

		// Honest placeholders for BOTH reference syntaxes.
		await expect(anonPage.locator('.attachment-unavailable')).toHaveCount(2);
		await expect(anonPage.locator('.attachment-unavailable').first()).toHaveAttribute(
			'title',
			/aren't available on shared pages/,
		);
		// The explanation is VISIBLE text (share pages get read on touch
		// devices where the title tooltip doesn't exist).
		await expect(
			anonPage.locator('.attachment-unavailable-note').first(),
		).toHaveText(/not available on shared pages/);
		// Never a broken image aimed at the pseudo-URL, and never the
		// false "missing or deleted" wording.
		expect(await anonPage.locator('img[src^="pad-attachment:"]').count()).toBe(0);
		expect(await anonPage.getByTitle(/missing or has been deleted/).count()).toBe(0);
	} finally {
		await anon.close();
	}
});
