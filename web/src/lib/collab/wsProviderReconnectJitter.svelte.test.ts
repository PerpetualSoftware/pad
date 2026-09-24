import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import * as Y from 'yjs';
import { CollabProvider } from './wsProvider.svelte';

// BUG-1308: a refused collab dial (the per-user dial bucket or socket cap
// answers 429) reaches the browser as a handshake that never opens: 'error'
// then 'close', with no status the page can read. The provider must keep
// retrying, and each tab must pick its own delay, so a bounce's herd of
// refused tabs spreads out instead of re-dialling on the same marks.

class FakeSocket extends EventTarget {
	static all: FakeSocket[] = [];
	binaryType = 'arraybuffer';
	readyState = 0;
	constructor(public url: string) {
		super();
		FakeSocket.all.push(this);
	}
	send() {}
	close() {
		this.readyState = 3;
	}
	/** A handshake the server refused: error, then close, never open. */
	refuse() {
		this.readyState = 3;
		this.dispatchEvent(new Event('error'));
		this.dispatchEvent(new CloseEvent('close', { code: 1006 }));
	}
}

function makeProvider(random: () => number) {
	return new CollabProvider('item-1308', new Y.Doc(), {
		url: 'ws://test.invalid/api/v1/collab/item-1308',
		WebSocketImpl: FakeSocket as unknown as typeof WebSocket,
		random,
	});
}

/** Milliseconds from a refusal until the provider dials again. */
function msUntilNextDial(maxMs: number): number {
	const before = FakeSocket.all.length;
	for (let t = 1; t <= maxMs; t++) {
		vi.advanceTimersByTime(1);
		if (FakeSocket.all.length > before) return t;
	}
	return -1;
}

describe('CollabProvider reconnect after a refused handshake (BUG-1308)', () => {
	beforeEach(() => {
		vi.useFakeTimers();
		FakeSocket.all = [];
	});
	afterEach(() => {
		vi.useRealTimers();
	});

	it('waits between half and all of each backoff step, as the random source picks', () => {
		const low = makeProvider(() => 0);
		FakeSocket.all.at(-1)!.refuse();
		expect(msUntilNextDial(2000)).toBe(500); // step 1000, floor half
		FakeSocket.all.at(-1)!.refuse();
		expect(msUntilNextDial(4000)).toBe(1000); // step 2000
		low.destroy();

		const mid = makeProvider(() => 0.5);
		FakeSocket.all.at(-1)!.refuse();
		expect(msUntilNextDial(2000)).toBe(750); // 500 + 0.5 * 500
		mid.destroy();
	});

	it('two tabs refused in the same instant do not re-dial in the same instant', () => {
		const a = makeProvider(() => 0.1);
		const sockA = FakeSocket.all.at(-1)!;
		const b = makeProvider(() => 0.9);
		const sockB = FakeSocket.all.at(-1)!;
		const born = FakeSocket.all.length;
		sockA.refuse();
		sockB.refuse();
		const times: number[] = [];
		for (let t = 1; t <= 1000 && times.length < 2; t++) {
			const seen = FakeSocket.all.length;
			vi.advanceTimersByTime(1);
			for (let i = seen; i < FakeSocket.all.length; i++) times.push(t);
		}
		expect(FakeSocket.all.length - born).toBe(2);
		expect(times).toHaveLength(2);
		expect(times[1] - times[0]).toBeGreaterThanOrEqual(300); // 550 vs 950
		a.destroy();
		b.destroy();
	});

	it('keeps retrying past the offline threshold, so a refused editor is never left dead', () => {
		const p = makeProvider(() => 0.5);
		for (let i = 0; i < 6; i++) {
			FakeSocket.all.at(-1)!.refuse();
			expect(msUntilNextDial(31_000)).toBeGreaterThan(0);
		}
		expect(p.state).toBe('offline'); // honest about it, and still dialling
		p.destroy();
	});
});
