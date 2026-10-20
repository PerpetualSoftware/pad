// TASK-2202 — errors linger, and a toast someone is reading or reaching for
// does not vanish under them. Fake timers, so durations are exact.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { toastStore, ERROR_DURATION, ACTIONABLE_DURATION } from './toast.svelte';
import { announceCaptured } from '$lib/items/quickCapture';

vi.mock('$app/navigation', () => ({ goto: vi.fn() }));

const present = (id: string) => toastStore.toasts.some((t) => t.id === id);

beforeEach(() => {
	vi.useFakeTimers();
	toastStore.clearAll();
});
afterEach(() => {
	toastStore.clearAll();
	vi.useRealTimers();
});

describe('toast durations (TASK-2202)', () => {
	it('an error with no duration stays for ERROR_DURATION, well past the 3s default', () => {
		expect(ERROR_DURATION).toBeGreaterThanOrEqual(8000);
		const id = toastStore.show('Could not save', 'error');
		vi.advanceTimersByTime(3000);
		expect(present(id), 'still up at the old 3s').toBe(true);
		vi.advanceTimersByTime(ERROR_DURATION - 3000 - 1);
		expect(present(id)).toBe(true);
		vi.advanceTimersByTime(1);
		expect(present(id)).toBe(false);
	});

	it('success keeps the 3s default, and an explicit duration still wins for errors', () => {
		const ok = toastStore.show('Saved', 'success');
		const brief = toastStore.show('Brief error', 'error', 2000);
		vi.advanceTimersByTime(2000);
		expect(present(brief)).toBe(false);
		vi.advanceTimersByTime(1000);
		expect(present(ok)).toBe(false);
	});
});

describe('toasts that offer something to do linger (TASK-3973)', () => {
	const act = { label: 'Undo', onAction: () => {} };

	it('a toast with an action and no duration stays for ACTIONABLE_DURATION', () => {
		expect(ACTIONABLE_DURATION).toBeGreaterThanOrEqual(8000);
		const id = toastStore.show('Archived 3 items', 'success', undefined, undefined, act);
		vi.advanceTimersByTime(ACTIONABLE_DURATION - 1);
		expect(present(id)).toBe(true);
		vi.advanceTimersByTime(1);
		expect(present(id)).toBe(false);
	});

	it('a toast with only a link stays for ACTIONABLE_DURATION', () => {
		const id = toastStore.show('Imported DOC-4', 'info', undefined, '/w/docs/doc-4');
		vi.advanceTimersByTime(ACTIONABLE_DURATION - 1);
		expect(present(id)).toBe(true);
		vi.advanceTimersByTime(1);
		expect(present(id)).toBe(false);
	});

	it('an explicit duration still wins over the actionable default', () => {
		const id = toastStore.show('Archived', 'success', 2000, '/w/x', act);
		vi.advanceTimersByTime(2000);
		expect(present(id)).toBe(false);
	});

	it("quick add's Open toast survives 9s", () => {
		announceCaptured({ title: 'Buy milk', collection_prefix: 'TASK', item_number: 12 }, '/w/tasks/task-12');
		const toast = toastStore.toasts.find((t) => t.message === 'Added TASK-12');
		expect(toast?.action?.label).toBe('Open');
		vi.advanceTimersByTime(9000);
		expect(present(toast!.id), 'still up at 9s').toBe(true);
	});
});

describe('toast hold (TASK-2202)', () => {
	it('a paused toast stays, and resumes with only what was left', () => {
		const id = toastStore.show('Saved', 'success', 3000);
		vi.advanceTimersByTime(2000);
		toastStore.pause(id);
		vi.advanceTimersByTime(60_000);
		expect(present(id), 'held while paused').toBe(true);
		toastStore.resume(id);
		vi.advanceTimersByTime(999);
		expect(present(id)).toBe(true);
		vi.advanceTimersByTime(1);
		expect(present(id), 'gone after the remaining 1s').toBe(false);
	});

	it('holds nest: hover ending while focus stays inside keeps it up', () => {
		const id = toastStore.show('Saved', 'success', 3000);
		toastStore.pause(id); // hover
		toastStore.pause(id); // focus
		toastStore.resume(id); // hover ends
		vi.advanceTimersByTime(10_000);
		expect(present(id)).toBe(true);
		toastStore.resume(id); // focus leaves
		vi.advanceTimersByTime(3000);
		expect(present(id)).toBe(false);
	});

	it('an unmatched resume does not start a second clock', () => {
		const id = toastStore.show('Saved', 'success', 3000);
		toastStore.resume(id);
		vi.advanceTimersByTime(3000);
		expect(present(id)).toBe(false);
	});

	it('dismissing a paused toast leaves nothing behind', () => {
		const id = toastStore.show('Saved', 'success', 3000);
		toastStore.pause(id);
		toastStore.dismiss(id);
		toastStore.resume(id);
		vi.advanceTimersByTime(5000);
		expect(present(id)).toBe(false);
		expect(toastStore.toasts.length).toBe(0);
	});
});
