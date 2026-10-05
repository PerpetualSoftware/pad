import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { mkdir } from 'node:fs/promises';
import { test } from './fixtures';
import { seedReadmeShowcase } from './lib/readme-seed';

/**
 * README / getpad.dev product screenshots (TASK-3426).
 *
 * Gated on PAD_SCREENSHOTS=1: runs only when explicitly requested, never as
 * part of the normal e2e suite. It seeds the e2e fixture's own throwaway
 * workspace with ./lib/readme-seed.ts (never a real workspace, CONVE-15),
 * then captures each view in dark and light at 2x device pixel ratio.
 *
 * Re-run against a FRESH data dir, so earlier runs' items don't pile up:
 *   make build-go && cd web && \
 *     PAD_SCREENSHOTS=1 PAD_BINARY=../pad PAD_E2E_DATA_DIR=$(mktemp -d) \
 *     npx playwright test screenshots --project=desktop-chromium
 *
 * Then shrink them (about 60% smaller, no visible change at these sizes):
 *   cd docs/screenshots && for f in *.png; do \
 *     pngquant --quality=85-98 --speed 1 --force --output "$f" "$f"; done
 *
 * Theme: the layout's onMount forces data-theme="light" only when
 * matchMedia reports light, so each shot emulates the colour scheme before
 * navigating. Dark is Pad's default and getpad.dev's palette.
 *
 * Output (docs/screenshots/, 2880x1800 pixels each):
 *   dashboard.png      dashboard-light.png
 *   board.png          board-light.png
 *   list.png           list-light.png
 *   item.png           item-light.png
 */

const HERE = dirname(fileURLToPath(import.meta.url));
const REPO_ROOT = resolve(HERE, '..', '..');
const OUT_DIR = resolve(REPO_ROOT, 'docs', 'screenshots');

const enabled = process.env.PAD_SCREENSHOTS === '1';

// The sidebar footer shows the build's version label, which on a local build
// reads "dev (<commit>)". It is not product UI anyone would see on a release.
const HIDE_BUILD_LABEL = '.version-label { visibility: hidden !important; }';

test.describe.configure({ mode: 'serial' });

test.describe('README screenshots', () => {
	test.skip(!enabled, 'set PAD_SCREENSHOTS=1 to regenerate screenshots');

	// 1440x900 CSS pixels at 2x: a MacBook-sized layout, crisp on retina and
	// when getpad.dev scales it down to ~900px wide.
	test.use({ viewport: { width: 1440, height: 900 }, deviceScaleFactor: 2 });

	test.beforeAll(async () => {
		await mkdir(OUT_DIR, { recursive: true });
	});

	test('seed + capture', async ({ page, fixture, request }) => {
		// Seeding plus eight captures is well past the default 30s. The pages
		// hold an open SSE connection, so `networkidle` never fires; the waits
		// below are targeted instead.
		test.setTimeout(180_000);

		const seed = await seedReadmeShowcase(fixture, request);
		const wsPath = `/${fixture.adminUsername}/${fixture.workspaceSlug}`;

		const shots: { path: string; name: string; anchor: string }[] = [
			{ path: wsPath, name: 'dashboard', anchor: 'h1' },
			{ path: `${wsPath}/tasks?view=board`, name: 'board', anchor: '.item-card' },
			{ path: `${wsPath}/tasks?view=list`, name: 'list', anchor: '.item-card' },
			{
				// The split view: the list beside the open item reads as the
				// product better than the full item page's stretched form.
				path: `${wsPath}/tasks?view=list&item=${encodeURIComponent(seed.featureTaskSlug)}`,
				name: 'item',
				anchor: '.item-pane'
			}
		];

		for (const scheme of ['dark', 'light'] as const) {
			await page.emulateMedia({ colorScheme: scheme });
			for (const shot of shots) {
				await page.goto(shot.path);
				await page.waitForLoadState('domcontentloaded');
				await page.waitForSelector(shot.anchor, { state: 'visible', timeout: 15_000 });
				await page.addStyleTag({ content: HIDE_BUILD_LABEL });
				// Post-paint settle: SSE-driven re-renders, progress bars, the
				// pane's open transition.
				await page.waitForTimeout(1200);
				const suffix = scheme === 'dark' ? '' : '-light';
				await page.screenshot({ path: resolve(OUT_DIR, `${shot.name}${suffix}.png`) });
			}
		}
	});
});
