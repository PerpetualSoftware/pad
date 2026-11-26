import { afterEach, beforeEach, expect, it, vi } from 'vitest';

// DOC-4851 section 5: home_changed hints are coalesced before anything refetches.
const h = vi.hoisted(() => ({ resets: [] as Array<() => void> }));
vi.mock('$lib/stores/auth.svelte', () => ({
	authStore: { onIdentityChange: (reset: () => void) => { h.resets.push(reset); return () => {}; } }
}));
import { homeLive, HOME_LIVE_DEBOUNCE_MS, HOME_LIVE_MIN_INTERVAL_MS } from './homeLive';

beforeEach(() => { vi.useFakeTimers(); homeLive.reset(); });
afterEach(() => { homeLive.reset(); vi.useRealTimers(); });

it('five hints in a burst cause one load, after the trailing debounce', () => {
	const load = vi.fn();
	homeLive.register(load);
	for (let i = 0; i < 5; i++) { homeLive.hint(); vi.advanceTimersByTime(100); }
	expect(load).not.toHaveBeenCalled();
	vi.advanceTimersByTime(HOME_LIVE_DEBOUNCE_MS);
	expect(load).toHaveBeenCalledTimes(1);
	vi.advanceTimersByTime(10_000);
	expect(load).toHaveBeenCalledTimes(1);
});

it('refetches at most once per interval, with one trailing refetch for hints inside it', () => {
	const load = vi.fn();
	homeLive.register(load);
	homeLive.hint(); vi.advanceTimersByTime(HOME_LIVE_DEBOUNCE_MS);
	expect(load).toHaveBeenCalledTimes(1);
	homeLive.hint(); vi.advanceTimersByTime(HOME_LIVE_DEBOUNCE_MS);
	homeLive.hint(); vi.advanceTimersByTime(HOME_LIVE_DEBOUNCE_MS);
	expect(load).toHaveBeenCalledTimes(1);
	vi.advanceTimersByTime(HOME_LIVE_MIN_INTERVAL_MS);
	expect(load).toHaveBeenCalledTimes(2);
});

it('calls every registered consumer and none after it unregisters', () => {
	const a = vi.fn(); const b = vi.fn();
	homeLive.register(a);
	const offB = homeLive.register(b);
	homeLive.hint(); vi.advanceTimersByTime(HOME_LIVE_DEBOUNCE_MS);
	expect(a).toHaveBeenCalledTimes(1); expect(b).toHaveBeenCalledTimes(1);
	offB();
	vi.advanceTimersByTime(HOME_LIVE_MIN_INTERVAL_MS);
	homeLive.hint(); vi.advanceTimersByTime(HOME_LIVE_DEBOUNCE_MS);
	expect(a).toHaveBeenCalledTimes(2); expect(b).toHaveBeenCalledTimes(1);
});

it('an identity change drops a pending hint', () => {
	const load = vi.fn();
	homeLive.register(load);
	homeLive.hint();
	for (const reset of h.resets) reset();
	vi.advanceTimersByTime(10_000);
	expect(load).not.toHaveBeenCalled();
});
