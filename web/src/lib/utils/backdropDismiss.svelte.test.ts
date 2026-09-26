import { describe, it, expect, vi, afterEach } from 'vitest';
import { backdropDismiss } from './backdropDismiss';

// BUG-3229: dismiss only when the press AND the release both land on the
// backdrop element itself. A click is dispatched to the nearest common
// ancestor of the two, so a drag between content and backdrop, in either
// direction, produces a click targeting the backdrop that must not dismiss.

function setup(enabled = true) {
	const backdrop = document.body.appendChild(document.createElement('div'));
	const content = backdrop.appendChild(document.createElement('div'));
	const onDismiss = vi.fn();
	const action = backdropDismiss(backdrop, { enabled, onDismiss });
	return { backdrop, content, onDismiss, action };
}

function press(down: Element, up: Element, click: Element) {
	down.dispatchEvent(new Event('pointerdown', { bubbles: true }));
	up.dispatchEvent(new Event('pointerup', { bubbles: true }));
	click.dispatchEvent(new MouseEvent('click', { bubbles: true }));
}

afterEach(() => {
	document.body.innerHTML = '';
});

describe('backdropDismiss (BUG-3229)', () => {
	it('dismisses a press whose down and up both land on the backdrop', () => {
		const { backdrop, onDismiss } = setup();
		press(backdrop, backdrop, backdrop);
		expect(onDismiss).toHaveBeenCalledTimes(1);
	});

	it('does not dismiss a selection dragged from the content onto the backdrop', () => {
		const { backdrop, content, onDismiss } = setup();
		press(content, backdrop, backdrop);
		expect(onDismiss).not.toHaveBeenCalled();
	});

	it('does not dismiss a press dragged from the backdrop into the content', () => {
		const { backdrop, content, onDismiss } = setup();
		press(backdrop, content, backdrop);
		expect(onDismiss).not.toHaveBeenCalled();
	});

	it('does not dismiss a click inside the content', () => {
		const { content, onDismiss } = setup();
		press(content, content, content);
		expect(onDismiss).not.toHaveBeenCalled();
	});

	it('does not dismiss a bare click with no press behind it', () => {
		const { backdrop, onDismiss } = setup();
		backdrop.dispatchEvent(new MouseEvent('click', { bubbles: true }));
		expect(onDismiss).not.toHaveBeenCalled();
	});

	it('reads the press even when the content stops its propagation (capture phase)', () => {
		const { backdrop, content, onDismiss } = setup();
		// A stale backdrop press, then a content press whose handler stops it.
		backdrop.dispatchEvent(new Event('pointerdown', { bubbles: true }));
		content.addEventListener('pointerdown', (e) => e.stopPropagation());
		press(content, backdrop, backdrop);
		expect(onDismiss).not.toHaveBeenCalled();
	});

	it('one pointer on the backdrop does not vouch for another dragged out of the content', () => {
		const { backdrop, content, onDismiss } = setup();
		const ev = (type: string, pointerId: number) => {
			const e = new Event(type, { bubbles: true });
			Object.defineProperty(e, 'pointerId', { value: pointerId });
			return e;
		};
		content.dispatchEvent(ev('pointerdown', 1)); // a selection starts in the content
		backdrop.dispatchEvent(ev('pointerdown', 2)); // a second pointer touches the backdrop
		backdrop.dispatchEvent(ev('pointerup', 1)); // the selection is released on the backdrop
		backdrop.dispatchEvent(new MouseEvent('click', { bubbles: true }));
		expect(onDismiss).not.toHaveBeenCalled();
	});

	it('a cancelled press leaves nothing behind', () => {
		const { backdrop, onDismiss } = setup();
		backdrop.dispatchEvent(new Event('pointerdown', { bubbles: true }));
		backdrop.dispatchEvent(new Event('pointerup', { bubbles: true }));
		backdrop.dispatchEvent(new Event('pointercancel', { bubbles: true }));
		backdrop.dispatchEvent(new MouseEvent('click', { bubbles: true }));
		expect(onDismiss).not.toHaveBeenCalled();
	});

	it('honours enabled=false and a later update', () => {
		const { backdrop, onDismiss, action } = setup(false);
		press(backdrop, backdrop, backdrop);
		expect(onDismiss).not.toHaveBeenCalled();
		action.update({ enabled: true, onDismiss });
		press(backdrop, backdrop, backdrop);
		expect(onDismiss).toHaveBeenCalledTimes(1);
	});

	it('destroy removes every listener', () => {
		const { backdrop, onDismiss, action } = setup();
		action.destroy();
		press(backdrop, backdrop, backdrop);
		expect(onDismiss).not.toHaveBeenCalled();
	});
});
