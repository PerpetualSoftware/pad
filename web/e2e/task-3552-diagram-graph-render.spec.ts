import { test, expect } from './fixtures';
import type { APIRequestContext, Page } from '@playwright/test';
import { browserLogin, EDITOR_SELECTOR, expectEditorMounted } from './lib/collab-helpers';

/**
 * TASK-3552: mermaid and the graph view still DRAW on the embedded bundle.
 *
 * Dependabot moves both libraries (mermaid, 3d-force-graph), and nothing else
 * in the suite checks that either still paints after a bump: the mermaid specs
 * pin print colours and the empty placeholder, and there was no graph-view
 * spec at all. Each leg measures through one helper and runs it twice: once
 * where the thing must be drawn, and once where it must NOT be, so the
 * instrument is shown able to go red for the case it exists for (CONVE-34).
 *
 *  - mermaid: a two-node flowchart draws two nodes; invalid source draws none.
 *  - graph: two linked items paint the WebGL canvas; an empty workspace
 *    leaves it a flat background.
 */

const DIAGRAM = '.mermaid-wrapper .mermaid-diagram';

function apiHeaders(fixture: import('./fixtures').SuiteFixture) {
	return { Authorization: `Bearer ${fixture.apiToken}`, 'Content-Type': 'application/json' };
}

/**
 * A workspace of this spec's own. Every leg writes only here, never into the
 * suite's shared workspace: docs created there reflow the shared docs board
 * under other specs' clicks (BUG-3553).
 */
async function ownWorkspace(
	request: APIRequestContext,
	fixture: import('./fixtures').SuiteFixture,
	label: string,
	template: 'startup' | 'blank',
): Promise<string> {
	const resp = await request.post('/api/v1/workspaces', {
		headers: apiHeaders(fixture),
		data: { name: `TASK-3552 ${label} ${Date.now()}`, template },
	});
	expect(resp.ok(), await resp.text()).toBe(true);
	return ((await resp.json()) as { slug: string }).slug;
}

async function createDoc(
	request: APIRequestContext,
	fixture: import('./fixtures').SuiteFixture,
	ws: string,
	title: string,
	content = '',
): Promise<{ slug: string; id: string }> {
	const resp = await request.post(`/api/v1/workspaces/${ws}/collections/docs/items`, {
		headers: apiHeaders(fixture),
		data: { title, fields: '{}', content },
	});
	expect(resp.ok(), await resp.text()).toBe(true);
	return (await resp.json()) as { slug: string; id: string };
}

/**
 * Opens a doc holding one mermaid block and waits for the render to SETTLE
 * (an svg, or the failed-render marker), then reports what was drawn.
 */
async function mermaidDrawn(
	page: Page,
	request: APIRequestContext,
	fixture: import('./fixtures').SuiteFixture,
	source: string,
): Promise<{ nodes: number; failed: boolean; text: string }> {
	const ws = await ownWorkspace(request, fixture, 'mermaid', 'startup');
	const { slug } = await createDoc(
		request,
		fixture,
		ws,
		`TASK-3552 mermaid ${Date.now()}`,
		`Diagram:\n\n\`\`\`mermaid\n${source}\n\`\`\`\n\nAfter.\n`,
	);
	await page.goto(`/${fixture.adminUsername}/${ws}/docs/${slug}`);
	await expectEditorMounted(page);
	const diagram = page.locator(`${EDITOR_SELECTOR} ${DIAGRAM}`).first();
	await expect(diagram.locator('svg').or(page.locator(`${EDITOR_SELECTOR} .mermaid-error`)).first()).toBeVisible({
		timeout: 20_000,
	});
	return {
		nodes: await diagram.locator('svg g.node').count(),
		failed: (await page.locator(`${EDITOR_SELECTOR} .mermaid-error`).count()) > 0,
		text: (await diagram.textContent()) ?? '',
	};
}

/**
 * Opens a workspace's graph view and reports whether the WebGL canvas holds a
 * drawing: the number of distinct colours sampled from its screenshot. A
 * canvas with nothing drawn is one flat background colour.
 */
async function graphDrawn(
	page: Page,
	fixture: import('./fixtures').SuiteFixture,
	ws: string,
): Promise<{ colours: number; width: number; loadError: boolean; emptyState: boolean; pageErrors: string[] }> {
	const pageErrors: string[] = [];
	page.on('pageerror', (e) => pageErrors.push(e.message));
	await page.goto(`/${fixture.adminUsername}/${ws}/graph`);
	const canvas = page.locator('.graph-page .canvas canvas');
	await expect(canvas).toBeVisible({ timeout: 20_000 });
	await expect(page.getByText('Loading graph')).toHaveCount(0, { timeout: 20_000 });
	// The force layout animates in; give it a moment to place the nodes.
	await page.waitForTimeout(1500);
	const box = await canvas.boundingBox();
	const loadError = (await page.getByText("Couldn't load the graph").count()) > 0;
	const emptyState = await page.getByText('No active items to map').isVisible();
	// An element screenshot captures whatever is painted over the canvas too:
	// the page's toolbar and state cards, and the renderer's own navigation
	// hint, whose anti-aliased text alone is ~120 colours (it made an EMPTY
	// graph pass a "drawn" check). Hide all of it and measure only the WebGL
	// output.
	await page.addStyleTag({
		content: [
			'.graph-page > :not(.canvas) { visibility: hidden !important; }',
			'.graph-page .canvas * { visibility: hidden !important; }',
			'.graph-page .canvas canvas { visibility: visible !important; }',
		].join('\n'),
	});
	const png = await canvas.screenshot();
	const colours = await page.evaluate(async (b64) => {
		const img = new Image();
		img.src = `data:image/png;base64,${b64}`;
		await img.decode();
		const c = document.createElement('canvas');
		c.width = img.width;
		c.height = img.height;
		const ctx = c.getContext('2d')!;
		ctx.drawImage(img, 0, 0);
		const d = ctx.getImageData(0, 0, c.width, c.height).data;
		const seen = new Set<number>();
		for (let i = 0; i < d.length; i += 16) seen.add((d[i] << 16) | (d[i + 1] << 8) | d[i + 2]);
		return seen.size;
	}, png.toString('base64'));
	return {
		colours,
		width: box?.width ?? 0,
		loadError,
		emptyState,
		pageErrors,
	};
}

test.describe('TASK-3552: mermaid diagrams draw', () => {
	test('a two-node flowchart draws both nodes', async ({ page, fixture, request }) => {
		await browserLogin(page);
		const drawn = await mermaidDrawn(page, request, fixture, 'graph TD; Alpha-->Bravo');
		expect(drawn.failed, 'the render did not fail').toBe(false);
		expect(drawn.nodes).toBe(2);
		expect(drawn.text).toContain('Bravo');
	});

	test('counterfactual: invalid source draws no node', async ({ page, fixture, request }) => {
		await browserLogin(page);
		const drawn = await mermaidDrawn(page, request, fixture, 'graph TD; Alpha-->');
		expect(drawn.failed).toBe(true);
		expect(drawn.nodes).toBe(0);
	});
});

test.describe('TASK-3552: the graph view draws', () => {
	test('two linked items paint the canvas', async ({ page, fixture, request }) => {
		const ws = await ownWorkspace(request, fixture, 'graph', 'startup');
		const a = await createDoc(request, fixture, ws, `TASK-3552 graph a ${Date.now()}`);
		const b = await createDoc(request, fixture, ws, `TASK-3552 graph b ${Date.now()}`);
		const link = await request.post(`/api/v1/workspaces/${ws}/items/${a.slug}/links`, {
			headers: apiHeaders(fixture),
			data: { target_id: b.id, link_type: 'related' },
		});
		expect(link.ok(), await link.text()).toBe(true);
		await browserLogin(page);
		const drawn = await graphDrawn(page, fixture, ws);
		expect(drawn.loadError).toBe(false);
		expect(drawn.pageErrors).toEqual([]);
		expect(drawn.width).toBeGreaterThan(100);
		expect(drawn.emptyState).toBe(false);
		// Receipt (own workspaces, 3 runs each per project, day 90): drawn
		// 198-225 colours on desktop and 1134-1146 on mobile; empty 1 on desktop
		// and 2 on mobile. The bound sits above both empty readings and far
		// below every drawn one.
		expect(drawn.colours, 'distinct colours painted on the graph canvas').toBeGreaterThan(3);
	});

	test('counterfactual: an empty workspace leaves the canvas flat', async ({ page, fixture, request }) => {
		const slug = await ownWorkspace(request, fixture, 'empty', 'blank');
		// Even the blank template seeds the onboard playbook, which the graph
		// maps as a node: delete what was seeded so the workspace is empty.
		const list = await request.get(`/api/v1/workspaces/${slug}/items`, { headers: apiHeaders(fixture) });
		expect(list.ok(), await list.text()).toBe(true);
		for (const item of (await list.json()) as { slug: string }[]) {
			const del = await request.delete(`/api/v1/workspaces/${slug}/items/${item.slug}`, { headers: apiHeaders(fixture) });
			expect(del.ok(), await del.text()).toBe(true);
		}
		await browserLogin(page);
		const drawn = await graphDrawn(page, fixture, slug);
		expect(drawn.emptyState, 'the workspace really is empty').toBe(true);
		expect(drawn.colours, 'distinct colours on an undrawn canvas').toBeLessThanOrEqual(3);
	});
});
