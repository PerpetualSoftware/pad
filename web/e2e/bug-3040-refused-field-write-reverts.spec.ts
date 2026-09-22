import { test, expect } from './fixtures';
import { browserLogin } from './lib/collab-helpers';
import type { APIRequestContext, Page } from '@playwright/test';
import type { SuiteFixture } from './fixtures';

/**
 * BUG-3040 — a refused write must not leave the rejected value in a text, URL
 * or number field.
 *
 * Blur commits a typed edit; if the server refuses it the pane toasts and leaves
 * `item` alone, so the input's `value` prop never changes. The input used to go
 * on showing the rejected text, reading as saved when it was not. The typed path
 * now releases what it is showing when the write SETTLES (BUG-3039's
 * `sendTyped`), not only when the prop agrees — a refused value never agrees.
 *
 * The component-level leg is FieldEditor.typedDebounce.svelte.test.ts, whose
 * consumer is a stub that REJECTS. This is the leg that runs the real consumer:
 * ItemDetail's `updateField` catches the refusal and RESOLVES, so the release
 * here rides a resolved promise, not a rejected one.
 *
 * The refusal is fulfilled by the test rather than provoked from the server. A
 * text or URL value has no server-side check that refuses it, and a number field
 * refuses only what this input cannot send, so there is no honest way to make
 * the real server say no to all three. What is under test is what the client
 * does with a refusal; the body is the server's real `writeError` shape.
 */

const DESKTOP = { width: 1200, height: 900 };

function authHeaders(fixture: SuiteFixture) {
	return { Authorization: `Bearer ${fixture.apiToken}`, 'Content-Type': 'application/json' };
}

const STORED = { note: 'stored note', link: 'https://stored.example/', score: 7 };

async function seed(fixture: SuiteFixture, request: APIRequestContext) {
	const schema = JSON.stringify({
		fields: [
			{ key: 'note', label: 'Note', type: 'text' },
			{ key: 'link', label: 'Link', type: 'url' },
			{ key: 'score', label: 'Score', type: 'number' },
		],
	});
	const coll = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/collections`, {
		headers: authHeaders(fixture),
		data: { name: `B3040 refused ${Date.now()}`, prefix: 'BRF', schema },
	});
	expect(coll.ok(), await coll.text()).toBeTruthy();
	const { slug: collSlug } = await coll.json();
	const created = await request.post(
		`/api/v1/workspaces/${fixture.workspaceSlug}/collections/${collSlug}/items`,
		{
			headers: authHeaders(fixture),
			data: { title: `B3040 item ${Date.now()}`, fields: JSON.stringify(STORED), content: '' },
		},
	);
	expect(created.ok(), await created.text()).toBeTruthy();
	const item = (await created.json()) as { id: string; slug: string };
	return { collSlug, item };
}

function fieldInput(page: Page, label: string) {
	return page
		.locator('.item-page-host > .item-page')
		.locator(`.field-row:has(.field-label:text-is("${label}"))`)
		.locator('input')
		.first();
}

const CASES = [
	{ label: 'Note', key: 'note', typed: 'rejected note', shown: STORED.note },
	{ label: 'Link', key: 'link', typed: 'https://rejected.example/', shown: STORED.link },
	{ label: 'Score', key: 'score', typed: '42', shown: String(STORED.score) },
] as const;

// One test per field type rather than a loop, so a regression in one input
// kind is reported on its own and each leg has been shown able to go red.
for (const c of CASES) test(`BUG-3040: a refused ${c.label} write puts the stored value back on screen`, async ({
	page,
	fixture,
	request,
}, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'one browser is enough for a client-side release');

	await page.setViewportSize(DESKTOP);
	await browserLogin(page);
	const { collSlug, item } = await seed(fixture, request);

	// Refuse every field PATCH to this item. Reads and every other request pass.
	let refused = 0;
	await page.route(`**/api/v1/workspaces/*/items/${item.id}`, (route) => {
		if (route.request().method() !== 'PATCH') return route.fallback();
		refused++;
		return route.fulfill({
			status: 400,
			contentType: 'application/json',
			body: JSON.stringify({ error: { code: 'validation_error', message: 'refused by test (BUG-3040)' } }),
		});
	});

	await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/${collSlug}/${item.slug}`);

	const input = fieldInput(page, c.label);
	await expect(input, `${c.label}: seeded value`).toHaveValue(c.shown);

	await input.fill(c.typed);
	// PREMISE: the edit is on screen before the commit. Without this, a fill
	// that never landed would satisfy the revert assertion below.
	await expect(input, `${c.label}: typed value on screen`).toHaveValue(c.typed);

	const before = refused;
	const patched = page.waitForResponse(
		(r) => r.url().includes(`/items/${item.id}`) && r.request().method() === 'PATCH',
	);
	await input.blur();
	const res = await patched;
	// PREMISE: the write was sent and refused, for THIS field.
	expect(res.status(), `${c.label}: the write was not refused`).toBe(400);
	expect(refused, `${c.label}: exactly one refused write`).toBe(before + 1);
	const body = JSON.parse(res.request().postData() ?? '{}');
	expect(Object.keys(body.fields_patch ?? {}), `${c.label}: patch names the field`).toEqual([c.key]);

	// The defect: the rejected value stays on screen, reading as saved.
	await expect(input, `BUG-3040: ${c.label} still shows the refused value`).toHaveValue(c.shown);

	// And the server really does still hold the stored values — the screen is
	// showing server truth, not a coincidence.
	const after = await (
		await request.get(`/api/v1/workspaces/${fixture.workspaceSlug}/items/${item.id}`, {
			headers: authHeaders(fixture),
		})
	).json();
	expect(JSON.parse(after.fields)).toMatchObject(STORED);
});
