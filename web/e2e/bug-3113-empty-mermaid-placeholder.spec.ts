import { test, expect } from './fixtures';
import { browserLogin, EDITOR_SELECTOR, SYNCED_BADGE_SELECTOR, expectEditorMounted } from './lib/collab-helpers';

/**
 * BUG-3113: an empty mermaid block showed "Rendering..." forever.
 *
 * The mermaid NodeView seeded its diagram element with a "Rendering..."
 * placeholder and queued a render only when the source was non-empty, so a
 * block that was EMPTY WHEN ITS VIEW WAS BUILT kept the placeholder with
 * nothing coming to replace it. Both ways a block starts empty are covered —
 * loading an item that already holds one, and typing the fence — per the
 * contract on BUG-3113.
 *
 * The control leg matters (CONVE-12): "the placeholder is absent" would also
 * hold if mermaid rendering were broken outright and the diagram element were
 * left empty for every block. So the same item carries a block WITH source,
 * which must render an SVG, and the empty-block assertions are only reached
 * once that has happened.
 */

const DIAGRAM = '.mermaid-wrapper .mermaid-diagram';

async function openDoc(
	page: import('@playwright/test').Page,
	fixture: import('./fixtures').SuiteFixture,
	request: import('@playwright/test').APIRequestContext,
	content: string,
) {
	const ws = fixture.workspaceSlug;
	const resp = await request.post(`/api/v1/workspaces/${ws}/collections/docs/items`, {
		headers: { Authorization: `Bearer ${fixture.apiToken}`, 'Content-Type': 'application/json' },
		data: { title: `BUG-3113 ${Date.now()}`, fields: '{}', content },
	});
	expect(resp.ok()).toBe(true);
	const { slug } = (await resp.json()) as { slug: string };
	await browserLogin(page);
	await page.goto(`/${fixture.adminUsername}/${ws}/docs/${slug}`);
	await expectEditorMounted(page);
	await expect(page.locator(SYNCED_BADGE_SELECTOR)).toBeVisible();
}

test.describe('BUG-3113: an empty mermaid block is not stuck on the placeholder', () => {
	test.beforeEach(({}, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'editor rendering; one viewport is enough');
	});

	test('loading an item that already holds an empty mermaid block', async ({ page, fixture, request }) => {
		await openDoc(
			page,
			fixture,
			request,
			'Control:\n\n```mermaid\ngraph TD; A-->B\n```\n\nEmpty:\n\n```mermaid\n```\n\nAfter.\n',
		);
		const diagrams = page.locator(`${EDITOR_SELECTOR} ${DIAGRAM}`);
		await expect(diagrams).toHaveCount(2);

		// CONTROL: the block with source renders, so rendering works here.
		await expect(diagrams.nth(0).locator('svg')).toBeVisible({ timeout: 15_000 });

		// THE ASSERTION: the empty block shows nothing, not a placeholder that
		// claims work is in progress.
		await expect(diagrams.nth(1)).not.toContainText('Rendering');
		await expect(diagrams.nth(1)).toHaveText('');
	});

	test('typing the fence into the editor', async ({ page, fixture, request }) => {
		await openDoc(page, fixture, request, 'Control:\n\n```mermaid\ngraph TD; A-->B\n```\n\nType below.\n');
		const diagrams = page.locator(`${EDITOR_SELECTOR} ${DIAGRAM}`);
		await expect(diagrams).toHaveCount(1);
		await expect(diagrams.nth(0).locator('svg')).toBeVisible({ timeout: 15_000 });

		// Put the caret at the end of the last paragraph, start a new one, and
		// type the fence the way a user starts a diagram.
		await page.locator(`${EDITOR_SELECTOR} p`, { hasText: 'Type below.' }).click();
		await page.keyboard.press('End');
		await page.keyboard.press('Enter');
		await page.keyboard.type('```mermaid');
		await page.keyboard.press('Enter');

		// Precondition: the fence really produced a SECOND mermaid block.
		await expect(diagrams).toHaveCount(2);
		await expect(diagrams.nth(1)).not.toContainText('Rendering');
		await expect(diagrams.nth(1)).toHaveText('');
	});
});
