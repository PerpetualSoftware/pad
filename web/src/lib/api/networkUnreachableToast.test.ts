import { beforeEach, describe, expect, it, vi } from 'vitest';

// Mock the rune-based toast store so this stays a node test, like the busy
// toast's test.
vi.mock('$lib/stores/toast.svelte', () => ({
	toastStore: { show: vi.fn() }
}));

import { toastStore } from '$lib/stores/toast.svelte';
import {
	notifyNetworkUnreachable,
	NETWORK_UNREACHABLE_MESSAGE,
	__resetNetworkUnreachableToastForTest
} from './networkUnreachableToast';

const showMock = toastStore.show as ReturnType<typeof vi.fn>;

describe('notifyNetworkUnreachable (TASK-2202)', () => {
	beforeEach(() => {
		showMock.mockClear();
		__resetNetworkUnreachableToastForTest();
	});

	it('shows one ERROR toast with the unreachable message (an error, so it lingers and is assertive)', () => {
		expect(notifyNetworkUnreachable(0)).toBe(true);
		expect(showMock).toHaveBeenCalledTimes(1);
		expect(showMock).toHaveBeenCalledWith(NETWORK_UNREACHABLE_MESSAGE, 'error');
	});

	it('an outage burst shows one toast, and a later outage shows another', () => {
		notifyNetworkUnreachable(0);
		notifyNetworkUnreachable(100);
		notifyNetworkUnreachable(14_999);
		expect(showMock).toHaveBeenCalledTimes(1);
		expect(notifyNetworkUnreachable(15_000)).toBe(true);
		expect(showMock).toHaveBeenCalledTimes(2);
	});
});
