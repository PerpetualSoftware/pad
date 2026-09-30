/**
 * BUG-3278: a portal Menu closed on a scroll event that did not move its
 * anchor. A click that first scrolls its card into view (Playwright does;
 * so does a momentum scroll ending under a tap) gets that scroll's event
 * dispatched AFTER the menu opens, and the dismiss closed it in the same
 * frame. Scroll events are now ignored until the first animation frame after
 * the menu opens; every later scroll is judged as before.
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
	const onclose = vi.fn();
	render(Menu, { props: { open: true, onclose, trigger, mode: 'portal', children: body } });
	flushSync();
	expect(document.body.querySelector('.menu-content'), 'precondition: the portal panel rendered').not.toBeNull();
	return { scroller, unrelated, onclose };
}

describe('Menu portal scroll-dismiss (BUG-3278)', () => {
	it('a scroll event delivered before the first frame after opening does not close the menu', () => {
		const { scroller, onclose } = setup();
		scroller.dispatchEvent(new Event('scroll'));
		expect(onclose).not.toHaveBeenCalled();
	});

	it('CONTROL: the same scroll one frame later closes the menu', async () => {
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
