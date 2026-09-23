import { test, expect } from './fixtures';
import { browserLogin } from './lib/collab-helpers';

/**
 * BUG-3163 — create's `fields` refuses every reserved metadata key, so both web
 * convention writers send the metadata as the typed `convention` create member.
 * The unit tests vouch for the request shapes; these legs vouch for the pages
 * calling them against the real server: a writer left on the old shape would
 * be answered 400, and the stored `convention` key is the only witness that
 * the typed member landed (the response's derived `convention` object is
 * non-nil either way, built from the sibling trigger/scope keys).
 */

const DESKTOP = { width: 1200, height: 900 };

function storedConvention(body: { fields?: string }): unknown {
	const fields = JSON.parse(body.fields ?? '{}') as Record<string, unknown>;
	return fields.convention;
}

test.describe('BUG-3163: web convention writers', () => {
	test.beforeEach(async ({ page }, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'one browser is enough for a request-shape rule');
		await page.setViewportSize(DESKTOP);
		await browserLogin(page);
	});

	test('the Conventions page create stores convention metadata', async ({ page, fixture }) => {
		const title = `B3163 page ${Date.now()}`;
		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/conventions`);
		await page.getByRole('button', { name: '+ New Convention' }).click();
		await page.getByPlaceholder('Convention title...').fill(title);
		await page.getByPlaceholder('Instruction the agent should follow...').fill('Run the tests.');

		const write = page.waitForResponse(
			(r) => r.request().method() === 'POST' && /\/collections\/conventions\/items$/.test(new URL(r.url()).pathname),
		);
		await page.getByRole('button', { name: 'Create', exact: true }).click();
		const res = await write;

		expect(res.status(), await res.text()).toBe(201);
		const sent = res.request().postDataJSON() as { fields: string; convention?: unknown };
		expect(sent.convention).toBeTruthy();
		expect(JSON.parse(sent.fields)).not.toHaveProperty('convention');
		expect(storedConvention(await res.json())).toMatchObject({ trigger: 'always', enforcement: 'should' });
	});

	test('library activate stores convention metadata', async ({ page, fixture }) => {
		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/library`);
		const card = page.locator('.card').filter({ has: page.locator('button.activate-btn') }).first();
		await expect(card).toBeVisible();
		const title = (await card.locator('.card-title').textContent())?.trim();
		expect(title).toBeTruthy();

		const write = page.waitForResponse(
			(r) => r.request().method() === 'POST' && /\/collections\/conventions\/items$/.test(new URL(r.url()).pathname),
		);
		await card.locator('button.activate-btn').click();
		const res = await write;

		expect(res.status(), await res.text()).toBe(201);
		const sent = res.request().postDataJSON() as { fields: string; convention?: unknown };
		expect(sent.convention).toBeTruthy();
		expect(JSON.parse(sent.fields)).not.toHaveProperty('convention');
		expect(storedConvention(await res.json())).toBeTruthy();
	});
});
