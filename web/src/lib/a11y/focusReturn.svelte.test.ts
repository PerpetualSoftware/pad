// TASK-2235: the focus bookkeeping Modal and BottomSheet each kept, shared.
import { describe, it, expect, afterEach } from 'vitest';
import { createFocusReturn } from './focusReturn';

function button(id: string): HTMLButtonElement {
	const b = document.createElement('button');
	b.id = id;
	document.body.appendChild(b);
	return b;
}

afterEach(() => {
	document.body.innerHTML = '';
});

describe('createFocusReturn (TASK-2235)', () => {
	it('returns focus to the element focused at save()', () => {
		const trigger = button('trigger');
		const inside = button('inside');
		trigger.focus();
		const fr = createFocusReturn();
		fr.save();
		inside.focus();
		fr.restore();
		expect(document.activeElement).toBe(trigger);
	});

	it('keeps the FIRST capture: a re-run while open cannot overwrite the trigger', () => {
		const trigger = button('trigger');
		const inside = button('inside');
		trigger.focus();
		const fr = createFocusReturn();
		fr.save();
		inside.focus();
		fr.save(); // an effect re-running while the surface is open
		fr.restore();
		expect(document.activeElement).toBe(trigger);
	});

	it('captures afresh after a restore', () => {
		const a = button('a');
		const b = button('b');
		const fr = createFocusReturn();
		a.focus();
		fr.save();
		fr.restore();
		b.focus();
		fr.save();
		a.focus();
		fr.restore();
		expect(document.activeElement).toBe(b);
	});

	it('does nothing when the remembered element has left the document', () => {
		const trigger = button('trigger');
		const other = button('other');
		trigger.focus();
		const fr = createFocusReturn();
		fr.save();
		other.focus();
		trigger.remove();
		fr.restore();
		expect(document.activeElement).toBe(other);
	});

	it('restore without a save is a no-op', () => {
		const other = button('other');
		other.focus();
		createFocusReturn().restore();
		expect(document.activeElement).toBe(other);
	});
});
