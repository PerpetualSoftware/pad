// TASK-3593 — a burst of creations by one actor is ONE toast, and only other
// actors' events count toward the bell's unread badge. Fake timers, so the
// window and durations are exact.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { toastStore, GROUP_WINDOW_MS, __groupCountForTests } from './toast.svelte';
import { createdToast, foldCreated, type CreatedEntry } from '$lib/items/createdToast';

const WS = '/u/ws';

function created(actor: string, n: number, collection = 'tasks'): string {
	const entry: CreatedEntry = { title: `Item ${n}`, collection, link: `${WS}/${collection}/TASK-${n}` };
	return toastStore.showGrouped(
		`created:ws:${actor}`,
		entry,
		foldCreated,
		(burst) => createdToast(actor, burst, WS),
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

describe('grouped toasts hold bounded state (TASK-3593 codex round 1)', () => {
	it('a long burst folds into a summary, not a list of every event', () => {
		let calls = 0;
		const seen: number[] = [];
		for (let i = 1; i <= 50; i++) {
			toastStore.showGrouped(
				'burst',
				i,
				(acc: number | undefined, n: number) => {
					calls++;
					return (acc ?? 0) + n;
				},
				(sum) => {
					seen.push(sum);
					return { message: `sum ${sum}` };
				},
			);
		}
		// One fold and one render per event: no event is re-read later.
		expect(calls).toBe(50);
		expect(seen).toHaveLength(50);
		expect(toastStore.toasts[0].message).toBe('sum 1275');
	});

	it('a dismissed toast ends its burst: the next event starts a new toast', () => {
		const id = created('Agent', 1);
		created('Agent', 2);
		toastStore.dismiss(id);
		created('Agent', 3);
		expect(toastStore.toasts).toHaveLength(1);
		expect(toastStore.toasts[0].message).toBe('Agent created: Item 3');
	});

	it('a toast that leaves (dismissed, expired or evicted) leaves no group behind', () => {
		const a = created('Agent', 1);
		created('Dana', 2);
		expect(__groupCountForTests()).toBe(2);
		toastStore.dismiss(a);
		expect(__groupCountForTests(), 'dismissed').toBe(1);
		vi.advanceTimersByTime(4000);
		expect(__groupCountForTests(), 'expired').toBe(0);
		created('Eve', 3);
		for (let i = 0; i < 5; i++) toastStore.show(`other ${i}`, 'success');
		expect(toastStore.toasts.some((t) => t.message.startsWith('Eve')), 'PREMISE: evicted').toBe(false);
		expect(__groupCountForTests(), 'evicted').toBe(0);
	});

	it('an updated burst moves to the front of the history (codex r3)', () => {
		created('Agent', 1);
		vi.advanceTimersByTime(100);
		toastStore.show('Saved', 'success');
		vi.advanceTimersByTime(100);
		created('Agent', 2);
		expect(toastStore.history.map((h) => h.message)).toEqual(['Agent created 2 items', 'Saved']);
		expect(toastStore.history[0].timestamp).toBeGreaterThanOrEqual(toastStore.history[1].timestamp);
	});

	it('the folded summary tracks one collection, then mixed', () => {
		let b = foldCreated(undefined, { title: 'a', collection: 'tasks' });
		b = foldCreated(b, { title: 'b', collection: 'tasks' });
		expect(b).toMatchObject({ count: 2, collection: 'tasks' });
		b = foldCreated(b, { title: 'c', collection: 'bugs' });
		b = foldCreated(b, { title: 'd', collection: 'tasks' });
		expect(b).toMatchObject({ count: 4, collection: null });
	});
});

describe('unread counts only other actors (TASK-3593)', () => {
	it("a success or info confirmation does not bump unread", () => {
		toastStore.show('Saved', 'success');
		toastStore.show('Copied link', 'info');
		expect(toastStore.history).toHaveLength(2);
		expect(toastStore.unreadCount).toBe(0);
	});

	it('an error counts without the flag, whoever caused it', () => {
		// e.g. a debounced autosave failing in the background, or the user's
		// own save refused: neither is a confirmation.
		toastStore.show('Failed to save content', 'error');
		expect(toastStore.unreadCount).toBe(1);
	});

	it('an event marked unread does', () => {
		toastStore.show('Dana commented', 'info', undefined, undefined, undefined, { unread: true });
		expect(toastStore.unreadCount).toBe(1);
	});

	it('a non-error background notice counts with showUnread', () => {
		toastStore.showUnread('Server busy — please try again shortly', 'info');
		expect(toastStore.unreadCount).toBe(1);
	});
});
