import { test, expect } from './fixtures';
import { browserLogin } from './lib/collab-helpers';
import type { APIRequestContext, Locator, Page } from '@playwright/test';
import type { SuiteFixture } from './fixtures';

/**
 * TASK-3311 — touch targets at phone width, round 2 (split out of TASK-2244):
 *  1. the list group header's archive button was 18x14 and opacity 0 until
 *     hover, so on touch it could not be seen or reached;
 *  2. the board lane + and ⋯ were 32x32;
 *  3. the card tag buttons were small pills.
 * Scope as TASK-2244: <=768px reaches 44x44, desktop unchanged. Controls that
 * visibly grow are proved by boundingBox; invisible extenders by elementFromPoint,
 * from both sides (the area hits the control, and no neighbour control loses
 * area to it).
 */

const MIN = 44;

function authHeaders(fixture: SuiteFixture) {
	return { Authorization: `Bearer ${fixture.apiToken}`, 'Content-Type': 'application/json' };
}

/** A collection of the test's own with two open items, the first carrying two tags. */
async function seed(request: APIRequestContext, fixture: SuiteFixture, tag: string) {
	const stamp = `${Date.now()}${Math.floor(Math.random() * 1000)}`;
	const schema = JSON.stringify({
		fields: [{ key: 'status', label: 'Status', type: 'select', options: ['open', 'done'], default: 'open', terminal_options: ['done'] }]
	});
	const coll = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/collections`, {
		headers: authHeaders(fixture),
		data: { name: `T3311 ${tag} ${stamp}`, prefix: `TR${stamp.slice(-5)}`, schema }
	});
	expect(coll.ok(), await coll.text()).toBeTruthy();
	const collSlug = (await coll.json()).slug as string;
	const titles = ['a', 'b'].map((x) => `T3311 ${tag} ${x} ${stamp}`);
	for (const [i, title] of titles.entries()) {
		const res = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/collections/${collSlug}/items`, {
			headers: authHeaders(fixture),
			data: {
				title,
				fields: JSON.stringify({ status: 'open' }),
				content: '',
				...(i === 0 ? { tags: JSON.stringify(['t3311a', 't3311b']) } : {})
			}
		});
		expect(res.ok(), await res.text()).toBeTruthy();
	}
	return { collSlug, titles };
}

type Box = { x: number; y: number; w: number; h: number };

/** Sample `box` every 4px; `want` says whether a hit on `target` is sought. */
async function probe(target: Locator, box: Box, want: 'hits' | 'avoids') {
	return target.evaluate(
		(el, [b, mode]) => {
			const bad: string[] = [];
			for (let x = b.x + 0.5; x < b.x + b.w; x += 4) {
				for (let y = b.y + 0.5; y < b.y + b.h; y += 4) {
					const hit = document.elementFromPoint(x, y);
					const onEl = !!hit && (hit === el || el.contains(hit));
					if (onEl !== (mode === 'hits')) bad.push(`${Math.round(x)},${Math.round(y)}→${hit?.className || hit?.tagName}`);
				}
			}
			return bad;
		},
		[box, want] as const
	);
}

async function open(page: Page, fixture: SuiteFixture, collSlug: string, view: 'list' | 'board') {
	await browserLogin(page);
	await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/${collSlug}?view=${view}`);
}

// ── 1. the list group header's archive button ─────────────────────────────

test('TASK-3311: on touch the group archive button is visible and tappable across 44x44', async ({
	page,
	fixture,
	request
}, testInfo) => {
	test.skip(testInfo.project.name !== 'mobile-chromium', 'the ruling is <=768px on touch');
	const { collSlug, titles } = await seed(request, fixture, 'archive');
	await open(page, fixture, collSlug, 'list');
	const btn = page.locator('.archive-group-btn').first();
	await expect(btn).toHaveCount(1);
	const opacity = await btn.evaluate((el) => Number(getComputedStyle(el).opacity));
	expect(opacity, 'hidden until a hover a touch device cannot give').toBeGreaterThan(0.5);
	await expect(btn).toHaveAttribute('aria-label', /^Archive all /);

	const b = (await btn.boundingBox())!;
	const area = { x: b.x + b.width / 2 - MIN / 2, y: b.y + b.height / 2 - MIN / 2, w: MIN, h: MIN };
	expect(await probe(btn, area, 'hits'), 'points in the 44x44 area that miss the archive button').toEqual([]);

	// The first row under the header: none of its controls may lose area to it.
	const row = page.locator('.list-row, .item-row, .list-item').filter({ hasText: titles[1] }).first();
	if ((await row.count()) > 0) {
		for (const c of await row.locator('button, a').all()) {
			const cb = await c.boundingBox();
			if (!cb) continue;
			expect(await probe(btn, { x: cb.x, y: cb.y, w: cb.width, h: cb.height }, 'avoids'), 'row control points on the archive button').toEqual([]);
		}
	}
});

// ── 2. the board lane + and ⋯ ─────────────────────────────────────────────

test('TASK-3311: the board lane + and ⋯ are at least 44x44 on mobile', async ({ page, fixture, request }, testInfo) => {
	test.skip(testInfo.project.name !== 'mobile-chromium', 'the ruling is <=768px');
	const { collSlug } = await seed(request, fixture, 'lane');
	await open(page, fixture, collSlug, 'board');
	for (const [name, sel] of [
		['lane +', '.lane-add-btn'],
		['lane ⋯', '.lane-menu-btn']
	] as const) {
		const btn = page.locator(sel).first();
		await expect(btn, `${name} is rendered`).toBeVisible();
		const b = (await btn.boundingBox())!;
		expect.soft(b.width, `${name} width`).toBeGreaterThanOrEqual(MIN);
		expect.soft(b.height, `${name} height`).toBeGreaterThanOrEqual(MIN);
	}
});

// ── 3. the card tags ──────────────────────────────────────────────────────

for (const view of ['list', 'board'] as const) {
	test(`TASK-3311: each card tag is tappable across 44x44, and no tag or the status chip loses area (${view})`, async ({
		page,
		fixture,
		request
	}, testInfo) => {
		test.skip(testInfo.project.name !== 'mobile-chromium', 'the ruling is <=768px');
		const { collSlug, titles } = await seed(request, fixture, `tags-${view}`);
		await open(page, fixture, collSlug, view);
		const c = page.locator('.item-card').filter({ has: page.locator('.card-title', { hasText: titles[0] }) });
		await expect(c).toBeVisible();
		const tags = c.locator('.card-tag');
		await expect(tags).toHaveCount(2);
		const chip = c.locator('.card-meta button', { hasText: /^\s*open\s*$/i });

		for (let i = 0; i < 2; i++) {
			const tag = tags.nth(i);
			const b = (await tag.boundingBox())!;
			expect.soft(b.width, `tag ${i} width`).toBeGreaterThanOrEqual(MIN);
			const area = { x: b.x, y: b.y + b.height / 2 - MIN / 2, w: b.width, h: MIN };
			expect(await probe(tag, area, 'hits'), `points of tag ${i}'s 44-tall area that miss it`).toEqual([]);
			// Neither the other tag nor the status chip may lose area to this tag.
			const others: Array<[string, Locator]> = [
				[`tag ${1 - i}`, tags.nth(1 - i)],
				['status chip', chip]
			];
			for (const [name, n] of others) {
				if ((await n.count()) === 0) continue;
				const nb = (await n.boundingBox())!;
				expect(await probe(tag, { x: nb.x, y: nb.y, w: nb.width, h: nb.height }, 'avoids'), `points of ${name} that land on tag ${i}`).toEqual([]);
			}
		}
	});
}

// ── desktop is unchanged ──────────────────────────────────────────────────

test('TASK-3311: desktop lane buttons, archive button and tags are unchanged', async ({ page, fixture, request }, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'desktop');
	const { collSlug, titles } = await seed(request, fixture, 'desktop');
	await open(page, fixture, collSlug, 'board');
	const lane = (await page.locator('.lane-add-btn').first().boundingBox())!;
	expect(lane.height, 'desktop lane + stays 28 tall').toBe(28);
	const tag = (await page.locator('.item-card').filter({ has: page.locator('.card-title', { hasText: titles[0] }) }).locator('.card-tag').first().boundingBox())!;
	expect(tag.height, 'desktop tag keeps its pill height').toBeLessThan(28);

	await open(page, fixture, collSlug, 'list');
	const btn = page.locator('.archive-group-btn').first();
	await expect(btn).toHaveCount(1);
	// A mouse device still reveals it on hover only.
	expect(await btn.evaluate((el) => Number(getComputedStyle(el).opacity))).toBe(0);
});
