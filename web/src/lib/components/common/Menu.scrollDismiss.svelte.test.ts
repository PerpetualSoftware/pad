/**
 * BUG-3278: a portal Menu closed on a scroll event that did not move its
 * anchor. A click that first scrolls its card into view (Playwright does;
 * so does a momentum scroll ending under a tap) gets that scroll's event
 * dispatched AFTER the menu opens, and the dismiss closed it in the same
 * frame. Until the first animation frame after opening, a scroll is now
 * ignored if the trigger has not moved; every later scroll is judged as
 * before.
 */
import { afterEach, describe, expect, it, vi } from 'vitest';
import { render, cleanup } from '@testing-library/svelte';
import { createRawSnippet, flushSync } from 'svelte';
import Menu from './Menu.svelte';

vi.mock('$lib/stores/breakpoint.svelte', () => ({
	viewport: {
		get isMobile() {
			return false;
		},
	},
}));

afterEach(() => {
	cleanup();
	document.body.innerHTML = '';
});

const body = createRawSnippet(() => ({ render: () => '<p class="menu-content">Move to top</p>' }));

const nextFrame = () => new Promise<void>((r) => requestAnimationFrame(() => r()));

function setup() {
	const scroller = document.createElement('div');
	const trigger = document.createElement('button');
	scroller.appendChild(trigger);
	const unrelated = document.createElement('div');
	document.body.append(scroller, unrelated);
	let top = 100;
	let left = 40;
	let width = 18;
	vi.spyOn(trigger, 'getBoundingClientRect').mockImplementation(
		() =>
			({ left, top, right: left + width, bottom: top + 14, width, height: 14, x: left, y: top, toJSON: () => ({}) }) as DOMRect
	);
	const onclose = vi.fn();
	render(Menu, { props: { open: true, onclose, trigger, mode: 'portal', children: body } });
	flushSync();
	expect(document.body.querySelector('.menu-content'), 'precondition: the portal panel rendered').not.toBeNull();
	return {
		scroller,
		unrelated,
		onclose,
		moveTrigger: (dy: number) => (top += dy),
		shiftTrigger: (dx: number) => (left += dx),
		growTrigger: (dw: number) => (width += dw),
	};
}

describe('Menu portal scroll-dismiss (BUG-3278)', () => {
	it('a scroll event delivered before the first frame after opening does not close the menu', () => {
		const { scroller, onclose } = setup();
		scroller.dispatchEvent(new Event('scroll'));
		expect(onclose).not.toHaveBeenCalled();
	});

	it('CONTROL: a scroll before the first frame that MOVED the trigger closes the menu', () => {
		const { scroller, onclose, moveTrigger } = setup();
		moveTrigger(-233);
		scroller.dispatchEvent(new Event('scroll'));
		expect(onclose).toHaveBeenCalledTimes(1);
	});

	it('CONTROL: a scroll before the first frame that moved the trigger SIDEWAYS closes the menu', () => {
		const { scroller, onclose, shiftTrigger } = setup();
		shiftTrigger(-120);
		scroller.dispatchEvent(new Event('scroll'));
		expect(onclose).toHaveBeenCalledTimes(1);
	});

	it('CONTROL: a scroll before the first frame that resized the trigger in place closes the menu', () => {
		const { scroller, onclose, growTrigger } = setup();
		growTrigger(30);
		scroller.dispatchEvent(new Event('scroll'));
		expect(onclose).toHaveBeenCalledTimes(1);
	});

	it('CONTROL: the same unmoved scroll one frame later closes the menu, as on main (a sticky trigger)', async () => {
		const { scroller, onclose } = setup();
		await nextFrame();
		scroller.dispatchEvent(new Event('scroll'));
		expect(onclose).toHaveBeenCalledTimes(1);
	});

	it('CONTROL: a scroll in a container that does not hold the trigger still never closes it (BUG-2610)', async () => {
		const { unrelated, onclose } = setup();
		await nextFrame();
		unrelated.dispatchEvent(new Event('scroll'));
		expect(onclose).not.toHaveBeenCalled();
	});

	it('CONTROL: a resize closes the menu, even before the first frame', () => {
		const { onclose } = setup();
		window.dispatchEvent(new Event('resize'));
		expect(onclose).toHaveBeenCalledTimes(1);
	});
});
