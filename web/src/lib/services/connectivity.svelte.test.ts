// TASK-2201: the one "connection recovered" signal.
import { describe, it, expect, vi, beforeEach } from 'vitest';
import { flushSync } from 'svelte';

const sse = vi.hoisted(() => ({ box: null as null | { status: string } }));
vi.mock('./sse.svelte', async () => {
	const { createStatusBox } = await import('../../test/sseStatusBox.svelte');
	sse.box = createStatusBox();
	return { sseService: sse.box };
});

import { onConnectivityRecovered, signalRecovered, __resetConnectivityForTests, DEDUPE_MS } from './connectivity.svelte';

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
		const heard = vi.fn();
		const off = onConnectivityRecovered(heard);
		// The stream starts `disconnected`: that is not an outage.
		setStatus('disconnected');
		setStatus('connected');
		expect(heard).not.toHaveBeenCalled();
		setStatus('reconnecting');
		setStatus('connected');
		expect(heard).toHaveBeenCalledTimes(1);
		off();
	});

	it('a reconnect into polling (no stream slot) counts as recovered', () => {
		const heard = vi.fn();
		const off = onConnectivityRecovered(heard);
		vi.useFakeTimers();
		vi.setSystemTime(10_000_000);
		setStatus('connected');
		setStatus('disconnected');
		setStatus('polling');
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
