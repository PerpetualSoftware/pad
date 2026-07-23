import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { mkdir } from 'node:fs/promises';
import { test } from './fixtures';
import { seedRealisticContent, seedConventions, type ConventionInput } from './lib/demo-seed';
import { browserLogin } from './lib/collab-helpers';

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

// ─── BLOG-2289: Pad v0.11 docked detail pane (item mini-browser) ─────────────

test.describe('BLOG-2289 pad-v0-11', () => {
	test.skip(!enabled, 'set PAD_BLOG_SCREENSHOTS=1 to regenerate blog screenshots');

	// 2x device scale → high-res source PNG for cover.webp / og.png (as v0.7).
	test.use({ viewport: { width: 1440, height: 900 }, colorScheme: 'dark', deviceScaleFactor: 2 });

	const POST_SLUG = 'pad-v0-11-item-pane';
	const OUT_DIR = resolve(PAD_WEB_STATIC_BLOG, POST_SLUG);

	test.beforeAll(async () => {
		await mkdir(OUT_DIR, { recursive: true });
	});

	test('seed + capture docked detail pane', async ({ page, fixture, request }) => {
		test.setTimeout(120_000);

		// Lived-in tasks collection so the list column behind the pane looks real.
		await seedRealisticContent(fixture, request);

		const ws = fixture.workspaceSlug;
		const headers = {
			Authorization: `Bearer ${fixture.apiToken}`,
			'Content-Type': 'application/json'
		};

		type RefLike = {
			id?: string;
			ref?: string;
			collection_prefix?: string;
			item_number?: number;
			slug?: string;
		};
		// The pane's `?item=` param wants the issue-ID ref (e.g. TASK-7). Tolerate
		// either a computed `ref` or the raw prefix+number pair; fall back to slug.
		const refOf = (it: RefLike): string | undefined =>
			it.ref ||
			(it.collection_prefix && it.item_number
				? `${it.collection_prefix}-${it.item_number}`
				: it.slug);

		// The demo tasks carry no `content`, so the pane would render an empty
		// body. Create one task WITH a real markdown paragraph so the docked
		// detail pane shows a lived-in item, and open THAT.
		const createResp = await request.post(
			`/api/v1/workspaces/${ws}/collections/tasks/items`,
			{
				headers,
				data: {
					title: 'Wire up the docked detail pane',
					fields: JSON.stringify({ status: 'in-progress', priority: 'high', effort: 'm' }),
					content:
						'Open any item beside the list without losing your place. The detail ' +
						'pane docks to the right of the board or list so you can read the full ' +
						'write-up, edit inline, and page through items with j/k while the ' +
						'collection stays put.\n\n' +
						'Back and forward work naturally, deep links reopen the pane on the ' +
						'right item, and on mobile it expands to a full-screen overlay.'
				}
			}
		);
		if (!createResp.ok()) {
			throw new Error(
				`pane item create failed (${createResp.status()}): ${await createResp.text()}`
			);
		}
		const created = (await createResp.json()) as RefLike;
		let ref = refOf(created);

		// Fallback: if the create response didn't carry a usable ref, look it up
		// in the collection's item list (a bare array) by the id we just created.
		if (!ref && created.id) {
			const listResp = await request.get(
				`/api/v1/workspaces/${ws}/collections/tasks/items`,
				{ headers }
			);
			if (listResp.ok()) {
				const items = (await listResp.json()) as RefLike[];
				const match = Array.isArray(items)
					? items.find((it) => it.id === created.id)
					: undefined;
				if (match) ref = refOf(match);
			}
		}
		if (!ref) throw new Error('could not resolve a ref for the pane item');

		// The pane's body is a live collab editor for an editable (owner) user,
		// and the collab WebSocket handshake authenticates from the SESSION
		// COOKIE — a browser WS can't carry the suite's Bearer header. Without a
		// same-browser login the editor stays stuck "Connecting…" and the body
		// renders loading skeletons instead of the seeded paragraph. Mint the
		// cookie in-page (same pattern as the collab specs) so the pane hydrates.
		await browserLogin(page);

		// `?item=<ref>` opens the docked detail pane beside the tasks list
		// (PLAN-2105). `.item-pane` is the pane region; its `.title` node only
		// renders once the embedded ItemDetail has loaded the item.
		await page.goto(`/${fixture.adminUsername}/${ws}/tasks?item=${ref}`);
		await page.waitForLoadState('domcontentloaded');
		await page
			.locator('.item-pane .title')
			.first()
			.waitFor({ state: 'visible', timeout: 15_000 });
		// Wait for the pane's collab editor to actually render the seeded body
		// (not a connecting-skeleton) so the screenshot shows a lived-in item.
		await page
			.locator('.item-pane .editor-content .ProseMirror')
			.filter({ hasText: 'Open any item beside the list' })
			.first()
			.waitFor({ state: 'visible', timeout: 20_000 });
		await page.waitForTimeout(1000); // settle SSE-driven re-renders
		await page.screenshot({ path: resolve(OUT_DIR, '01-item-pane.png'), fullPage: false });
	});
});
