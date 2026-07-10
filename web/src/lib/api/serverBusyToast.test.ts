import { describe, it, expect, vi, beforeEach } from 'vitest';

// Mock the rune-based toast store so this stays a pure node test (no Svelte
// compile). We only assert on how `notifyServerBusy` calls `show`.
vi.mock('$lib/stores/toast.svelte', () => ({
	toastStore: { show: vi.fn() },
}));

import { toastStore } from '$lib/stores/toast.svelte';
import {
	notifyServerBusy,
	serverBusyMessage,
	__resetServerBusyToastForTest,
} from './serverBusyToast';

const showMock = toastStore.show as ReturnType<typeof vi.fn>;

describe('serverBusyMessage (TASK-2080)', () => {
	it('names the wait when a usable Retry-After is present', () => {
		expect(serverBusyMessage(3000)).toBe('Server busy — please try again in 3s.');
		expect(serverBusyMessage(1200)).toBe('Server busy — please try again in 1s.');
	});

	it('stays vague when no (or sub-second) Retry-After is given', () => {
		expect(serverBusyMessage()).toBe('Server busy — please try again in a moment.');
		expect(serverBusyMessage(0)).toBe('Server busy — please try again in a moment.');
		expect(serverBusyMessage(400)).toBe('Server busy — please try again in a moment.');
	});
});

describe('notifyServerBusy dedupe (TASK-2080)', () => {
	beforeEach(() => {
		showMock.mockClear();
		__resetServerBusyToastForTest();
	});

	it('fires exactly one info toast for a single rate_limited notification', () => {
		const shown = notifyServerBusy(undefined, 0);
		expect(shown).toBe(true);
		expect(showMock).toHaveBeenCalledTimes(1);
		// Non-alarming severity: 'info'.
		expect(showMock.mock.calls[0][1]).toBe('info');
	});

	it('does NOT stack toasts for a burst of 429s within the dedupe window', () => {
		// First 429 shows; the next nine (all within the ~6s window) suppress.
		expect(notifyServerBusy(undefined, 0)).toBe(true);
		for (let i = 1; i <= 9; i++) {
			expect(notifyServerBusy(undefined, i * 100)).toBe(false);
		}
		expect(showMock).toHaveBeenCalledTimes(1);
	});

	it('shows again once the dedupe window has elapsed', () => {
		expect(notifyServerBusy(undefined, 0)).toBe(true);
		// Still inside the window — suppressed.
		expect(notifyServerBusy(undefined, 5_000)).toBe(false);
		// Past the window — a fresh toast is allowed.
		expect(notifyServerBusy(undefined, 6_500)).toBe(true);
		expect(showMock).toHaveBeenCalledTimes(2);
	});
});
