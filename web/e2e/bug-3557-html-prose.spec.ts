import { test, expect } from './fixtures';
import type { APIRequestContext } from '@playwright/test';
import { browserLogin, expectEditorMounted, EDITOR_SELECTOR, SYNCED_BADGE_SELECTOR } from './lib/collab-helpers';

/**
 * BUG-3557: angle-bracketed prose survives the editor, end to end against the
 * embedded bundle and the real server.
 *
 * Before the fix (measured on main 28872c241):
 *  - an agent's write to an item a person merely had OPEN answered 200
 *    applied_pending_flush, and the stored body lost `<Button>` / `<T>` with
 *    nobody typing;
 *  - one keystroke anywhere deleted `<Table>`, `<Button>`, `Promise<T>`,
 *    `List<T>`, HTML comments and `<details>` tags, and split a paragraph at
 *    `<div>`.
 *
 * Each leg has a workspace of its own (BUG-3553: writes into the shared
 * workspace reflow other specs' boards).
 */

const FLUSH_WAIT = 8000; // past the collab flush debounce

/** [label, agent-written body, fragments that must survive verbatim] */
const CASES: Array<[string, string, string[]]> = [
	['a capitalised tag in prose', 'Make @ui <Table> a drop-in for every list.', ['<Table>']],
	['a component name', 'Use the <Button> component here.', ['<Button>']],
	['a generic return type', 'The function returns Promise<T> on success.', ['Promise<T>']],
	['a generic collection', 'Store them in a List<T> first.', ['List<T>']],
	['a two-parameter generic', 'Index by Map<K, V> keys.', ['Map<K, V>']],
	['an HTML comment', 'Before <!-- reviewer note --> after.', ['<!-- reviewer note -->']],
	['a details block', '<details><summary>More</summary>hidden text</details>\n\nafter', ['<details><summary>More</summary>hidden text</details>']],
	['a lower-case block tag in prose', 'Wrap it in a <div> element.', ['Wrap it in a <div> element.']],
	['a line break tag', 'line one<br>line two', ['line one', 'line two']],
];

async function setup(request: APIRequestContext, fixture: import('./fixtures').SuiteFixture, label: string, content: string) {
	const h = { Authorization: `Bearer ${fixture.apiToken}`, 'Content-Type': 'application/json' };
	const ws = await request.post('/api/v1/workspaces', { headers: h, data: { name: `BUG-3557 ${label} ${Date.now()}`, template: 'startup' } });
	expect(ws.ok(), await ws.text()).toBe(true);
	const { slug: wsSlug } = (await ws.json()) as { slug: string };
	const created = await request.post(`/api/v1/workspaces/${wsSlug}/collections/docs/items`, {
		headers: h,
		data: { title: `BUG-3557 ${label}`, fields: '{}', content },
	});
	expect(created.ok(), await created.text()).toBe(true);
	const { slug } = (await created.json()) as { slug: string };
	const read = async () =>
		((await (await request.get(`/api/v1/workspaces/${wsSlug}/items/${slug}`, { headers: h })).json()) as { content: string }).content;
	return { h, wsSlug, slug, read };
}

test.describe('BUG-3557: angle-bracketed prose survives the editor', () => {
	test.beforeEach(({}, info) => {
		test.skip(info.project.name !== 'desktop-chromium', 'the editor path is the same on mobile; one browser is enough');
	});

	for (const [label, body, keep] of CASES) {
		test(`a keystroke keeps it: ${label}`, async ({ page, fixture, request }) => {
			test.setTimeout(60_000);
			const { wsSlug, slug, read } = await setup(request, fixture, label, body);
			await browserLogin(page);
			await page.goto(`/${fixture.adminUsername}/${wsSlug}/docs/${slug}`);
			await expectEditorMounted(page);
			await expect(page.locator(SYNCED_BADGE_SELECTOR)).toBeVisible({ timeout: 20_000 });
			await page.locator(EDITOR_SELECTOR).click();
			await page.keyboard.press('Control+End');
			await page.keyboard.type('Z');
			await expect.poll(read, { timeout: 20_000 }).toContain('Z');
			const stored = await read();
			for (const fragment of keep) expect(stored, `stored body keeps ${fragment}`).toContain(fragment);
		});
	}

	test('an agent write to an item someone has open keeps it, with nobody typing', async ({ page, fixture, request }) => {
		test.setTimeout(60_000);
		const { h, wsSlug, slug, read } = await setup(request, fixture, 'applier', 'plain start');
		await browserLogin(page);
		await page.goto(`/${fixture.adminUsername}/${wsSlug}/docs/${slug}`);
		await expectEditorMounted(page);
		await expect(page.locator(SYNCED_BADGE_SELECTOR)).toBeVisible({ timeout: 20_000 });
		const body = 'Use the <Button> component; it returns Promise<T>.';
		// A tab still confirming its first sync refuses an external write
		// (BUG-3542, unconfirmed_edits) and says to resend: the contract, so
		// resend until it lands.
		await expect
			.poll(
				async () => {
					const patch = await request.patch(`/api/v1/workspaces/${wsSlug}/items/${slug}`, { headers: h, data: { content: body } });
					if (patch.ok()) return 'applied';
					const err = (await patch.json()) as { error?: { details?: { apply_reason?: string } } };
					return err.error?.details?.apply_reason ?? `status ${patch.status()}`;
				},
				{ timeout: 20_000, intervals: [1000] },
			)
			.toBe('applied');
		await expect.poll(read, { timeout: FLUSH_WAIT * 3 }).not.toBe('plain start');
		await page.waitForTimeout(FLUSH_WAIT); // let any later flush land too
		const stored = await read();
		expect(stored).toContain('<Button>');
		expect(stored).toContain('Promise<T>');
	});
});
