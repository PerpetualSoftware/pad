// A touch long-press (TASK-4044 PR 3b): `onLong` fires once the pointer has been
// down 500 ms without moving or ending. A short tap does nothing, and a mouse
// never long-presses (it gets the hover menu).

export const LONG_PRESS_MS = 500;
const MOVE_SLOP_PX = 10;

export function longPress(node: HTMLElement, onLong: () => void) {
	let timer: ReturnType<typeof setTimeout> | null = null;
	let x = 0;
	let y = 0;
	const cancel = () => {
		if (timer !== null) clearTimeout(timer);
		timer = null;
	};
	const down = (e: PointerEvent) => {
		if (e.pointerType === 'mouse') return;
		cancel();
		x = e.clientX;
		y = e.clientY;
		timer = setTimeout(() => {
			timer = null;
			onLong();
		}, LONG_PRESS_MS);
	};
	const move = (e: PointerEvent) => {
		if (timer !== null && Math.hypot(e.clientX - x, e.clientY - y) > MOVE_SLOP_PX) cancel();
	};
	// The OS may raise its own callout / context menu on the same press (r5 H4);
	// while a press is armed that would sit on top of ours.
	const ctx = (e: Event) => {
		if (timer !== null) e.preventDefault();
	};
	node.addEventListener('pointerdown', down);
	node.addEventListener('contextmenu', ctx);
	node.addEventListener('pointermove', move);
	node.addEventListener('pointerup', cancel);
	node.addEventListener('pointercancel', cancel);
	return {
		update(next: () => void) {
			onLong = next;
		},
		destroy() {
			cancel();
			node.removeEventListener('pointerdown', down);
			node.removeEventListener('contextmenu', ctx);
			node.removeEventListener('pointermove', move);
			node.removeEventListener('pointerup', cancel);
			node.removeEventListener('pointercancel', cancel);
		}
	};
}
