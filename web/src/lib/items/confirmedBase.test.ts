// BUG-3850: the confirmed base a rich crash draft is anchored on.
import { describe, it, expect } from 'vitest';
import { createConfirmedBaseTracker, providerConfirmed } from './confirmedBase';

function harness() {
	const ctxA = {};
	const ctxB = {};
	const s = { context: ctxA as object | null, confirmed: true, version: 1, markdown: 'one' as string | null, seq: 5, snapshots: 0 };
	const t = createConfirmedBaseTracker({
		context: () => s.context,
		confirmed: () => s.confirmed,
		version: () => s.version,
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
		expect(h.t.current()).toEqual({ markdown: 'one', seq: 5 });
	});

	it('never takes a snapshot while edits may be missing; the last confirmed one stands', () => {
		const h = harness();
		h.t.refresh();
		h.s.confirmed = false;
		h.s.version = 2;
		h.s.markdown = 'one plus unconfirmed typing';
		h.s.seq = 6;
		h.t.refresh();
		expect(h.t.current()).toEqual({ markdown: 'one', seq: 5 });
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
		expect(h.t.current()).toEqual({ markdown: 'two', seq: 9 });
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
		expect(h.t.current()).toEqual({ markdown: 'one', seq: 5 });
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
		expect(h.t.current()).toEqual({ markdown: 'one', seq: 5 });
	});
});
