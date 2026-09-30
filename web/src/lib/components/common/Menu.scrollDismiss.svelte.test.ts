/**
 * BUG-3278: a portal Menu closed on a scroll event that did not move its
 * anchor. A click that first scrolls its card into view (Playwright does;
 * so does a momentum scroll ending under a tap) gets that scroll's event
 * dispatched AFTER the menu opens, and the dismiss closed it in the same
 * frame. The rule is now: a scroll closes the menu only if the trigger moved
 * since the menu opened.
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

function setup() {
	const scroller = document.createElement('div');
	const trigger = document.createElement('button');
	scroller.appendChild(trigger);
	const unrelated = document.createElement('div');
	document.body.append(scroller, unrelated);

	let top = 100;
	vi.spyOn(trigger, 'getBoundingClientRect').mockImplementation(
		() => ({ left: 40, top, right: 58, bottom: top + 14, width: 18, height: 14, x: 40, y: top, toJSON: () => ({}) }) as DOMRect
	);
	const onclose = vi.fn();
	render(Menu, { props: { open: true, onclose, trigger, mode: 'portal', children: body } });
	flushSync();
	expect(document.body.querySelector('.menu-content'), 'precondition: the portal panel rendered').not.toBeNull();
	return { scroller, unrelated, onclose, moveTrigger: (dy: number) => (top += dy) };
}

describe('Menu portal scroll-dismiss (BUG-3278)', () => {
	it('a scroll in an ancestor that left the trigger where it was does not close the menu', () => {
		const { scroller, onclose } = setup();
		scroller.dispatchEvent(new Event('scroll'));
		expect(onclose).not.toHaveBeenCalled();
	});

	it('CONTROL: a scroll in an ancestor that moved the trigger closes the menu', () => {
		const { scroller, onclose, moveTrigger } = setup();
		moveTrigger(-233);
		scroller.dispatchEvent(new Event('scroll'));
		expect(onclose).toHaveBeenCalledTimes(1);
	});

	it('CONTROL: a scroll in a container that does not hold the trigger still never closes it (BUG-2610)', () => {
		const { unrelated, onclose, moveTrigger } = setup();
		moveTrigger(-50);
		unrelated.dispatchEvent(new Event('scroll'));
		expect(onclose).not.toHaveBeenCalled();
	});

	it('CONTROL: a resize still closes the menu unconditionally', () => {
		const { onclose } = setup();
		window.dispatchEvent(new Event('resize'));
		expect(onclose).toHaveBeenCalledTimes(1);
	});
});
