import { test, expect } from './fixtures';
import { browserLogin } from './lib/collab-helpers';
import type { APIRequestContext, Locator, Page } from '@playwright/test';
import type { SuiteFixture } from './fixtures';

/**
 * TASK-2244 — touch targets at phone width (Dave's ruling, day 83: options 1+3).
 *
 * The collection control strip grows VISIBLY to 44x44 at <=768px, so its proof is
 * the element box (boundingBox). Desktop is unchanged, pinned to the sizes
 * measured on the base commit, so a mobile rule that leaks past its media query
 * goes red here rather than in a screenshot nobody compares.
 */

const MIN = 44;

function strip(page: Page) {
	return page.locator('.header-actions');
}

/** Every interactive control the strip renders, by the name a user would see. */
function stripControls(page: Page, isMobile: boolean): Array<[string, Locator]> {
	const s = strip(page);
	return [
		[
			'view',
			isMobile ? s.locator('button.view-chip') : s.locator('button.view-dd-trigger'),
		],
		['sort', s.getByRole('button', { name: 'Sort items' })],
		['filters', s.getByRole('button', { name: 'Toggle filters' })],
		['quick actions', s.locator('.quick-actions-menu > .trigger-btn')],
		['collection menu', s.getByRole('button', { name: 'Collection menu' })],
		['new', s.locator('button.new-btn')],
	];
}

async function sizes(page: Page, isMobile: boolean) {
	const out: Record<string, { w: number; h: number }> = {};
	for (const [name, loc] of stripControls(page, isMobile)) {
		await expect(loc, `${name} control is rendered`).toHaveCount(1);
		const box = await loc.boundingBox();
		expect(box, `${name} has a box`).not.toBeNull();
		out[name] = { w: Math.round(box!.width * 10) / 10, h: Math.round(box!.height * 10) / 10 };
	}
	return out;
}

for (const view of ['list', 'board'] as const) {
	test(`TASK-2244: every strip control is at least 44x44 on mobile (${view})`, async ({
		page,
		fixture,
	}, testInfo) => {
		test.skip(testInfo.project.name !== 'mobile-chromium', 'the ruling is <=768px');

		await browserLogin(page);
		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/tasks?view=${view}`);
		await expect(strip(page)).toBeVisible();

		const measured = await sizes(page, true);
		for (const [name, { w, h }] of Object.entries(measured)) {
			expect.soft(w, `${name} width (${w}x${h})`).toBeGreaterThanOrEqual(MIN);
			expect.soft(h, `${name} height (${w}x${h})`).toBeGreaterThanOrEqual(MIN);
		}
	});
}

/**
 * The strip stays ONE row where it fits (the lead's ruling on the 360 checkpoint,
 * option B: the chip drops its visible "View:" prefix). A future control or a
 * longer label would otherwise re-wrap it silently, since every size assertion
 * above still passes on two rows. The realtime dot is a badge on the collection
 * ⋯ button's corner, so it can never wrap onto a row of its own: list at 360 used
 * to wrap it alone, which Dave's gate called awkward, and is now held to one row
 * too.
 */
const ONE_ROW: Array<{ width: number; view: 'list' | 'board'; withDot: boolean }> = [
	{ width: 390, view: 'list', withDot: true },
	{ width: 390, view: 'board', withDot: true },
	{ width: 360, view: 'board', withDot: true },
	{ width: 360, view: 'list', withDot: true },
];

for (const { width, view, withDot } of ONE_ROW) {
	test(`TASK-2244: the strip is one row at ${width}px (${view})`, async ({ page, fixture }, testInfo) => {
		test.skip(testInfo.project.name !== 'mobile-chromium', 'phone widths');

		await page.setViewportSize({ width, height: 844 });
		await browserLogin(page);
		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/tasks?view=${view}`);
		await expect(strip(page)).toBeVisible();

		// Same row = each control's vertical centre lies inside the first
		// control's span. Not equal tops: on one row, sub-pixel layout moves a
		// top by 1px (measured on base, 61 vs 60), and a wrap moves it by a row.
		const boxes = [];
		for (const [name, loc] of stripControls(page, true)) {
			const box = await loc.boundingBox();
			expect(box, `${name} has a box`).not.toBeNull();
			boxes.push({ name, top: box!.y, bottom: box!.y + box!.height, mid: box!.y + box!.height / 2 });
		}
		const rowTop = boxes[0].top;
		for (const b of boxes) {
			expect(b.mid, `${b.name} is on the first row`).toBeGreaterThan(boxes[0].top);
			expect(b.mid, `${b.name} is on the first row`).toBeLessThan(boxes[0].bottom);
		}

		if (withDot) {
			// The dot rides the collection ⋯ button's corner: its box lies inside
			// the button's, so it adds no flex item and cannot wrap.
			// Located by its status role, which the old flex-item placement had
			// too, so this leg fails on that layout for the wrap, not the selector.
			const dot = await strip(page).locator('[role="status"][aria-label^="Live updates"] .sse-state-dot').boundingBox();
			const menu = await page.getByRole('button', { name: 'Collection menu' }).boundingBox();
			expect(dot, 'the realtime dot has a box').not.toBeNull();
			expect(menu, 'the collection menu button has a box').not.toBeNull();
			expect(dot!.y, 'the realtime dot shares the row').toBeLessThan(rowTop + MIN);
			expect(dot!.x, 'the dot sits on the menu button').toBeGreaterThanOrEqual(menu!.x);
			expect(dot!.x + dot!.width, 'the dot sits on the menu button').toBeLessThanOrEqual(menu!.x + menu!.width);
			expect(dot!.y, 'the dot sits on the menu button').toBeGreaterThanOrEqual(menu!.y);
			expect(dot!.y + dot!.height, 'the dot sits on the menu button').toBeLessThanOrEqual(menu!.y + menu!.height);
			// It keeps its status region and label.
			await expect(strip(page).locator('[role="status"][aria-label^="Live updates: "]')).toHaveCount(1);
		}
	});
}

/**
 * Desktop is unchanged. Pinned: what the CSS fixes. Every strip control is 28px
 * tall, and sort, filters and the collection menu are 30px wide. A text-sized
 * width (the view label, "+ New Task", the ⚡ trigger) depends on the machine's
 * fonts: CI measured 77.6 / 101.1 / 34.4 where this box measured 75 / 93.8 / 41,
 * so those are not pinned. The height is the leak signal, since the phone rule
 * makes every control 44 tall.
 */
const DESKTOP_FIXED: Record<string, { w?: number; h: number }> = {
	view: { h: 28 },
	sort: { w: 30, h: 28 },
	filters: { w: 30, h: 28 },
	'quick actions': { h: 28 },
	'collection menu': { w: 30, h: 28 },
	new: { h: 28 },
};

for (const view of ['list', 'board'] as const) {
	test(`TASK-2244: desktop strip sizes are unchanged (${view})`, async ({ page, fixture }, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'the desktop leg');

		await browserLogin(page);
		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/tasks?view=${view}`);
		await expect(strip(page)).toBeVisible();

		const measured = await sizes(page, false);
		for (const [name, want] of Object.entries(DESKTOP_FIXED)) {
			expect.soft(measured[name].h, `${name} height`).toBe(want.h);
			if (want.w !== undefined) expect.soft(measured[name].w, `${name} width`).toBe(want.w);
		}
	});
}

// ── U-b: the card's controls ────────────────────────────────────────────────

function authHeaders(fixture: SuiteFixture) {
	return { Authorization: `Bearer ${fixture.apiToken}`, 'Content-Type': 'application/json' };
}

/**
 * Three open items in one board lane, so the middle one has every reorder entry,
 * in a collection of the test's OWN. The shared Tasks lane collects every other
 * leg's items (and a repeat's), which put a seeded card out of view and tied its
 * sort_order with strangers'.
 */
async function seedLane(request: APIRequestContext, fixture: SuiteFixture, tag: string) {
	const stamp = `${Date.now()}${Math.floor(Math.random() * 1000)}`;
	const schema = JSON.stringify({
		fields: [
			{ key: 'status', label: 'Status', type: 'select', options: ['open', 'done'], default: 'open', terminal_options: ['done'] },
		],
	});
	const coll = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/collections`, {
		headers: authHeaders(fixture),
		data: { name: `T2244 ${tag} ${stamp}`, prefix: `TT${stamp.slice(-5)}`, schema },
	});
	expect(coll.ok(), await coll.text()).toBeTruthy();
	const collSlug = (await coll.json()).slug as string;

	const titles = ['a', 'b', 'c'].map((x) => `T2244 ${tag} ${x} ${stamp}`);
	const slugs: string[] = [];
	for (const title of titles) {
		const res = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/collections/${collSlug}/items`, {
			headers: authHeaders(fixture),
			data: { title, fields: JSON.stringify({ status: 'open' }), content: '' },
		});
		expect(res.ok(), await res.text()).toBeTruthy();
		slugs.push((await res.json()).slug);
	}
	// Fresh items tie on sort_order, and a tie renders in no fixed order, so the
	// lane order is set explicitly: a, b, c.
	for (const [i, slug] of slugs.entries()) {
		const res = await request.patch(`/api/v1/workspaces/${fixture.workspaceSlug}/items/${slug}`, {
			headers: authHeaders(fixture),
			data: { sort_order: (i + 1) * 10 },
		});
		expect(res.ok(), await res.text()).toBeTruthy();
	}
	return { titles, slugs, collSlug };
}

function card(page: Page, title: string) {
	return page.locator('.item-card').filter({ has: page.locator('.card-title', { hasText: title }) });
}

async function isStarred(request: APIRequestContext, fixture: SuiteFixture, slug: string) {
	const res = await request.get(`/api/v1/workspaces/${fixture.workspaceSlug}/items/${slug}/star`, {
		headers: authHeaders(fixture),
	});
	expect(res.ok(), await res.text()).toBeTruthy();
	return (await res.json()).starred as boolean;
}

for (const view of ['list', 'board'] as const) {
	test(`TASK-2244: a mobile card has ONE 44x44 ⋯ and no star, copy or ⋮ (${view})`, async ({
		page,
		fixture,
		request,
	}, testInfo) => {
		test.skip(testInfo.project.name !== 'mobile-chromium', 'the ruling is <=768px');
		const { titles, collSlug } = await seedLane(request, fixture, `shape-${view}`);

		await browserLogin(page);
		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/${collSlug}?view=${view}`);
		const c = card(page, titles[1]);
		await expect(c).toBeVisible();

		await expect(c.locator('.star-btn'), 'no star button on mobile').toHaveCount(0);
		await expect(c.locator('.copy-ref-btn'), 'no copy button on mobile').toHaveCount(0);
		await expect(c.locator('.iam-trigger:not(.card)'), 'no reorder ⋮ on mobile').toHaveCount(0);
		const more = c.locator('.iam-trigger.card');
		await expect(more).toHaveCount(1);
		const box = await more.boundingBox();
		expect(box).not.toBeNull();
		expect(box!.width, `⋯ width (${box!.width}x${box!.height})`).toBeGreaterThanOrEqual(MIN);
		expect(box!.height, `⋯ height (${box!.width}x${box!.height})`).toBeGreaterThanOrEqual(MIN);
	});
}

test('TASK-2244: the mobile card ⋯ stars, copies the ID and reorders (board)', async ({
	page,
	fixture,
	request,
}, testInfo) => {
	test.skip(testInfo.project.name !== 'mobile-chromium', 'the ruling is <=768px');
	const { titles, slugs, collSlug } = await seedLane(request, fixture, 'wire');

	await page.context().grantPermissions(['clipboard-read', 'clipboard-write']);
	await browserLogin(page);
	await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/${collSlug}?view=board`);
	const c = card(page, titles[2]);
	await expect(c).toBeVisible();
	const more = c.locator('.iam-trigger.card');

	// Star: the server records it, and the card shows the passive mark.
	expect(await isStarred(request, fixture, slugs[2]), 'precondition: not starred').toBe(false);
	await expect(c.locator('.starred-mark')).toHaveCount(0);
	await more.tap();
	await page.getByRole('menuitem', { name: 'Star', exact: true }).tap();
	await expect.poll(() => isStarred(request, fixture, slugs[2])).toBe(true);
	await expect(c.locator('.starred-mark'), 'a starred card shows the passive mark').toBeVisible();
	await expect(page, 'the tap did not navigate').toHaveURL(new RegExp(`/${collSlug}\\?view=board`));

	// Copy: the clipboard receives the ref the card shows.
	const ref = (await c.locator('.item-ref').innerText()).trim();
	await page.evaluate(() => navigator.clipboard.writeText('T2244-SENTINEL'));
	await more.tap();
	await page.getByRole('menuitem', { name: 'Copy item ID' }).tap();
	await expect.poll(() => page.evaluate(() => navigator.clipboard.readText())).toBe(ref);

	// Reorder: the card moves to the top of its lane, on screen and on the
	// server (a reload after the write's own response, never after the tap).
	const lane = page.locator('.board-view .item-card .card-title');
	const mine = async () => {
		const all = await lane.allInnerTexts();
		return all.map((t) => titles.indexOf(t.trim())).filter((i) => i >= 0);
	};
	const before = await mine();
	expect(before[0], `precondition: the card is not already first (${before})`).not.toBe(2);
	await more.tap();
	await page.getByRole('menuitem', { name: 'Move to top' }).tap();
	const moved = [2, ...before.filter((i) => i !== 2)];
	await expect.poll(mine, 'on screen').toEqual(moved);
	// persistReorder writes whichever rows' sort_order changes, not always the
	// moved card (one PUT /items/sort-order since TASK-3517), so the server's
	// answer is read by reloading until it agrees.
	await expect
		.poll(
			async () => {
				await page.reload();
				await expect(lane.first()).toBeVisible();
				return mine();
			},
			{ message: 'after a reload (the server order)', timeout: 15000 },
		)
		.toEqual(moved);
});

test('TASK-2244: on a host without reorder the mobile ⋯ holds only Star and Copy (starred page)', async ({
	page,
	fixture,
	request,
}, testInfo) => {
	test.skip(testInfo.project.name !== 'mobile-chromium', 'the ruling is <=768px');
	const { titles, slugs } = await seedLane(request, fixture, 'starred');
	const star = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/items/${slugs[0]}/star`, {
		headers: authHeaders(fixture),
	});
	expect(star.ok(), await star.text()).toBeTruthy();

	await browserLogin(page);
	await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/starred`);
	const c = card(page, titles[0]);
	await expect(c).toBeVisible();
	await expect(c.locator('.starred-mark')).toBeVisible();
	await c.locator('.iam-trigger.card').tap();
	const items = page.getByRole('menuitem');
	await expect(items).toHaveText(['Unstar', 'Copy item ID'].map((t) => new RegExp(t)));
});

/**
 * Desktop card controls are unchanged. Pinned: the copy button, whose CSS fixes
 * it at 22x22. The star and the ⋮ are glyph-sized and depend on the machine's
 * fonts (CI measured ⋮ 18 and star 11.9 wide where this box measured 8 and 13),
 * so for them the check is that they stayed small: the phone rule would make
 * one 44x44 ⋯ in their place.
 */
for (const view of ['list', 'board'] as const) {
	test(`TASK-2244: desktop card controls are unchanged (${view})`, async ({ page, fixture, request }, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'the desktop leg');
		const { titles, collSlug } = await seedLane(request, fixture, `desk-${view}`);

		await browserLogin(page);
		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/${collSlug}?view=${view}`);
		const c = card(page, titles[1]);
		await expect(c).toBeVisible();

		for (const sel of ['.star-btn', '.copy-ref-btn', '.iam-trigger']) {
			await expect(c.locator(sel), `${sel} is rendered on desktop`).toHaveCount(1);
		}
		const copy = (await c.locator('.copy-ref-btn').boundingBox())!;
		expect({ w: copy.width, h: copy.height }, 'copy button').toEqual({ w: 22, h: 22 });
		for (const sel of ['.star-btn', '.iam-trigger']) {
			const b = (await c.locator(sel).boundingBox())!;
			expect.soft(b.width, `${sel} width stays small`).toBeLessThan(MIN);
			expect.soft(b.height, `${sel} height stays small`).toBeLessThan(MIN);
		}
		await expect(c.locator('.iam-trigger.card'), 'no card ⋯ on desktop').toHaveCount(0);
		await expect(c.locator('.starred-mark'), 'no passive mark on desktop').toHaveCount(0);
	});
}

// ── U-c: the status chip's invisible extender ───────────────────────────────

/**
 * The extender does not change the chip's box, so boundingBox cannot prove it.
 * The instrument is elementFromPoint, from both sides:
 *   1. every point of the 44x44 area hits the chip. The area is centred
 *      horizontally and grows UPWARD from the chip's bottom edge, because a
 *      centred square took the top of the first tag (measured on this leg);
 *   2. no point of a neighbour control's box (both tag buttons below, and the
 *      card ⋯ above) lands on the chip. Asked that way, not as "every point
 *      hits the neighbour": a rounded tag's corner pixel hits its row, with or
 *      without any extender, and that is not what the ruling is about. A
 *      centred extender DID land 20 of tag 0's 40 sampled points on the chip
 *      (list and board alike; the upward one lands 0), which is the measurement
 *      that made it upward-only.
 * The card carries tags on purpose: without them the row below is plain text,
 * and half 2 would pass on any extender, however large.
 */
type Box = { x: number; y: number; w: number; h: number };

/** Sample the box every 4px; `want` says whether a hit on `el` is what is sought. */
async function probe(target: Locator, box: Box, want: 'hits' | 'avoids') {
	return target.evaluate(
		(el, [b, mode]) => {
			const bad: string[] = [];
			const step = 4;
			for (let x = b.x + 0.5; x < b.x + b.w; x += step) {
				for (let y = b.y + 0.5; y < b.y + b.h; y += step) {
					const hit = document.elementFromPoint(x, y);
					const onEl = !!hit && (hit === el || el.contains(hit));
					if (onEl !== (mode === 'hits')) {
						bad.push(`${Math.round(x)},${Math.round(y)}→${hit?.className || hit?.tagName}`);
					}
				}
			}
			return bad;
		},
		[box, want] as const,
	);
}

for (const view of ['list', 'board'] as const) {
	test(`TASK-2244: the status chip is tappable across 44x44 and takes nothing from the tags (${view})`, async ({
		page,
		fixture,
		request,
	}, testInfo) => {
		test.skip(testInfo.project.name !== 'mobile-chromium', 'the ruling is <=768px');
		const { titles, slugs, collSlug } = await seedLane(request, fixture, `chip-${view}`);
		const tagged = await request.patch(`/api/v1/workspaces/${fixture.workspaceSlug}/items/${slugs[1]}`, {
			headers: authHeaders(fixture),
			data: { tags: JSON.stringify(['t2244a', 't2244b']) },
		});
		expect(tagged.ok(), await tagged.text()).toBeTruthy();

		await browserLogin(page);
		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/${collSlug}?view=${view}`);
		const c = card(page, titles[1]);
		await expect(c).toBeVisible();
		// Addressed without the new wrapper class, so the leg also runs on base.
		const chip = c.locator('.card-meta button', { hasText: /^\s*open\s*$/i });
		await expect(chip, 'the status chip is a picker on this card').toHaveCount(1);
		const tags = c.locator('.card-tag');
		await expect(tags, 'precondition: the tag row the extender must not cover').toHaveCount(2);

		const b = (await chip.boundingBox())!;
		const area = { x: b.x + b.width / 2 - MIN / 2, y: b.y + b.height - MIN, w: MIN, h: MIN };
		expect(await probe(chip, area, 'hits'), 'points in the 44x44 area that miss the chip').toEqual([]);

		const neighbours: Array<[string, Locator]> = [
			['tag 0', tags.nth(0)],
			['tag 1', tags.nth(1)],
			['card ⋯', c.locator('.iam-trigger.card')],
		];
		for (const [name, n] of neighbours) {
			if ((await n.count()) === 0) continue; // base has no card ⋯; half 1 already fails there
			const nb = (await n.boundingBox())!;
			expect(
				await probe(chip, { x: nb.x, y: nb.y, w: nb.width, h: nb.height }, 'avoids'),
				`points of ${name} that land on the status chip`,
			).toEqual([]);
		}
	});
}
