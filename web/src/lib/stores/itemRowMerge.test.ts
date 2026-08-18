import { describe, it, expect } from 'vitest';
import {
	resolveRowWrite,
	preserveProjectionMetadata,
	mergeEqualSeqProjection,
	type Tombstone,
} from './itemRowMerge';
import type { ItemIndexRow } from '$lib/types';

/**
 * PLAN-2636 unit 2 — the pure write-decision + projection-merge logic shared by
 * the in-RAM localIndex store and the IDB persistence layer. `resolveRowWrite`
 * replaces the boolean `shouldWriteRow` at both persistence call sites and adds
 * the tombstone gate (BUG-2633) and the equal-seq MERGE (BUG-2635); this file
 * pins the DECISION. The interleaved-in-a-real-transaction OUTCOME is pinned by
 * the `.idb.test.ts` siblings, which run against fake-indexeddb.
 */

function row(seq: number | undefined, extra: Record<string, unknown> = {}, id = 'item-1'): ItemIndexRow {
	return { id, seq, ...extra } as unknown as ItemIndexRow;
}

function tomb(deletedAtSeq: number, id = 'item-1'): Tombstone {
	return { id, deletedAtSeq };
}

function decide(
	stored: ItemIndexRow | undefined,
	tombstone: Tombstone | undefined,
	incoming: ItemIndexRow,
) {
	return resolveRowWrite(stored, tombstone, incoming);
}

describe('resolveRowWrite — seq guard (the BUG-2609 policy, unchanged)', () => {
	it('writes when nothing is stored yet', () => {
		expect(decide(undefined, undefined, row(5))).toEqual({ action: 'write', row: row(5) });
	});

	it('writes over a stored row that carries no seq', () => {
		// Stored has no ordering evidence — the incoming row wins regardless.
		expect(decide(row(undefined), undefined, row(7)).action).toBe('write');
		expect(decide(row(undefined), undefined, row(undefined)).action).toBe('write');
	});

	it('REFUSES a strictly older row', () => {
		expect(decide(row(7), undefined, row(6))).toEqual({ action: 'skip' });
		expect(decide(row(7), undefined, row(0))).toEqual({ action: 'skip' });
	});

	it('writes a strictly newer row', () => {
		expect(decide(row(6), undefined, row(7)).action).toBe('write');
	});

	it('REFUSES a seq-less row over a stamped one (the asymmetry)', () => {
		// The optimistic-reorder path clears seq to bypass the RAM guard; persisting
		// it over an authoritative stamped row would strand an unorderable cache row.
		expect(decide(row(7), undefined, row(undefined))).toEqual({ action: 'skip' });
		expect(decide(row(0), undefined, row(undefined))).toEqual({ action: 'skip' });
	});

	it('treats seq 0 as a value, not as missing', () => {
		expect(decide(row(0), undefined, row(1)).action).toBe('write');
		expect(decide(row(1), undefined, row(0))).toEqual({ action: 'skip' });
	});
});

describe('resolveRowWrite — equal seq MERGE (BUG-2635, replaces blind-accept)', () => {
	it('writes the MERGED row when the incoming contributes a differing projection bit', () => {
		const stored = row(7, { is_unparented: false });
		const incoming = row(7, { is_unparented: true });
		const d = decide(stored, undefined, incoming);
		expect(d.action).toBe('write');
		expect(d.action === 'write' && d.row.is_unparented).toBe(true);
	});

	it('SKIPS at equal seq when the incoming contributes nothing (no projection bit)', () => {
		// A second tab's unmerged snapshot at the same seq must not clobber the
		// merged row — the residual BUG-2635 closed by merging instead of accepting.
		const stored = row(7, { is_unparented: true });
		const incoming = row(7); // omits is_unparented
		expect(decide(stored, undefined, incoming)).toEqual({ action: 'skip' });
	});

	it('SKIPS at equal seq when the projection bit matches (nothing to contribute)', () => {
		const stored = row(7, { is_unparented: true });
		const incoming = row(7, { is_unparented: true });
		expect(decide(stored, undefined, incoming)).toEqual({ action: 'skip' });
	});
});

describe('resolveRowWrite — projection preserve on write (unfiled 2635 sibling)', () => {
	it('carries the stored is_unparented across a NEWER-seq write that omits it', () => {
		// A mutation-response snapshot omits local-first projections; a newer-seq
		// write must not drop the bit from IDB (mirrors RAM's mergeRow preserve).
		const stored = row(5, { is_unparented: true });
		const incoming = row(8); // newer, omits is_unparented
		const d = decide(stored, undefined, incoming);
		expect(d.action).toBe('write');
		expect(d.action === 'write' && d.row.is_unparented).toBe(true);
		expect(d.action === 'write' && d.row.seq).toBe(8);
	});

	it('does not invent a projection bit when neither side has one', () => {
		const d = decide(row(5), undefined, row(8));
		expect(d.action === 'write' && 'is_unparented' in d.row).toBe(false);
	});
});

describe('resolveRowWrite — tombstone gate (BUG-2633)', () => {
	it('REFUSES a seq-less snapshot when a tombstone exists (resurrection)', () => {
		expect(decide(undefined, tomb(10), row(undefined))).toEqual({ action: 'skip' });
	});

	it('REFUSES a snapshot at or below the tombstone seq (stale resurrection)', () => {
		expect(decide(undefined, tomb(10), row(9))).toEqual({ action: 'skip' });
		expect(decide(undefined, tomb(10), row(10))).toEqual({ action: 'skip' });
	});

	it('WRITES a strictly-newer snapshot, superseding the tombstone', () => {
		const d = decide(undefined, tomb(10), row(11));
		expect(d).toEqual({ action: 'write', row: row(11) });
		// The caller deletes the tombstone in the same tx; resolveRowWrite only
		// decides the write — the supersession-delete is the persistence layer's.
	});

	it('a tombstone does not override a stored row that is even newer', () => {
		// Defensive: if both a stored row and a tombstone exist for an id, the seq
		// guard against the stored row still applies once the tombstone is passed.
		const stored = row(20);
		const incoming = row(15); // newer than tombstone, older than stored
		expect(decide(stored, tomb(10), incoming)).toEqual({ action: 'skip' });
	});
});

describe('preserveProjectionMetadata (moved verbatim from localIndex)', () => {
	it('copies is_unparented from existing when next omits it', () => {
		expect(preserveProjectionMetadata(row(1, { is_unparented: true }), row(2))).toMatchObject({
			is_unparented: true,
		});
	});
	it('leaves next untouched when it already carries the bit', () => {
		const next = row(2, { is_unparented: false });
		expect(preserveProjectionMetadata(row(1, { is_unparented: true }), next)).toBe(next);
	});
	it('no-ops when existing is undefined', () => {
		const next = row(2);
		expect(preserveProjectionMetadata(undefined, next)).toBe(next);
	});
});

describe('mergeEqualSeqProjection (moved verbatim from localIndex)', () => {
	it('returns a merged row when incoming changes the projection bit', () => {
		expect(
			mergeEqualSeqProjection(row(7, { is_unparented: false }), row(7, { is_unparented: true })),
		).toMatchObject({ is_unparented: true });
	});
	it('returns null when incoming omits the bit', () => {
		expect(mergeEqualSeqProjection(row(7, { is_unparented: true }), row(7))).toBeNull();
	});
	it('returns null when the bit is unchanged', () => {
		expect(
			mergeEqualSeqProjection(row(7, { is_unparented: true }), row(7, { is_unparented: true })),
		).toBeNull();
	});
});
