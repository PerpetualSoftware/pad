import type { APIRequestContext, Page } from '@playwright/test';
import { test, expect, type SuiteFixture } from './fixtures';
import { browserLogin } from './lib/collab-helpers';
import { createWorkspace } from './lib/attachment-viewer';

/**
 * TASK-2230: a board lane mounts a window of its cards (50 for a terminal
 * lane), and the full lane is what gets persisted. The drop that proves it is
 * at the END of the window: the drag zone holds only the 50 mounted cards, so
 * numbering that window alone would give the dropped card max + 1 = 50, the
 * value the first hidden card already holds. Rebuilt around the full lane, the
 * card lands between the last mounted card and the first hidden one, and the
 * stored values stay strictly increasing in board order.
 *
 * Its own workspace: the order is read back from the server, and other specs'
 * writes must not move cards in it.
 */

function auth(fixture: SuiteFixture) {
	return { Authorization: `Bearer ${fixture.apiToken}`, 'Content-Type': 'application/json' };
}

async function seed(fixture: SuiteFixture, request: APIRequestContext) {
	const ws = (await createWorkspace(fixture, request, 'Lane window')).slug;
	const create = async (title: string, status: string) => {
		const r = await request.post(`/api/v1/workspaces/${ws}/collections/tasks/items`, {
			headers: auth(fixture),
			data: { title, fields: JSON.stringify({ status }) },
		});
		if (!r.ok()) throw new Error(`create: ${r.status()} ${await r.text()}`);
		return (await r.json()) as { id: string };
	};
	const done: { id: string }[] = [];
	for (let i = 0; i < 60; i++) done.push(await create(`Done ${String(i).padStart(2, '0')}`, 'done'));
	// Created LAST, so a tie with a done card's value would sort it after that
	// card: the strictness assertion below catches the tie either way.
	const mover = await create('Mover card', 'open');
	// Distinct stored order 0..59 for the done lane.
	const r = await request.put(`/api/v1/workspaces/${ws}/items/sort-order`, {
		headers: auth(fixture),
		data: { updates: done.map((d, i) => ({ id: d.id, sort_order: i })) },
	});
	if (!r.ok()) throw new Error(`sort-order: ${r.status()} ${await r.text()}`);
	return { ws, mover: mover.id };
}

/** The done lane as the server orders it: [title, sort_order] pairs. */
async function storedDoneLane(request: APIRequestContext, fixture: SuiteFixture, ws: string) {
	const r = await request.get(`/api/v1/workspaces/${ws}/collections/tasks/items?status=done&limit=500`, { headers: auth(fixture) });
	const rows = (await r.json()) as { title: string; sort_order: number; created_at: string }[];
	return rows
		.sort((a, b) => a.sort_order - b.sort_order || a.created_at.localeCompare(b.created_at))
		.map((x) => [x.title, x.sort_order] as const);
}

/**
 * Drag a card into a lane at a height, retrying a gesture the library did not
 * register: under load svelte-dnd-action can lag the pointer, and a release
 * before it moved the card into the target lane finalizes in the source lane
 * and writes nothing (permission-walk-collections.spec.ts measured both). A
 * gesture is retried only when the card never entered the target.
 */
async function dragIntoLaneAt(page: Page, title: string, lane: string, y: () => Promise<number>) {
	const card = page.locator('.item-card', { hasText: title });
	const target = page.getByRole('group', { name: `${lane} column`, exact: true });
	for (let attempt = 0; attempt < 3; attempt++) {
		const from = (await card.boundingBox())!;
		const zone = (await target.locator('.column-cards').boundingBox())!;
		await page.mouse.move(from.x + from.width / 2, from.y + from.height / 2);
		await page.mouse.down();
		await page.mouse.move(from.x + from.width / 2 + 10, from.y + from.height / 2 + 10, { steps: 4 });
		await page.mouse.move(zone.x + zone.width / 2, await y(), { steps: 20 });
		const entered = await expect(target.locator('.item-card', { hasText: title }))
			.not.toHaveCount(0, { timeout: 5_000 })
			.then(() => true)
			.catch(() => false);
		await page.mouse.up();
		if (entered) return;
		await page.waitForTimeout(500);
	}
	throw new Error(`the drag of "${title}" never entered the ${lane} lane in 3 tries`);
}

test('a drop at the end of a capped lane keeps the stored order strict around the hidden cards', async ({ page, fixture, request }, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'the board drag is a desktop gesture');
	test.setTimeout(90_000);
	const { ws } = await seed(fixture, request);
	await browserLogin(page);
	await page.setViewportSize({ width: 1600, height: 1000 });
	await page.goto(`/${fixture.adminUsername}/${ws}/tasks?view=board`);
	const done = page.getByRole('group', { name: 'Done column', exact: true });
	await expect(done.locator('.card-wrapper')).toHaveCount(50);
	await expect(done.getByText('Show all 60 (10 more)')).toBeVisible();

	// Scroll the lane to the end of its window, then drop just above the
	// "Show all" control: after the 50th mounted card, before the hidden ones.
	// Cards off screen use content-visibility with an 80px placeholder, so the
	// lane's scrollHeight is an estimate that grows as cards render: one
	// scroll-to-bottom lands short. Scroll until the 50th card is in view.
	const list = done.locator('.column-cards');
	const last = done.locator('.card-wrapper').nth(49);
	/** The height just below the middle of the 50th card, once it is in view:
	 *  after the last mounted card, before the hidden ones. Re-measured per
	 *  attempt, since a retried gesture can leave the lane scrolled. */
	const endOfWindow = async () => {
		await expect
			.poll(
				async () => {
					await list.evaluate((el) => (el.scrollTop = el.scrollHeight));
					const box = await last.boundingBox();
					const zone = await list.boundingBox();
					return !!box && !!zone && box.y + box.height <= zone.y + zone.height + 1;
				},
				{ timeout: 10_000 }
			)
			.toBe(true);
		const box = (await last.boundingBox())!;
		return box.y + box.height * 0.8;
	};
	const sortWrite = page.waitForRequest((r) => r.method() === 'PUT' && r.url().includes('/items/sort-order'), { timeout: 30_000 });
	await dragIntoLaneAt(page, 'Mover card', 'Done', endOfWindow);
	await sortWrite;

	await expect
		.poll(async () => (await storedDoneLane(request, fixture, ws)).map(([t]) => t).indexOf('Mover card'), { timeout: 10_000 })
		.toBeGreaterThan(-1);
	const laneNow = await storedDoneLane(request, fixture, ws);
	const titles = laneNow.map(([t]) => t);
	testInfo.annotations.push({ type: 'done-lane', description: laneNow.map(([t, v]) => `${t}=${v}`).join(', ') });
	// The card landed at the window's end, ahead of every hidden card...
	const at = titles.indexOf('Mover card');
	expect(at, 'dropped at the end of the mounted window').toBeGreaterThanOrEqual(45);
	expect(at).toBeLessThanOrEqual(50);
	// ...the done cards kept their order around it...
	expect(titles.filter((t) => t !== 'Mover card')).toEqual(Array.from({ length: 60 }, (_, i) => `Done ${String(i).padStart(2, '0')}`));
	// ...and no two cards share a value: the order does not lean on a tie.
	const values = laneNow.map(([, v]) => v);
	for (let i = 1; i < values.length; i++) expect(values[i], `${titles[i - 1]} < ${titles[i]}`).toBeGreaterThan(values[i - 1]);
});
