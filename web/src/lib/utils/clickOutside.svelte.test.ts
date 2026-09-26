import { describe, it, expect, vi, afterEach } from 'vitest';
import { clickOutside } from './clickOutside';

// BUG-3231: the shared outside-dismissal primitive decides on the PRESS. A
// drag that starts inside the node and is released outside produces a click
// on a common ancestor outside the node; a click-based closer took that for an
// outside click. This one never sees it, because the press was inside.

function setup(opts: Partial<Parameters<typeof clickOutside>[1]> = {}) {
	const outside = document.body.appendChild(document.createElement('div'));
	const node = document.body.appendChild(document.createElement('div'));
	const inside = node.appendChild(document.createElement('span'));
	const extraEl = document.body.appendChild(document.createElement('button'));
	const onOutside = vi.fn();
	const action = clickOutside(node, { onOutside, ...opts });
	return { outside, node, inside, extraEl, onOutside, action };
}

const down = (el: Element) => el.dispatchEvent(new Event('pointerdown', { bubbles: true }));
const up = (el: Element) => el.dispatchEvent(new Event('pointerup', { bubbles: true }));
const click = (el: Element) => el.dispatchEvent(new MouseEvent('click', { bubbles: true }));

afterEach(() => {
	document.body.innerHTML = '';
});

describe('clickOutside (BUG-3231)', () => {
	it('a press outside dismisses', () => {
		const { outside, onOutside } = setup();
		down(outside);
		expect(onOutside).toHaveBeenCalledTimes(1);
	});

	it('a drag that starts inside and is released outside does not dismiss', () => {
		const { inside, outside, onOutside } = setup();
		down(inside);
		up(outside);
		click(document.body); // the common ancestor of the press and the release
		expect(onOutside).not.toHaveBeenCalled();
	});

	it('a press inside does not dismiss', () => {
		const { inside, onOutside } = setup();
		down(inside);
		expect(onOutside).not.toHaveBeenCalled();
	});

	it('a press on an extra container does not dismiss', () => {
		const { extraEl, onOutside } = setup({ extra: () => [document.querySelector('button')] });
		down(extraEl);
		expect(onOutside).not.toHaveBeenCalled();
	});

	it('enabled=false and suppress hold it off', () => {
		const a = setup({ enabled: false });
		down(a.outside);
		expect(a.onOutside).not.toHaveBeenCalled();
		a.action.destroy();
		document.body.innerHTML = '';
		const b = setup({ suppress: () => true });
		down(b.outside);
		expect(b.onOutside).not.toHaveBeenCalled();
	});

	it('destroy removes the listener', () => {
		const { outside, onOutside, action } = setup();
		action.destroy();
		down(outside);
		expect(onOutside).not.toHaveBeenCalled();
	});
});
