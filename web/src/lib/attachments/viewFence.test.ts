import { describe, it, expect } from 'vitest';
import { viewIdentity, createFence, createPaintFence } from './viewFence';

// The invariant these fences encode failed four review rounds running, always
// by comparing only PART of the identity. These tests pin the module directly:
// the identity rules, then each of the three fences, then the two collapses
// that a prior round proved are NOT safe.

describe('viewIdentity', () => {
	it('is the whole record, not any one part', () => {
		let ws = 'ws-a';
		let item = 'item-1';
		const view = viewIdentity(() => ({ ws, item }));

		const token = view.capture();
		expect(token.changed()).toBe(false);

		// The item half.
		item = 'item-2';
		expect(token.changed()).toBe(true);
		item = 'item-1';
		expect(token.changed()).toBe(false);

		// The workspace half — the one that kept getting forgotten.
		ws = 'ws-b';
		expect(token.changed()).toBe(true);
	});

	it('is not addressable while any part is missing', () => {
		let item: string | null = null;
		const view = viewIdentity(() => ({ ws: 'ws-a', item }));

		expect(view.key()).toBeNull();
		// A half-identified view must not pass a fence just because the half it
		// does know still matches.
		const blank = view.capture();
		expect(blank.changed()).toBe(true);

		item = '';
		expect(view.key()).toBeNull();

		item = 'item-1';
		expect(view.key()).not.toBeNull();
		expect(view.capture().changed()).toBe(false);
	});

	it('never matches a null key', () => {
		const view = viewIdentity(() => ({ ws: 'ws-a' }));
		expect(view.matches(null)).toBe(false);
		expect(view.matches(view.key())).toBe(true);
	});

	it('cannot confuse two different splits of the same characters', () => {
		const a = viewIdentity(() => ({ ws: 'x', item: 'yz' })).key();
		const b = viewIdentity(() => ({ ws: 'xy', item: 'z' })).key();
		expect(a).not.toBe(b);
	});

	it('cannot be collided by parts that embed the serialiser\'s own syntax', () => {
		// The specific way a delimiter join fails: a part containing the entry
		// separator FOLLOWED BY the next part's name and the name/value
		// separator re-creates the exact byte sequence the join itself emits, so
		// two DIFFERENT identities serialise the same — and a stale fence passes,
		// which is the failure this module exists to prevent (Codex round 1).
		// Reproduced for the NUL/SOH join this module used to use, and for
		// separators built from the characters JSON has to escape.
		const shapes: [string, string][] = [
			[String.fromCharCode(0), String.fromCharCode(1)],
			['|', ':'],
			['"', ','],
			['\\', '"'],
			[']', '['],
		];
		for (const [entrySep, pairSep] of shapes) {
			const a = viewIdentity(() => ({ item: `x${entrySep}ws${pairSep}y`, ws: 'z' })).key();
			const b = viewIdentity(() => ({ item: 'x', ws: `y${entrySep}ws${pairSep}z` })).key();
			expect(a).not.toBeNull();
			expect(a).not.toBe(b);
		}

		// A join that drops the part NAMES collides without any crafted run at
		// all: `x<sep>y` + `z` and `x` + `y<sep>z` are the same list of values.
		for (const sep of [',', '|', String.fromCharCode(0)]) {
			expect(viewIdentity(() => ({ item: `x${sep}y`, ws: 'z' })).key()).not.toBe(
				viewIdentity(() => ({ item: 'x', ws: `y${sep}z` })).key()
			);
		}
	});

	it('does not depend on the order the parts are written in', () => {
		const a = viewIdentity(() => ({ ws: 'w', item: 'i' })).key();
		const b = viewIdentity(() => ({ item: 'i', ws: 'w' })).key();
		expect(a).toBe(b);
	});

	it('snapshots the parts, so a request reads back what it was ISSUED for', () => {
		let ws = 'ws-a';
		const view = viewIdentity(() => ({ ws }));
		const token = view.capture();
		ws = 'ws-b';
		// This is what stops a continuation from calling the API — or keying a
		// cache invalidation — with whichever workspace happens to be live.
		expect(token.value.ws).toBe('ws-a');
	});
});

describe('createFence (fences 1 and 2)', () => {
	it('stales a token once the fence restarts — same identity', () => {
		const view = viewIdentity(() => ({ ws: 'ws-a', item: 'item-1' }));
		const fence = createFence(view);

		const first = fence.begin();
		expect(first.stale()).toBe(false);
		const second = fence.restart();
		// A→A: the identity matches, so only the generation can tell a
		// superseded request from the current one.
		expect(first.stale()).toBe(true);
		expect(second.stale()).toBe(false);
	});

	it('stales a token once the identity moves — same generation', () => {
		let ws = 'ws-a';
		const fence = createFence(viewIdentity(() => ({ ws })));

		const token = fence.begin();
		expect(token.stale()).toBe(false);
		ws = 'ws-b';
		expect(token.stale()).toBe(true);
	});

	it('stales across an A→B→A round trip, where the identity matches again', () => {
		let ws = 'ws-a';
		const fence = createFence(viewIdentity(() => ({ ws })));

		const first = fence.restart();
		ws = 'ws-b';
		fence.restart();
		ws = 'ws-a';
		const current = fence.restart();

		// The identity is back to A, so the identity compare alone would let
		// A's first response write over A's current one.
		expect(first.stale()).toBe(true);
		expect(current.stale()).toBe(false);
	});

	it('begin() does not supersede its siblings; restart() does', () => {
		const fence = createFence(viewIdentity(() => ({ ws: 'ws-a' })));

		// Two concurrent mutations must both still be allowed to reconcile.
		const one = fence.begin();
		const two = fence.begin();
		expect(one.stale()).toBe(false);
		expect(two.stale()).toBe(false);

		fence.invalidate();
		expect(one.stale()).toBe(true);
		expect(two.stale()).toBe(true);
	});

	it('keeps the request and view fences independent', () => {
		// The whole reason there are two: a Retry restarts the REQUEST fence
		// (superseding its own in-flight listing) without touching the VIEW
		// fence, so a delete of a row still on screen keeps reconciling.
		const view = viewIdentity(() => ({ ws: 'ws-a', item: 'item-1' }));
		const requests = createFence(view);
		const views = createFence(view);

		const del = views.begin();
		requests.restart(); // the Retry
		expect(del.stale()).toBe(false);

		views.invalidate(); // a real switch, or unmount
		expect(del.stale()).toBe(true);
	});
});

describe('createPaintFence (fence 3)', () => {
	it('answers about the DOM, lagging the live props on purpose', () => {
		let ws = 'ws-a';
		const view = viewIdentity(() => ({ ws }));
		const paint = createPaintFence(view);

		expect(paint.isCurrent()).toBe(false); // nothing painted yet
		paint.record(view.capture());
		expect(paint.isCurrent()).toBe(true);

		// Props update synchronously; the effect that repaints flushes later.
		// In that window the controls on screen still belong to ws-a, and a
		// click on one must be refused.
		ws = 'ws-b';
		expect(paint.isCurrent()).toBe(false);
		expect(paint.painted()?.value.ws).toBe('ws-a');

		paint.record(view.capture());
		expect(paint.isCurrent()).toBe(true);
	});

	it('claims nothing for an un-addressable view', () => {
		let item: string | null = null;
		const view = viewIdentity(() => ({ ws: 'ws-a', item }));
		const paint = createPaintFence(view);

		paint.record(view.capture());
		expect(paint.painted()).toBeNull();
		expect(paint.isCurrent()).toBe(false);

		// And a run that bails on a missing id stops claiming the previous view.
		item = 'item-1';
		paint.record(view.capture());
		expect(paint.isCurrent()).toBe(true);
		item = null;
		paint.record(view.capture());
		expect(paint.painted()).toBeNull();
	});

	it('is not a generation fence: repainting the same view stays current', () => {
		const view = viewIdentity(() => ({ ws: 'ws-a' }));
		const paint = createPaintFence(view);
		const first = view.capture();
		paint.record(first);
		paint.record(view.capture());
		expect(paint.isCurrent()).toBe(true);
		expect(first.changed()).toBe(false);
	});
});
