import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { longPress } from './longPress';

let node: HTMLElement;
let action: ReturnType<typeof longPress>;
let onLong: ReturnType<typeof vi.fn>;
function pointer(type: string, target = node, x = 0) {
	target.dispatchEvent(new PointerEvent(type, { bubbles: true, pointerType: 'touch', clientX: x }));
}
beforeEach(() => {
	vi.useFakeTimers();
	node = document.createElement('article');
	document.body.append(node);
	onLong = vi.fn();
	action = longPress(node, onLong);
});
afterEach(() => {
	action.destroy();
	node.remove();
	window.getSelection()?.removeAllRanges();
	vi.useRealTimers();
});
describe('stationary long press', () => {
	it('fires once at 500ms', () => {
		pointer('pointerdown');
		vi.advanceTimersByTime(499);
		expect(onLong).not.toHaveBeenCalled();
		vi.advanceTimersByTime(1);
		expect(onLong).toHaveBeenCalledTimes(1);
		vi.advanceTimersByTime(500);
		expect(onLong).toHaveBeenCalledTimes(1);
	});
	it.each(['movement', 'scroll', 'pointercancel', 'short tap'])('%s cancels', (kind) => {
		pointer('pointerdown');
		vi.advanceTimersByTime(200);
		if (kind === 'movement') pointer('pointermove', node, 11);
		else if (kind === 'scroll') node.dispatchEvent(new Event('scroll'));
		else pointer(kind === 'short tap' ? 'pointerup' : 'pointercancel');
		vi.advanceTimersByTime(600);
		expect(onLong).not.toHaveBeenCalled();
	});
	it.each(['a', 'button', 'input', 'textarea', 'select'])('does not arm inside %s', (tag) => {
		const control = document.createElement(tag);
		control.innerHTML = tag === 'a' || tag === 'button' ? '<span>text</span>' : '';
		if (tag === 'a') control.setAttribute('href', '/');
		node.append(control);
		pointer('pointerdown', (control.firstElementChild ?? control) as HTMLElement);
		vi.advanceTimersByTime(600);
		expect(onLong).not.toHaveBeenCalled();
	});
	it('does not arm with selected text', () => {
		node.textContent = 'selected text';
		const range = document.createRange();
		range.selectNodeContents(node);
		window.getSelection()!.addRange(range);
		pointer('pointerdown');
		vi.advanceTimersByTime(600);
		expect(onLong).not.toHaveBeenCalled();
	});
	it('honors optional ignore and removes the armed scroll listener on destroy', () => {
		action.destroy();
		const ignore = vi.fn(() => true);
		action = longPress(node, onLong, { ignore });
		pointer('pointerdown');
		vi.advanceTimersByTime(600);
		expect(ignore).toHaveBeenCalled();
		expect(onLong).not.toHaveBeenCalled();
		ignore.mockReturnValue(false);
		pointer('pointerdown');
		action.destroy();
		vi.advanceTimersByTime(600);
		expect(onLong).not.toHaveBeenCalled();
	});
});
