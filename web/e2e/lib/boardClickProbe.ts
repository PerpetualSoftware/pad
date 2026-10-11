import { test, type Locator } from '@playwright/test';

/**
 * BUG-3553 diagnostic: click a board card, and if the click fails, record the
 * geometry that explains why, then fail exactly as before. It adds NO wait and
 * changes nothing about a click that succeeds.
 *
 * The failure it is for (pane-controller :583, pane-content-link-anchors
 * :290): after scrolling, another card intercepts the hit point, and the
 * target then reads "not stable" until the test times out. The trace shows a
 * static screen and a column scrollTop far too small for the target's place
 * in the lane, so the target's box at hit-test time is not where normal flow
 * puts it. The fix depends on which mechanism moves it; this records the
 * candidates (content-visibility state, transforms, scroll, the element under
 * the hit point, the box across frames) from the failing run itself, because
 * it has not reproduced locally.
 *
 * The click gets a bounded timeout, so the probe still has time to run inside
 * the test's 30s budget.
 */
export async function clickBoardCard(card: Locator, timeout = 12_000): Promise<void> {
	const testInfo = test.info();
	try {
		await card.click({ timeout });
	} catch (err) {
		const probe = await card
			.evaluate(async (el) => {
				const frame = () => new Promise<void>((r) => requestAnimationFrame(() => r()));
				const rect = (e: Element) => {
					const b = e.getBoundingClientRect();
					return { x: Math.round(b.x), y: Math.round(b.y), w: Math.round(b.width), h: Math.round(b.height) };
				};
				const boxes: ReturnType<typeof rect>[] = [];
				for (let i = 0; i < 10; i++) {
					boxes.push(rect(el));
					await frame();
				}
				const wrapper = el.closest('.card-wrapper');
				const column = el.closest('.column-cards') as HTMLElement | null;
				const wrappers = column ? Array.from(column.querySelectorAll(':scope > .card-wrapper')) : [];
				const at = wrapper ? wrappers.indexOf(wrapper) : -1;
				const b = el.getBoundingClientRect();
				const hit = document.elementFromPoint(b.x + b.width / 2, b.y + b.height / 2);
				const transforms: string[] = [];
				for (let n: Element | null = el; n && n !== document.body; n = n.parentElement) {
					const t = getComputedStyle(n).transform;
					if (t && t !== 'none') transforms.push(`${n.className}: ${t}`);
				}
				const cv = (e: Element | null) =>
					e && 'checkVisibility' in e ? (e as Element & { checkVisibility(o: object): boolean }).checkVisibility({ contentVisibilityAuto: true }) : null;
				const neighbour = (i: number) => (wrappers[i] ? { i, rect: rect(wrappers[i]), rendered: cv(wrappers[i]) } : null);
				return {
					viewport: { w: innerWidth, h: innerHeight },
					boxesOverTenFrames: boxes,
					wrapper: wrapper ? { rect: rect(wrapper), offsetTop: (wrapper as HTMLElement).offsetTop, rendered: cv(wrapper) } : null,
					indexInLane: at,
					laneLength: wrappers.length,
					column: column
						? { scrollTop: column.scrollTop, scrollHeight: column.scrollHeight, clientHeight: column.clientHeight, rect: rect(column) }
						: null,
					neighbours: [neighbour(at - 2), neighbour(at - 1), neighbour(at + 1)],
					hitAtCentre: hit ? { tag: hit.tagName, cls: String(hit.className), text: (hit.textContent ?? '').trim().slice(0, 60), insideTarget: el.contains(hit) } : null,
					transforms,
					activeAnimations: document.getAnimations().length,
					url: location.pathname + location.search,
				};
			})
			.catch((e) => ({ probeFailed: String(e) }));
		const body = JSON.stringify(probe, null, 1);
		console.log(`BUG-3553 board-click probe (${testInfo.title}):\n${body}`);
		await testInfo.attach('bug-3553-board-click-probe', { body, contentType: 'application/json' });
		throw err;
	}
}
