import { afterEach, describe, expect, it } from 'vitest';
import { autofocus } from './autofocus';

// TASK-2259: the action focuses on mount, never steals focus the person
// already placed, and can be switched off so a page picks its first field.
describe('autofocus', () => {
	afterEach(() => {
		document.body.innerHTML = '';
	});

	function input(): HTMLInputElement {
		const el = document.createElement('input');
		document.body.appendChild(el);
		return el;
	}

	it('focuses the node when nothing else has focus', () => {
		const el = input();
		autofocus(el);
		expect(document.activeElement).toBe(el);
	});

	it('does not take focus from a field the person already focused', () => {
		const other = input();
		other.focus();
		const el = input();
		autofocus(el);
		expect(document.activeElement).toBe(other);
	});

	it('does nothing when disabled', () => {
		const el = input();
		autofocus(el, false);
		expect(document.activeElement).toBe(document.body);
	});
});
