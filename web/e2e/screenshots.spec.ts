import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { mkdir } from 'node:fs/promises';
import { test } from './fixtures';
import { seedRealisticContent } from './lib/demo-seed';

/**
 * README screenshot capture.
 *
 * Gated on PAD_SCREENSHOTS=1 — runs only when explicitly requested,
 * never as part of the normal e2e suite. The script seeds a small
 * demo dataset (realistic task titles + statuses + an active plan)
 * on top of the per-run workspace, then captures dashboard / board /
 * list / table screenshots into docs/screenshots/ at the repo root.
 *
 * Re-run via:
 *   make build-go && cd web && PAD_SCREENSHOTS=1 npx playwright test screenshots --project=desktop-chromium
 *
 * Theme: captured in DARK mode (Pad's default when no user preference is
 * set). Playwright's headless Chromium reports prefers-color-scheme: light
 * by default, which would otherwise make the layout's onMount set
 * data-theme="light" — producing light-mode screenshots that don't match
 * what most users see and that clash with getpad.dev's dark marketing site.
 * `test.use({ colorScheme: 'dark' })` flips this so the layout falls through
 * to its default (dark) branch.
 *
 * Output:
 *   docs/screenshots/dashboard.png
 *   docs/screenshots/board.png
 *   docs/screenshots/list.png
 *   docs/screenshots/table.png
 */

const HERE = dirname(fileURLToPath(import.meta.url));
const REPO_ROOT = resolve(HERE, '..', '..');
const OUT_DIR = resolve(REPO_ROOT, 'docs', 'screenshots');

// Skip the entire describe block when PAD_SCREENSHOTS isn't set so a
// regular `npx playwright test` doesn't accidentally regenerate screenshots
// or fail because the OUT_DIR write isn't permitted in some environment.
const enabled = process.env.PAD_SCREENSHOTS === '1';

test.describe.configure({ mode: 'serial' });

test.describe('README screenshots', () => {
	test.skip(!enabled, 'set PAD_SCREENSHOTS=1 to regenerate screenshots');

	// 1440x900 ≈ "Macbook Air at default zoom" — the most common desktop
	// viewport for product screenshots. Wide enough that the board view
	// shows multiple columns side-by-side without horizontal scroll.
	//
	// colorScheme: 'dark' makes Chromium report prefers-color-scheme: dark.
	// The Pad layout's onMount only forces data-theme="light" when matchMedia
	// reports 'light'; with dark emulation, it leaves the document on the
	// default theme — which renders dark.
	test.use({ viewport: { width: 1440, height: 900 }, colorScheme: 'dark' });

	test.beforeAll(async () => {
		await mkdir(OUT_DIR, { recursive: true });
	});

	test('seed + capture', async ({ page, fixture, request }) => {
		// Seed + four screenshots is more than the suite's default 30s.
		// `networkidle` would never trigger anyway because the dashboard
		// holds an open SSE connection — we use targeted waits below.
		test.setTimeout(120_000);

		await seedRealisticContent(fixture, request);

		const wsPath = `/${fixture.adminUsername}/${fixture.workspaceSlug}`;

		const captureView = async (
			path: string,
			outFile: string,
			anchorSelector: string
		) => {
			await page.goto(path);
			// DOMContentLoaded fires once the SvelteKit hydration scripts
			// are parsed but before the SSE long-lived connection opens.
			await page.waitForLoadState('domcontentloaded');
			// Wait for a content-meaningful element to appear so we capture
			// a populated view rather than the empty skeleton.
			await page.waitForSelector(anchorSelector, { state: 'visible', timeout: 15_000 });
			// Settle delay covers SSE-driven re-renders + animations
			// (progress bars, drag handles) that happen post-paint.
			await page.waitForTimeout(800);
			await page.screenshot({ path: resolve(OUT_DIR, outFile), fullPage: false });
		};

		await captureView(wsPath, 'dashboard.png', 'h1, h2');
		await captureView(`${wsPath}/tasks?view=board`, 'board.png', 'h1, h2');
		await captureView(`${wsPath}/tasks?view=list`, 'list.png', 'h1, h2');
		// Table view is not URL-reachable in the current code (only 'list'
		// and 'board' are parsed from search params; 'table' is only set
		// via the toggle UI + localStorage). Skip it for now — three
		// screenshots already cover the README's hero use cases.
	});
});

// seedRealisticContent now lives in ./lib/demo-seed.ts and is shared with
// blog-screenshots.spec.ts.
