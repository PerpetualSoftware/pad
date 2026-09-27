import { dndzone, DRAGGED_ELEMENT_ID, type DndEvent, type Item as DndItem, type Options } from 'svelte-dnd-action';
import type { ActionReturn } from 'svelte/action';

/**
 * `dndzone` with per-card locking (BUG-3259).
 *
 * svelte-dnd-action disables drag per ZONE only. A zone child (the element
 * rendered for one item) carrying `data-drag-locked="true"` cannot start a
 * drag here, by pointer or by keyboard, while its unlocked siblings still can.
 * The board and list lock a card the caller may only view, which an item grant
 * makes possible inside a collection the caller may edit.
 *
 * Mechanism: the library listens for mousedown / touchstart / keydown on each
 * zone child. A capture-phase listener on the zone runs before those, and when
 * the gesture starts on a locked child it reconfigures the zone as disabled
 * for that one event, synchronously, which removes the library's listeners
 * from every child. The event then reaches the child with no drag listener on
 * it, and the zone is re-enabled on the next task. This is the library's own
 * drag-handle mechanism (`dragHandleZone`), scoped to this zone instead of a
 * module-wide store.
 *
 * The gesture then stops at this zone, in the bubble phase, exactly where the
 * library stops it for an unlocked card (its handlers call stopPropagation).
 * Without that, a zone nested in another zone hands the gesture to the outer
 * one: on the list, pressing a locked row dragged its whole GROUP (measured).
 * Listeners above the zone see what they already saw for an unlocked card.
 */
export function lockableDndzone<T extends DndItem>(
	node: HTMLElement,
	options: Options<T>
): ActionReturn<Options<T>, { onconsider?: (e: CustomEvent<DndEvent<T>>) => void; onfinalize?: (e: CustomEvent<DndEvent<T>>) => void }> {
	let current = options;
	let suspended = false;
	let destroyed = false;
	const zone = dndzone(node, current);

	function lockedChild(target: EventTarget | null): boolean {
		let el = target instanceof Element ? target : null;
		while (el && el.parentElement !== node) el = el.parentElement;
		return el instanceof HTMLElement && el.dataset.dragLocked === 'true';
	}

	// A gesture this zone would have started on an unlocked card: a pointer
	// press, or a key that starts a keyboard drag (the library's defaults), on
	// a locked child, with no pointer drag in progress (reconfiguring mid-drag
	// would disturb its shadow item).
	function lockedGesture(e: Event): boolean {
		if (current.dragDisabled) return false;
		if (e instanceof KeyboardEvent && e.key !== 'Enter' && e.key !== ' ') return false;
		if (document.getElementById(DRAGGED_ELEMENT_ID)) return false;
		return lockedChild(e.target);
	}

	function stopAtZone(e: Event) {
		if (lockedGesture(e)) e.stopPropagation();
	}

	function gate(e: Event) {
		if (suspended || !lockedGesture(e)) return;
		suspended = true;
		zone.update?.({ ...current, dragDisabled: true });
		setTimeout(() => {
			suspended = false;
			if (!destroyed) zone.update?.(current);
		}, 0);
	}

	node.addEventListener('mousedown', gate, true);
	node.addEventListener('touchstart', gate, { capture: true, passive: true });
	node.addEventListener('keydown', gate, true);
	node.addEventListener('mousedown', stopAtZone);
	node.addEventListener('touchstart', stopAtZone, { passive: true });
	node.addEventListener('keydown', stopAtZone);

	return {
		update(next: Options<T>) {
			current = next;
			if (!suspended) zone.update?.(current);
		},
		destroy() {
			destroyed = true;
			node.removeEventListener('mousedown', gate, true);
			node.removeEventListener('touchstart', gate, true);
			node.removeEventListener('keydown', gate, true);
			node.removeEventListener('mousedown', stopAtZone);
			node.removeEventListener('touchstart', stopAtZone);
			node.removeEventListener('keydown', stopAtZone);
			zone.destroy?.();
		}
	};
}
