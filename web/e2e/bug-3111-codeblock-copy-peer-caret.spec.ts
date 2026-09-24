import { test, expect, quietCrossActorToasts } from './fixtures';
import { browserLogin, EDITOR_SELECTOR, SYNCED_BADGE_SELECTOR, expectEditorMounted } from './lib/collab-helpers';

/**
 * BUG-3111: a code block's Copy button copied a remote peer's display name.
 *
 * The button read the code element's `textContent`, and that element is the
 * block's contentDOM — where ProseMirror also renders DECORATIONS. A remote
 * collaborator's caret is a widget decoration whose label is a real text node
 * holding their name, so with a peer's caret parked in the block, Copy
 * produced the code with the name spliced in at the caret.
 *
 * This drives the real mechanism end to end rather than a hand-built
 * decoration: two browser contexts on one item, the second parks its caret
 * inside the code block, and the first clicks Copy and reads the clipboard.
 *
 * The precondition matters as much as the assertion (CONVE-12): the clipboard
 * equalling the code proves nothing unless the peer's caret was actually in
 * the block's contentDOM when Copy was clicked. So the spec first asserts the
 * caret label sits inside the `<code>` element AND that the element's own
 * `textContent` carries the name — the exact read the fix removed. Without
 * that leg, a caret that never arrived would make this pass against the bug.
 */

const CODE = 'const answer = 42;';

test('copying a code block excludes a remote peer caret label', async ({
	page,
	fixture,
	request,
	browser,
}, testInfo) => {
	test.skip(
		testInfo.project.name !== 'desktop-chromium',
		'two live collab peers plus a clipboard read; one viewport is enough',
	);
	test.setTimeout(60_000);

	const ws = fixture.workspaceSlug;
	const resp = await request.post(`/api/v1/workspaces/${ws}/collections/docs/items`, {
		headers: { Authorization: `Bearer ${fixture.apiToken}`, 'Content-Type': 'application/json' },
		data: {
			title: `BUG-3111 copy ${Date.now()}`,
			fields: '{}',
			content: 'Before the block.\n\n```js\n' + CODE + '\n```\n\nAfter the block.\n',
		},
	});
	expect(resp.ok()).toBe(true);
	const { slug } = (await resp.json()) as { slug: string };
	const path = `/${fixture.adminUsername}/${ws}/docs/${slug}`;

	// Page A — the one that copies.
	await page.context().grantPermissions(['clipboard-read', 'clipboard-write']);
	await browserLogin(page);
	await page.goto(path);
	await expectEditorMounted(page);
	await expect(page.locator(SYNCED_BADGE_SELECTOR)).toBeVisible();

	// Page B — the peer, in its own context so it is a separate awareness
	// client with its own caret.
	const peerContext = await browser.newContext({ baseURL: fixture.baseURL });
	await quietCrossActorToasts(peerContext);
	const peer = await peerContext.newPage();
	try {
		await browserLogin(peer);
		await peer.goto(path);
		await expect(peer.locator(EDITOR_SELECTOR)).toBeVisible();
		await expect(peer.locator(SYNCED_BADGE_SELECTOR)).toBeVisible();

		// Park the peer's caret INSIDE the code, mid-line, so the label would
		// land between characters rather than at an edge.
		const peerCode = peer.locator(`${EDITOR_SELECTOR} pre.code-block code`);
		await expect(peerCode).toHaveText(CODE);
		const box = await peerCode.boundingBox();
		if (!box) throw new Error('peer code block has no bounding box');
		await peer.mouse.click(box.x + box.width / 2, box.y + box.height / 2);

		// PRECONDITION: the peer's caret label is inside A's contentDOM, and
		// the DOM read the bug used would include it.
		const code = page.locator(`${EDITOR_SELECTOR} pre.code-block code`);
		const label = code.locator('.collaboration-carets__label');
		await expect(label).toHaveCount(1, { timeout: 10_000 });
		const peerName = (await label.textContent()) ?? '';
		expect(peerName.length).toBeGreaterThan(0);
		const domText = await code.evaluate((el) => el.textContent ?? '');
		expect(domText).toContain(peerName);
		expect(domText).not.toBe(CODE);

		// THE ASSERTION: Copy yields exactly the code. Read the CLIPBOARD, not
		// the button's "Copied" flash: the code block's DOM can be rebuilt
		// under a live peer (measured while writing this spec — the button the
		// click landed on was no longer in the document a moment later), so a
		// locator on the button can watch a fresh one that never flashed. A
		// sentinel written first keeps "not copied yet" distinguishable from
		// "copied".
		await page.evaluate(() => navigator.clipboard.writeText('BUG-3111-SENTINEL'));
		await page.locator(`${EDITOR_SELECTOR} pre.code-block`).hover();
		await page.locator(`${EDITOR_SELECTOR} pre.code-block .code-copy-btn`).click();
		await expect
			.poll(() => page.evaluate(() => navigator.clipboard.readText()), { timeout: 5_000 })
			.not.toBe('BUG-3111-SENTINEL');
		const copied = await page.evaluate(() => navigator.clipboard.readText());
		expect(copied).not.toContain(peerName);
		expect(copied).toBe(CODE);
	} finally {
		await peerContext.close();
	}
});
