// TASK-2201: the one "connection recovered" signal.
import { describe, it, expect, vi, beforeEach } from 'vitest';
import { flushSync } from 'svelte';

const sse = vi.hoisted(() => ({ box: null as null | { status: string } }));
vi.mock('./sse.svelte', async () => {
	const { createStatusBox } = await import('../../test/sseStatusBox.svelte');
	sse.box = createStatusBox();
	return { sseService: sse.box };
});

import { onConnectivityRecovered, signalRecovered, __resetConnectivityForTests, DEDUPE_MS, JITTER_MS } from './connectivity.svelte';

function setStatus(s: string) {
	sse.box!.status = s;
	flushSync();
}

beforeEach(() => {
	__resetConnectivityForTests();
	vi.useRealTimers();
});

describe('onConnectivityRecovered', () => {
	it('the first connect after load is not a recovery; a drop and reconnect is', () => {
		vi.useFakeTimers();
		vi.setSystemTime(5_000_000);
		const heard = vi.fn();
		const off = onConnectivityRecovered(heard);
		// The stream starts `disconnected`: that is not an outage.
		setStatus('disconnected');
		setStatus('connected');
		vi.advanceTimersByTime(JITTER_MS);
		expect(heard).not.toHaveBeenCalled();
		setStatus('reconnecting');
		setStatus('connected');
		vi.advanceTimersByTime(JITTER_MS);
		expect(heard).toHaveBeenCalledTimes(1);
		off();
	});

	it('a reconnect into polling (no stream slot) counts as recovered', () => {
		vi.useFakeTimers();
		vi.setSystemTime(10_000_000);
		const heard = vi.fn();
		const off = onConnectivityRecovered(heard);
		setStatus('connected');
		setStatus('disconnected');
		setStatus('polling');
		vi.advanceTimersByTime(JITTER_MS);
		expect(heard).toHaveBeenCalledTimes(1);
		off();
	});

	it('a STREAM recovery waits a random 0..JITTER_MS (herd control); the draw sets the delay', () => {
		vi.useFakeTimers();
		vi.setSystemTime(30_000_000);
		__resetConnectivityForTests(() => 0.6); // 0.6 * 5000 = 3000 ms
		const heard = vi.fn();
		const off = onConnectivityRecovered(heard);
		setStatus('connected');
		setStatus('reconnecting');
		setStatus('connected');
		vi.advanceTimersByTime(2999);
		expect(heard).not.toHaveBeenCalled();
		vi.advanceTimersByTime(1);
		expect(heard).toHaveBeenCalledTimes(1);
		off();
	});

	it('an `online` during the jitter wait is the same recovery: immediate, and the stream signal is cancelled', () => {
		vi.useFakeTimers();
		vi.setSystemTime(40_000_000);
		__resetConnectivityForTests(() => 0.99);
		const heard = vi.fn();
		const off = onConnectivityRecovered(heard);
		setStatus('connected');
		setStatus('reconnecting');
		setStatus('connected');
		window.dispatchEvent(new Event('online'));
		expect(heard).toHaveBeenCalledTimes(1);
		vi.advanceTimersByTime(JITTER_MS * 2);
		expect(heard).toHaveBeenCalledTimes(1);
		off();
	});

	it('the window coming online is a recovery', () => {
		const heard = vi.fn();
		const off = onConnectivityRecovered(heard);
		vi.useFakeTimers();
		vi.setSystemTime(20_000_000);
		window.dispatchEvent(new Event('online'));
		expect(heard).toHaveBeenCalledTimes(1);
		off();
	});

	it('two triggers of one recovery notify once; a later recovery notifies again', () => {
		const heard = vi.fn();
		const off = onConnectivityRecovered(heard);
		signalRecovered(1_000);
		signalRecovered(1_000 + DEDUPE_MS - 1);
		expect(heard).toHaveBeenCalledTimes(1);
		signalRecovered(1_000 + DEDUPE_MS + 1);
		expect(heard).toHaveBeenCalledTimes(2);
		off();
	});

	it('an unsubscribed listener hears nothing, and one throwing listener does not stop the rest', () => {
		const gone = vi.fn();
		const thrower = vi.fn(() => {
			throw new Error('boom');
		});
		const kept = vi.fn();
		onConnectivityRecovered(gone)();
		const a = onConnectivityRecovered(thrower);
		const b = onConnectivityRecovered(kept);
		signalRecovered(50_000_000);
		expect(gone).not.toHaveBeenCalled();
		expect(kept).toHaveBeenCalledTimes(1);
		a();
		b();
	});
});
