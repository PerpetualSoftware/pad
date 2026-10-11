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
});
