// TASK-3593 — a burst of creations by one actor is ONE toast, and only other
// actors' events count toward the bell's unread badge. Fake timers, so the
// window and durations are exact.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { toastStore, GROUP_WINDOW_MS } from './toast.svelte';
import { createdToast, type CreatedEntry } from '$lib/items/createdToast';

const WS = '/u/ws';

function created(actor: string, n: number, collection = 'tasks'): string {
	const entry: CreatedEntry = { title: `Item ${n}`, collection, link: `${WS}/${collection}/TASK-${n}` };
	return toastStore.showGrouped(
		`created:ws:${actor}`,
		entry,
		(entries) => createdToast(actor, entries, WS),
		'info',
		4000,
		{ unread: true },
	);
}

beforeEach(() => {
	vi.useFakeTimers();
	toastStore.clearAll();
});
afterEach(() => {
	toastStore.clearAll();
	vi.useRealTimers();
});

describe('grouped creation toasts (TASK-3593)', () => {
	it('ten creations by one agent within the window are one toast, one history entry and one unread', () => {
		for (let i = 1; i <= 10; i++) {
			created('Agent', i);
			vi.advanceTimersByTime(200);
		}
		expect(toastStore.toasts).toHaveLength(1);
		expect(toastStore.toasts[0].message).toBe('Agent created 10 items');
		expect(toastStore.toasts[0].link).toBe(`${WS}/tasks`);
		expect(toastStore.history).toHaveLength(1);
		expect(toastStore.history[0].message).toBe('Agent created 10 items');
		expect(toastStore.unreadCount).toBe(1);
	});

	it('a single creation names the item and links to it', () => {
		created('Agent', 7);
		expect(toastStore.toasts[0].message).toBe('Agent created: Item 7');
		expect(toastStore.toasts[0].link).toBe(`${WS}/tasks/TASK-7`);
	});

	it('a burst across collections links to the workspace', () => {
		created('Agent', 1, 'tasks');
		created('Agent', 2, 'bugs');
		expect(toastStore.toasts[0].link).toBe(WS);
	});

	it('different actors get their own toasts', () => {
		created('Agent', 1);
		created('Dana', 2);
		expect(toastStore.toasts.map((t) => t.message)).toEqual(['Agent created: Item 1', 'Dana created: Item 2']);
		expect(toastStore.unreadCount).toBe(2);
	});

	it('each event in the burst restarts the toast clock', () => {
		const id = created('Agent', 1);
		vi.advanceTimersByTime(3000);
		created('Agent', 2);
		vi.advanceTimersByTime(3000);
		expect(toastStore.toasts.some((t) => t.id === id), 'expired 4s after the FIRST event').toBe(true);
		vi.advanceTimersByTime(1000);
		expect(toastStore.toasts.some((t) => t.id === id)).toBe(false);
	});

	it('an event after the toast has gone starts a new burst', () => {
		created('Agent', 1);
		vi.advanceTimersByTime(4000);
		expect(toastStore.toasts).toHaveLength(0);
		created('Agent', 2);
		expect(toastStore.toasts).toHaveLength(1);
		expect(toastStore.toasts[0].message).toBe('Agent created: Item 2');
		expect(toastStore.unreadCount).toBe(2);
	});

	it('an event past the window starts a new burst even if the toast is held on screen', () => {
		const id = created('Agent', 1);
		toastStore.pause(id);
		vi.advanceTimersByTime(GROUP_WINDOW_MS + 1);
		created('Agent', 2);
		expect(toastStore.toasts).toHaveLength(2);
	});
});

describe('unread counts only other actors (TASK-3593)', () => {
	it("the user's own confirmation does not bump unread", () => {
		toastStore.show('Saved', 'success');
		toastStore.show('Could not save', 'error');
		expect(toastStore.history).toHaveLength(2);
		expect(toastStore.unreadCount).toBe(0);
	});

	it('an event marked unread does', () => {
		toastStore.show('Dana commented', 'info', undefined, undefined, undefined, { unread: true });
		expect(toastStore.unreadCount).toBe(1);
	});
});
