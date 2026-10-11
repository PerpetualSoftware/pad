import { test, expect } from './fixtures';
import { browserLogin } from './lib/collab-helpers';
import type { SuiteFixture } from './fixtures';

/**
 * BUG-3553: a board card far down a long lane must keep a real height while the
 * browser skips it.
 *
 * Board cards use `content-visibility: auto` (TASK-1347), and the lane is a
 * height-constrained flex column that scrolls. With the default
 * `flex-shrink: 1`, the flex layout shrank every SKIPPED card to 0px:
 * size containment gives it no content height to defend, so the 80px
 * placeholder (`contain-intrinsic-size`) never applied. The lane's geometry
 * for off-screen cards was then wrong by a whole card each. Measured on a
 * 200-card lane: 178 cards at 0px, and scrollIntoView on the last card put
 * the column at scrollTop 2871 instead of about 24500, the same figure as
 * the CI failures (2921). Any scroll to a far card (a test's click, keyboard
 * focus, a deep link) aimed at that false geometry, and the click landed on
 * whatever card the real layout put there.
 *
 * Its own collection, so the shared Docs board gets no extra cards.
 */

function authHeaders(fixture: SuiteFixture) {
	return { Authorization: `Bearer ${fixture.apiToken}`, 'Content-Type': 'application/json' };
}

test('BUG-3553: skipped board cards keep a height, and scrolling to the last card centres it', async ({ page, fixture, request }, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'a desktop lane layout');
	test.setTimeout(90_000);
	const schema = JSON.stringify({
		fields: [{ key: 'status', label: 'Status', type: 'select', options: ['open', 'done'], default: 'open' }],
	});
	const coll = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/collections`, {
		headers: authHeaders(fixture),
		data: { name: `B3553 lane ${Date.now()}`, prefix: 'BLN', schema },
	});
	expect(coll.ok(), await coll.text()).toBeTruthy();
	const { slug: collSlug } = (await coll.json()) as { slug: string };
	// Enough cards that most of the lane is far outside the viewport.
	const N = 80;
	let last = '';
	for (let i = 0; i < N; i++) {
		const r = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/collections/${collSlug}/items`, {
			headers: authHeaders(fixture),
			data: { title: `Lane card ${i}${i % 3 ? '' : ' with a title long enough to wrap'}`, fields: JSON.stringify({ status: 'open' }), content: '' },
		});
		expect(r.ok(), await r.text()).toBeTruthy();
		last = ((await r.json()) as { slug: string }).slug;
	}

	await page.setViewportSize({ width: 1200, height: 900 });
	await browserLogin(page);
	await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/${collSlug}?view=board`);
	const target = page.locator(`.item-card[data-item-slug="${last}"]`);
	await expect(target).toHaveCount(1);

	const out = await target.evaluate(async (el) => {
		const frame = () => new Promise<void>((r) => requestAnimationFrame(() => r()));
		const col = el.closest('.column-cards') as HTMLElement;
		const wrappers = Array.from(col.querySelectorAll(':scope > .card-wrapper'));
		const zero = wrappers.filter((w) => w.getBoundingClientRect().height < 1).length;
		el.scrollIntoView({ block: 'center' });
		await frame();
		await frame();
		const b = el.getBoundingClientRect();
		const c = col.getBoundingClientRect();
		const hit = document.elementFromPoint(b.x + b.width / 2, b.y + b.height / 2);
		return {
			cards: wrappers.length,
			zero,
			inColumn: b.top >= c.top - 1 && b.bottom <= c.bottom + 1,
			hitIsTarget: !!hit && el.contains(hit),
		};
	});
	// The vantage point: the lane really is long and mostly off-screen.
	expect(out.cards).toBe(N);
	// The property: no skipped card collapses.
	expect(out.zero, 'skipped cards at 0px height').toBe(0);
	// The consequence: the scroll lands on the card, and a click there hits it.
	expect(out.inColumn).toBe(true);
	expect(out.hitIsTarget).toBe(true);
});
