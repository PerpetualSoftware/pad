// BUG-3850: the confirmed base a rich crash draft is anchored on.
import { describe, it, expect } from 'vitest';
import { createConfirmedBaseTracker, providerConfirmed, restoreWaitsForEditor } from './confirmedBase';

function harness() {
	const ctxA = {};
	const ctxB = {};
	const s = { context: ctxA as object | null, confirmed: true, version: 1, remote: 0, markdown: 'one' as string | null, seq: 5, snapshots: 0 };
	const t = createConfirmedBaseTracker({
		context: () => s.context,
		confirmed: () => s.confirmed,
		version: () => s.version,
		remoteVersion: () => s.remote,
		snapshot: () => {
			s.snapshots++;
			return s.markdown;
		},
		seq: () => s.seq,
	});
	return { s, t, ctxA, ctxB };
}

describe('providerConfirmed (BUG-3850)', () => {
	const ok = { synced: true, editsMayBeMissing: false, editsAtRiskOnClose: false };
	it('is true only when synced with nothing unconfirmed', () => {
		expect(providerConfirmed(ok)).toBe(true);
		expect(providerConfirmed(null)).toBe(false);
		expect(providerConfirmed({ ...ok, synced: false })).toBe(false);
		expect(providerConfirmed({ ...ok, editsAtRiskOnClose: true })).toBe(false);
	});

	it('a send still awaiting its barrier is NOT confirmed: a base taken then could hold just-typed text', () => {
		// editsAtRiskOnClose leaves the barrier wait out on purpose; the base must not.
		expect(providerConfirmed({ ...ok, editsMayBeMissing: true, editsAtRiskOnClose: false })).toBe(false);
	});
});

describe('confirmed base tracker (BUG-3850)', () => {
	it('takes the snapshot and its seq while confirmed', () => {
		const h = harness();
		h.t.refresh();
		expect(h.t.current()).toEqual({ markdown: 'one', seq: 5, remoteAfter: false });
	});

	it('never takes a snapshot while edits may be missing; the last confirmed one stands', () => {
		const h = harness();
		h.t.refresh();
		h.s.confirmed = false;
		h.s.version = 2;
		h.s.markdown = 'one plus unconfirmed typing';
		h.s.seq = 6;
		h.t.refresh();
		expect(h.t.current()).toEqual({ markdown: 'one', seq: 5, remoteAfter: false });
		expect(h.s.snapshots).toBe(1);
	});

	it('with no confirmed moment yet there is no base', () => {
		const h = harness();
		h.s.confirmed = false;
		h.t.refresh();
		expect(h.t.current()).toBeNull();
		expect(h.s.snapshots).toBe(0);
	});

	it('an unmoved document is not re-serialized; a moved one is, with the seq at that moment', () => {
		const h = harness();
		h.t.refresh();
		h.t.refresh();
		expect(h.s.snapshots).toBe(1);
		h.s.version = 2;
		h.s.markdown = 'two';
		h.s.seq = 9;
		h.t.refresh();
		expect(h.s.snapshots).toBe(2);
		expect(h.t.current()).toEqual({ markdown: 'two', seq: 9, remoteAfter: false });
	});

	it('the seq is the one at capture, not a later one', () => {
		const h = harness();
		h.t.refresh();
		h.s.seq = 12;
		h.s.confirmed = false;
		h.t.refresh();
		expect(h.t.current()?.seq).toBe(5);
	});

	it("another context's snapshot is never offered as this one's base", () => {
		const h = harness();
		h.t.refresh();
		h.s.context = h.ctxB;
		expect(h.t.current()).toBeNull();
		h.s.confirmed = false;
		h.t.refresh();
		expect(h.t.current()).toBeNull();
		h.s.confirmed = true;
		h.t.refresh();
		expect(h.t.current()).toEqual({ markdown: 'one', seq: 5, remoteAfter: false });
	});

	it('no context drops the held snapshot', () => {
		const h = harness();
		h.t.refresh();
		h.s.context = null;
		h.t.refresh();
		h.s.context = h.ctxA;
		expect(h.t.current()).toBeNull();
	});

	it('an unavailable snapshot keeps the previous base', () => {
		const h = harness();
		h.t.refresh();
		h.s.version = 2;
		h.s.markdown = null;
		h.t.refresh();
		expect(h.t.current()).toEqual({ markdown: 'one', seq: 5, remoteAfter: false });
	});

	it('the seq is the one read with the snapshot, not a later one', () => {
		const h = harness();
		h.t.refresh();
		h.s.seq = 8;
		expect(h.t.current()).toEqual({ markdown: 'one', seq: 5, remoteAfter: false });
	});
});

// Lead ruling 0337bf77: the patch restore waits for this tab's open editor.
describe('restoreWaitsForEditor (BUG-3850)', () => {
	it('runs only with the editor mounted on a CURRENT connection that is synced', () => {
		expect(restoreWaitsForEditor({ state: 'synced' }, true)).toBe(false);
		for (const state of ['connecting', 'reconnecting', 'offline']) expect(restoreWaitsForEditor({ state }, true), state).toBe(true);
		expect(restoreWaitsForEditor({ state: 'synced' }, false), 'editor not mounted').toBe(true);
		expect(restoreWaitsForEditor(null, true), 'no provider').toBe(true);
		expect(restoreWaitsForEditor(undefined, true)).toBe(true);
	});
});

