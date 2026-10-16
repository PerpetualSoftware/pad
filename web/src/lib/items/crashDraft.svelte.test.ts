// TASK-3606: crash drafts. jsdom (localStorage), with the interval injected.
import { describe, it, expect, beforeEach } from 'vitest';
import { registerCrashDraftKeeper, CRASH_DRAFT_MAX_CHARS, type CrashDraftSource } from './crashDraft';
import { readRefusedRawDraft, keepRefusedRawDraft } from './refusedRawDraft';

function harness(over: Partial<CrashDraftSource> = {}) {
	const s = { userId: 'u1', itemId: 'i1' as string | null, atRisk: true, markdown: 'draft', baseSeq: 7, baseBody: 'base', discarding: false };
	let tick: () => void = () => {};
	const stop = registerCrashDraftKeeper(
		{
			userId: () => s.userId,
			itemId: () => s.itemId,
			atRisk: () => s.atRisk,
			snapshot: () => ({ markdown: s.markdown, baseSeq: s.baseSeq, baseBody: s.baseBody }),
			discarding: () => s.discarding,
			...over,
		},
		(fn) => {
			tick = fn;
			return () => {};
		},
	);
	return { s, tick: () => tick(), stop };
}

beforeEach(() => localStorage.clear());

describe('crash drafts (TASK-3606)', () => {
	it('while edits are at risk, the markdown is kept with its base seq and body', () => {
		const h = harness();
		h.tick();
		expect(readRefusedRawDraft('u1', 'i1')).toMatchObject({ markdown: 'draft', baseSeq: 7, baseBody: 'base' });
		h.s.markdown = 'draft 2';
		h.tick();
		expect(readRefusedRawDraft('u1', 'i1')?.markdown).toBe('draft 2');
	});

	it('cleared once the same item has nothing at risk (the edits are confirmed)', () => {
		const h = harness();
		h.tick();
		h.s.atRisk = false;
		h.tick();
		expect(readRefusedRawDraft('u1', 'i1')).toBeNull();
	});

	it('switching items clears nothing: the earlier item may not have saved', () => {
		const h = harness();
		h.tick();
		h.s.itemId = 'i2';
		h.s.atRisk = false;
		h.tick();
		expect(readRefusedRawDraft('u1', 'i1')?.markdown).toBe('draft');
	});

	it('a newer keep from another producer is not erased by the confirmation', () => {
		const h = harness();
		h.tick();
		keepRefusedRawDraft('u1', 'i1', 'kept by the unload flush');
		h.s.atRisk = false;
		h.tick();
		expect(readRefusedRawDraft('u1', 'i1')?.markdown).toBe('kept by the unload flush');
	});

	it('nothing is kept during a confirmed sign-out discard, or with no user', () => {
		const h = harness();
		h.s.discarding = true;
		h.tick();
		expect(readRefusedRawDraft('u1', 'i1')).toBeNull();
		h.s.discarding = false;
		h.s.userId = '';
		h.tick();
		expect(readRefusedRawDraft('u1', 'i1')).toBeNull();
	});

	it('an unchanged version skips serializing (codex r1)', () => {
		let snaps = 0;
		let v = 1;
		const h = harness({
			version: () => v,
			snapshot: () => {
				snaps++;
				return { markdown: `draft ${v}`, baseSeq: 1, baseBody: 'b' };
			},
		});
		h.tick();
		h.tick();
		h.tick();
		expect(snaps, 'serialized once while nothing changed').toBe(1);
		v = 2;
		h.tick();
		expect(snaps).toBe(2);
		expect(readRefusedRawDraft('u1', 'i1')?.markdown).toBe('draft 2');
	});

	it('a body over the cap is not kept', () => {
		const h = harness();
		h.s.markdown = 'x'.repeat(CRASH_DRAFT_MAX_CHARS + 1);
		h.tick();
		expect(readRefusedRawDraft('u1', 'i1')).toBeNull();
	});

	it("a full store makes room from the user's oldest other draft, then keeps", () => {
		keepRefusedRawDraft('u1', 'old', 'old text', 1000);
		const real = Storage.prototype.setItem;
		let calls = 0;
		Storage.prototype.setItem = function (k: string, v: string) {
			calls++;
			if (calls === 1) throw new DOMException('full', 'QuotaExceededError');
			return real.call(this, k, v);
		};
		try {
			const h = harness();
			h.tick();
		} finally {
			Storage.prototype.setItem = real;
		}
		expect(readRefusedRawDraft('u1', 'i1')?.markdown).toBe('draft');
		expect(readRefusedRawDraft('u1', 'old')).toBeNull();
	});

	it('BUG-3850: a stale base is kept flagged, in the same write, and read back', () => {
		const h = harness({ snapshot: () => ({ markdown: 'A C U', baseSeq: 4, baseBody: 'A', baseStale: true }) });
		h.tick();
		expect(readRefusedRawDraft('u1', 'i1')).toEqual({ markdown: 'A C U', savedAt: expect.any(Number), baseSeq: 4, baseBody: 'A', baseStale: true });
	});

	it('BUG-3850: a base that is not stale carries no flag', () => {
		const h = harness({ snapshot: () => ({ markdown: 'A C U', baseSeq: 4, baseBody: 'A C', baseStale: false }) });
		h.tick();
		expect(readRefusedRawDraft('u1', 'i1')).toEqual({ markdown: 'A C U', savedAt: expect.any(Number), baseSeq: 4, baseBody: 'A C' });
	});

	it('BUG-3850 r5: the same markdown over a base that turned stale is written again, flagged', () => {
		// A remote ' C' lands and the user deletes it again within one tick: the
		// document moved (version), the markdown is what was kept, the base is stale.
		let v = 1;
		let snap: { markdown: string; baseSeq: number; baseBody: string; baseStale?: boolean } = { markdown: 'A U', baseSeq: 4, baseBody: 'A' };
		const h = harness({ version: () => v, snapshot: () => snap });
		h.tick();
		expect(readRefusedRawDraft('u1', 'i1')?.baseStale).toBeUndefined();
		v = 2;
		snap = { markdown: 'A U', baseSeq: 4, baseBody: 'A', baseStale: true };
		h.tick();
		expect(readRefusedRawDraft('u1', 'i1')).toEqual({ markdown: 'A U', savedAt: expect.any(Number), baseSeq: 4, baseBody: 'A', baseStale: true });
	});

	it('BUG-3850 r5: the same markdown over a base body that moved at the same seq is written with that body', () => {
		let v = 1;
		let snap = { markdown: 'A U', baseSeq: 4, baseBody: 'A' };
		const h = harness({ version: () => v, snapshot: () => snap });
		h.tick();
		v = 2;
		snap = { markdown: 'A U', baseSeq: 4, baseBody: 'A U' };
		h.tick();
		expect(readRefusedRawDraft('u1', 'i1')).toMatchObject({ markdown: 'A U', baseSeq: 4, baseBody: 'A U' });
	});

	it('BUG-3850 r5: the same markdown over the same body at a new seq is written with that seq', () => {
		let v = 1;
		let snap = { markdown: 'A U', baseSeq: 4, baseBody: 'A' };
		const h = harness({ version: () => v, snapshot: () => snap });
		h.tick();
		v = 2;
		snap = { markdown: 'A U', baseSeq: 6, baseBody: 'A' };
		h.tick();
		expect(readRefusedRawDraft('u1', 'i1')).toMatchObject({ baseSeq: 6 });
	});

	it('BUG-3850 r5: an unchanged markdown and base still skip the write', () => {
		const h = harness();
		h.tick();
		const first = readRefusedRawDraft('u1', 'i1')?.savedAt;
		localStorage.clear();
		h.tick();
		expect(first).toBeDefined();
		expect(readRefusedRawDraft('u1', 'i1'), 'skipped: nothing was written after the clear').toBeNull();
	});

	it('BUG-3850 r8: a send flags the kept draft stale at once, before any tick (a catch-up after offline typing)', () => {
		let fire: () => void = () => {};
		const h = harness({ onSent: (fn) => ((fire = fn), () => {}) });
		h.tick();
		expect(readRefusedRawDraft('u1', 'i1')).toEqual({ markdown: 'draft', savedAt: expect.any(Number), baseSeq: 7, baseBody: 'base' });
		fire();
		expect(readRefusedRawDraft('u1', 'i1')).toEqual({ markdown: 'draft', savedAt: expect.any(Number), baseSeq: 7, baseBody: 'base', baseStale: true });
	});

	it('BUG-3850 r8: a send rewrites once, and never with no draft, another item, or after the draft was cleared', () => {
		let fire: () => void = () => {};
		const h = harness({ onSent: (fn) => ((fire = fn), () => {}) });
		fire();
		expect(readRefusedRawDraft('u1', 'i1'), 'no draft kept yet').toBeNull();
		h.tick();
		fire();
		const flagged = localStorage.getItem(localStorage.key(0)!);
		localStorage.clear();
		fire();
		expect(localStorage.length, 'already stale: no second write').toBe(0);
		expect(flagged).toContain('"baseStale":true');
		const g = harness({ onSent: (fn) => ((fire = fn), () => {}) });
		g.tick();
		g.s.itemId = 'i2';
		fire();
		expect(readRefusedRawDraft('u1', 'i1')?.baseStale, 'another item shown: its sends say nothing of this draft').toBeUndefined();
		g.s.itemId = 'i1';
		g.s.atRisk = false;
		g.tick();
		fire();
		expect(readRefusedRawDraft('u1', 'i1'), 'cleared on confirmation: nothing to flag').toBeNull();
	});

	it('BUG-3850 r8: stopping the keeper unsubscribes', () => {
		let unsubscribed = false;
		const h = harness({ onSent: () => () => (unsubscribed = true) });
		h.stop();
		expect(unsubscribed).toBe(true);
	});

	it('BUG-3850: observe runs on every tick, at risk or not', () => {
		let n = 0;
		const h = harness({ observe: () => n++ });
		h.tick();
		h.s.atRisk = false;
		h.tick();
		expect(n).toBe(2);
	});
});

