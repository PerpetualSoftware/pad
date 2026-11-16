import { fireEvent } from '@testing-library/svelte';

/** Explicit timestamps keep velocity tests independent of machine speed. */
export function stripPointer(target: Element, type: string, x: number, y = 100, time = 0, pointerType = 'touch', pointerId = 1) {
	const event = new MouseEvent(type, { bubbles: true, cancelable: true, clientX: x, clientY: y });
	Object.defineProperties(event, {
		pointerType: { value: pointerType }, pointerId: { value: pointerId }, timeStamp: { value: time }
	});
	return fireEvent(target, event);
}

export async function swipeStrip(target: Element, dx = -80, dy = 0, duration = 300, start = 200, pointerType = 'touch') {
	await stripPointer(target, 'pointerdown', start, 100, 0, pointerType);
	await stripPointer(target, 'pointermove', start + dx, 100 + dy, duration, pointerType);
	await stripPointer(target, 'pointerup', start + dx, 100 + dy, duration + 10, pointerType);
}
