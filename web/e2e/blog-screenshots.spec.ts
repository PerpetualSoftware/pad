import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { mkdir } from 'node:fs/promises';
import { test } from './fixtures';
import { seedRealisticContent, seedConventions, type ConventionInput } from './lib/demo-seed';

/**
 * Blog post screenshot capture.
 *
 * Gated on PAD_BLOG_SCREENSHOTS=1 — runs only when explicitly requested,
 * never as part of the normal e2e suite. Each describe block owns one
 * blog post: it seeds whatever content the post's screenshots need, then
 * captures into ../../pad-web/static/blog/<slug>/ so the marketing site
 * picks them up at build time.
 *
 * Re-run via:
 *   make build-go && cd web && PAD_BLOG_SCREENSHOTS=1 \
 *     npx playwright test blog-screenshots --project=desktop-chromium
 *
 * Conventions for adding a new post's shots:
 *   1. Add a describe block keyed by the post's slug.
 *   2. test.beforeAll seeds the post-specific content.
 *   3. Capture each view with captureView(), naming files NN-shortname.png.
 *   4. Reference in the post body as ![alt](/blog/<slug>/NN-shortname.png).
 *
 * Theme: dark (matches getpad.dev's marketing site). Viewport 1440x900.
 */

const HERE = dirname(fileURLToPath(import.meta.url));
const PAD_REPO_ROOT = resolve(HERE, '..', '..');
// pad-web is a sibling of the pad repo (per CLAUDE.md / CONVE-159).
const PAD_WEB_STATIC_BLOG = resolve(PAD_REPO_ROOT, '..', 'pad-web', 'static', 'blog');

const enabled = process.env.PAD_BLOG_SCREENSHOTS === '1';

test.describe.configure({ mode: 'serial' });

// ─── BLOG-1007: Conventions and Playbooks ────────────────────────────────────

test.describe('BLOG-1007 conventions-and-playbooks', () => {
	test.skip(!enabled, 'set PAD_BLOG_SCREENSHOTS=1 to regenerate blog screenshots');

	test.use({ viewport: { width: 1440, height: 900 }, colorScheme: 'dark' });

	const POST_SLUG = 'conventions-and-playbooks';
	const OUT_DIR = resolve(PAD_WEB_STATIC_BLOG, POST_SLUG);

	test.beforeAll(async () => {
		await mkdir(OUT_DIR, { recursive: true });
	});

	test('seed + capture', async ({ page, fixture, request }) => {
		// Seed (8 items + 5 conventions) + two screenshots is more than 30s.
		test.setTimeout(120_000);

		// Realistic background content so the sidebar / dashboard look lived-in.
		await seedRealisticContent(fixture, request);

		// 4 conventions from BLOG-1007's "five worth stealing" section.
		// The 5th example — "Conventional commit format" — already ships
		// in the startup template's starter pack, so seeding it again
		// would create a near-duplicate ("Use conventional commit format"
		// vs "Conventional commit format") in the screenshot. Letting
		// the template's version stand alone keeps the page tidy and
		// reinforces the post's point that the Library + starter pack
		// already cover the common stuff.
		const POST_CONVENTIONS: ConventionInput[] = [
			{
				title: 'Run make install after backend changes',
				trigger: 'on-implement',
				priority: 'must',
				scope: 'backend'
			},
			{
				title: 'Tasks should be PR-sized',
				trigger: 'always',
				priority: 'should'
			},
			{
				title: 'Never wipe data without confirmation',
				trigger: 'always',
				priority: 'must'
			},
			{
				title: 'Document related repos in CLAUDE.md',
				trigger: 'always',
				priority: 'should'
			}
		];
		await seedConventions(fixture, request, POST_CONVENTIONS);

		const wsPath = `/${fixture.adminUsername}/${fixture.workspaceSlug}`;

		const captureView = async (path: string, outFile: string, anchorSelector: string) => {
			await page.goto(path);
			await page.waitForLoadState('domcontentloaded');
			await page.waitForSelector(anchorSelector, { state: 'visible', timeout: 15_000 });
			// Settle delay covers SSE-driven re-renders.
			await page.waitForTimeout(800);
			await page.screenshot({ path: resolve(OUT_DIR, outFile), fullPage: false });
		};

		// 1. Conventions board — shows the 5 seeded conventions grouped by trigger.
		await captureView(`${wsPath}/conventions`, '01-conventions-board.png', 'h1, h2');

		// 2. Library — conventions tab. No activations yet, so the screenshot
		//    shows the catalog in its "ready to install" state, which is
		//    what a new visitor sees.
		await captureView(
			`${wsPath}/library?tab=conventions`,
			'02-library-conventions.png',
			'h1, h2'
		);
	});
});

// ─── BLOG-1704: Pad v0.7 release roundup ─────────────────────────────────────

test.describe('BLOG-1704 pad-v0-7', () => {
	test.skip(!enabled, 'set PAD_BLOG_SCREENSHOTS=1 to regenerate blog screenshots');

	// 2x device scale → ~2880px-wide source PNG, the high-res input
	// optimize-cover.mjs wants for cover.webp (1600w) + og.png (1200x630).
	test.use({ viewport: { width: 1440, height: 900 }, colorScheme: 'dark', deviceScaleFactor: 2 });

	const POST_SLUG = 'pad-v0-7-share-insights-tags';
	const OUT_DIR = resolve(PAD_WEB_STATIC_BLOG, POST_SLUG);

	test.beforeAll(async () => {
		await mkdir(OUT_DIR, { recursive: true });
	});

	test('seed + capture public shared collection', async ({ page, fixture, request }) => {
		test.setTimeout(120_000);

		// Lived-in tasks collection (varied status/priority/parent linkage)
		// so the public share view has something real to render.
		await seedRealisticContent(fixture, request);

		// Create a public collection share link for `tasks` (the headline
		// v0.7 feature). The response carries the token; the anonymous page
		// lives at /s/<token>.
		const ws = fixture.workspaceSlug;
		const shareResp = await request.post(
			`/api/v1/workspaces/${ws}/collections/tasks/share-links`,
			{ headers: { Authorization: `Bearer ${fixture.apiToken}` }, data: {} }
		);
		if (!shareResp.ok()) {
			throw new Error(`share-link create failed (${shareResp.status()}): ${await shareResp.text()}`);
		}
		const link = (await shareResp.json()) as { token?: string };
		if (!link.token) throw new Error('share-link created but response missing token');

		// The public, unauthenticated page. Strip the Bearer header for this
		// navigation so we capture exactly what a logged-out visitor sees.
		await page.context().setExtraHTTPHeaders({});
		await page.goto(`/s/${link.token}`);
		await page.waitForLoadState('domcontentloaded');
		// Assert the shared collection actually rendered (not an error / auth
		// page) before capturing — a bare `h1, h2` would also match
		// "Unable to load" / "Sign in to view" and save a bad screenshot.
		await page
			.getByRole('heading', { name: 'Tasks' })
			.waitFor({ state: 'visible', timeout: 15_000 });
		await page.waitForTimeout(1000); // settle view render
		await page.screenshot({ path: resolve(OUT_DIR, '01-shared-collection.png'), fullPage: false });
	});
});
