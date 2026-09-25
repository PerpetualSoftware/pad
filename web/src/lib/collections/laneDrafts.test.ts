// Node-project test: where an unsaved lane draft saves once its lane is gone
// (BUG-3043, lead ruling RE-HOME).
//
// The "can Uncategorized receive it" column follows the server's validator
// (internal/items/validate.go): a select stores '' even when required, a
// reference blank is normalised to absent, and an absent key is filled by a
// default or refused as required.
import { describe, it, expect } from 'vitest';
import {
	blockedDraftMessage,
	draftCreateFields,
	draftSaveTarget,
	draftTargets,
	draftableLanes,
	draftKey,
	lostLaneLabel,
	parseDraftKey,
	saveAllDrafts,
	uncategorizedWrite
} from './laneDrafts';
import type { FieldDef } from '$lib/types';

const f = (type: string, over: Partial<FieldDef> = {}): FieldDef =>
	({ key: 'g', label: 'Group', type, ...over }) as unknown as FieldDef;

describe('draftableLanes', () => {
	it('is the field options for an options grouping', () => {
		expect([...draftableLanes(f('select', { options: ['a', 'b'] }))]).toEqual(['a', 'b']);
	});
	it('is empty where the board offers no "+": refused, relation, or no field', () => {
		// A multi_relation that RETAINED its options is the filing's case: the
		// board refuses the grouping, so those options are not lanes any more.
		expect(draftableLanes(f('multi_relation', { options: ['a'] })).size).toBe(0);
		expect(draftableLanes(f('relation', { collection: 'people', options: ['a'] })).size).toBe(0);
		expect(draftableLanes(f('relation', { options: ['a'] })).size).toBe(0);
		expect(draftableLanes(undefined).size).toBe(0);
	});
});

describe('uncategorizedWrite', () => {
	it("sends '' for a select and [] for a multi_select — both ARE Uncategorized", () => {
		expect(uncategorizedWrite(f('select'))).toEqual({ send: true, value: '' });
		expect(uncategorizedWrite(f('multi_select'))).toEqual({ send: true, value: [] });
	});
	it('omits the key for a reference field and for number / checkbox', () => {
		for (const t of ['relation', 'multi_relation', 'number', 'checkbox']) {
			expect(uncategorizedWrite(f(t)), t).toEqual({ send: false });
		}
	});
});

describe('draftSaveTarget', () => {
	const none = {};

	it('keeps a draft whose lane is still offered in that lane', () => {
		expect(draftSaveTarget('a', f('select', { options: ['a'] }), none)).toEqual({ kind: 'lane', lane: 'a' });
	});

	it('PATH 1 — a renamed or deleted option re-homes the draft, even on a REQUIRED select', () => {
		// Required does not block: the server stores '' on a required select.
		const field = f('select', { options: ['b'], required: true });
		expect(draftSaveTarget('a', field, none)).toEqual({ kind: 'rehomed', lostLane: 'a' });
	});

	it('PATH 2 — a retype to multi_relation re-homes every lane, since the grouping is refused', () => {
		const field = f('multi_relation', { options: ['a', 'b'] });
		expect(draftSaveTarget('a', field, none)).toEqual({ kind: 'rehomed', lostLane: 'a' });
		expect(draftSaveTarget('b', field, none)).toEqual({ kind: 'rehomed', lostLane: 'b' });
	});

	it('a regroup by another field orphans a draft typed under the previous one', () => {
		const priority = f('select', { key: 'priority', options: ['low', 'high'] });
		expect(draftSaveTarget('open', priority, none)).toEqual({ kind: 'rehomed', lostLane: 'open' });
	});

	it('EDGE 1 — a REQUIRED field Uncategorized cannot hold blocks the draft instead', () => {
		expect(draftSaveTarget('3', f('number', { options: ['1'], required: true }), none)).toEqual({
			kind: 'blocked',
			lostLane: '3',
			reason: 'required'
		});
		expect(draftSaveTarget('x', f('multi_relation', { required: true }), none)).toEqual({
			kind: 'blocked',
			lostLane: 'x',
			reason: 'required'
		});
	});

	it('EDGE 1 — a default that would fill the omitted key blocks rather than lands elsewhere', () => {
		// Saved "into Uncategorized", it would appear in the default's lane.
		expect(draftSaveTarget('3', f('number', { options: ['1'], default: 1 }), none)).toMatchObject({
			kind: 'blocked',
			reason: 'defaulted'
		});
		// The client pre-fill counts the same (createDefaultFields sets status).
		const status = f('checkbox', { key: 'status', options: ['true'] });
		expect(draftSaveTarget('x', status, { status: true })).toMatchObject({ kind: 'blocked', reason: 'defaulted' });
	});

	it('an optional field with no default is Uncategorized when omitted', () => {
		expect(draftSaveTarget('3', f('number', { options: ['1'] }), none)).toEqual({ kind: 'rehomed', lostLane: '3' });
	});
});

describe('draftTargets', () => {
	it('covers every non-empty draft and skips blank ones', () => {
		const field = f('select', { options: ['a'] });
		expect(draftTargets({ a: 'x', gone: 'y', blank: '  ' }, field, {})).toEqual({
			a: { kind: 'lane', lane: 'a' },
			gone: { kind: 'rehomed', lostLane: 'gone' }
		});
	});
});

describe('blockedDraftMessage', () => {
	it('names the lost lane and the reason', () => {
		const m = blockedDraftMessage('In review', 'Stage', 'required');
		expect(m).toContain('“In review”');
		expect(m).toContain('Stage is required');
	});
});

describe('draftCreateFields', () => {
	it('converts a LIVE lane through the declared type, as before', () => {
		expect(draftCreateFields('0', f('number', { options: ['0'] }), 'g', {})).toEqual({ ok: true, fields: { g: 0 } });
	});

	it('saves a re-homed select draft with the Uncategorized blank, over the client default', () => {
		const status = f('select', { key: 'status', options: ['open'] });
		expect(draftCreateFields('gone', status, 'status', { status: 'open' })).toEqual({
			ok: true,
			fields: { status: '' }
		});
	});

	it('OMITS the key for a re-homed reference field (a blank is absent at the write door)', () => {
		expect(draftCreateFields('a', f('multi_relation', { options: ['a'] }), 'g', {})).toEqual({ ok: true, fields: {} });
	});

	it('refuses a blocked draft with the lost-lane message instead of sending anything', () => {
		const r = draftCreateFields('7', f('number', { label: 'Score', options: ['1'], required: true }), 'g', {});
		expect(r.ok).toBe(false);
		expect(!r.ok && r.message).toContain('“7”');
	});
});

describe('saveAllDrafts', () => {
	it('clears each draft as its create lands', async () => {
		const cleared: string[] = [];
		const out = await saveAllDrafts({ a: 'x', b: 'y', c: ' ' }, async () => ({ id: 1 }), () => true, (l) => cleared.push(l));
		expect(out).toBe('done');
		expect(cleared).toEqual(['a', 'b']);
	});

	it('PATH 2 — a create that RETURNS null with the identity held is a failure, and the text is KEPT', async () => {
		// The old loop cleared the draft here: the create had toasted a
		// client-side refusal and returned null, and the text was gone.
		const cleared: string[] = [];
		const out = await saveAllDrafts(
			{ a: 'x', b: 'y' },
			async (l) => (l === 'a' ? null : { id: 1 }),
			() => true,
			(l) => cleared.push(l)
		);
		expect(out).toBe('failed');
		expect(cleared).toEqual([]);
	});

	it('PATH 1 — a THROWN create stops the loop and keeps that draft and the rest', async () => {
		const cleared: string[] = [];
		const out = await saveAllDrafts(
			{ a: 'x', b: 'y', c: 'z' },
			async (l) => {
				if (l === 'b') throw new Error('refused');
				return { id: 1 };
			},
			() => true,
			(l) => cleared.push(l)
		);
		expect(out).toBe('failed');
		expect(cleared).toEqual(['a']);
	});

	it('writes nothing once the identity has moved, whatever the create returned', async () => {
		const cleared: string[] = [];
		let held = true;
		const out = await saveAllDrafts(
			{ a: 'x' },
			async () => {
				held = false;
				return { id: 1 };
			},
			() => held,
			(l) => cleared.push(l)
		);
		expect(out).toBe('identity_moved');
		expect(cleared).toEqual([]);
	});
});

describe('draft keys carry the group field (BUG-3214)', () => {
	it('round-trips field and lane, and reads a bare key as a lane', () => {
		expect(parseDraftKey(draftKey('status', 'open'))).toEqual({ field: 'status', lane: 'open' });
		expect(parseDraftKey('open')).toEqual({ field: null, lane: 'open' });
		expect(parseDraftKey(draftKey('status', 'a,b'))).toEqual({ field: 'status', lane: 'a,b' });
	});

	it('THE BUG: a draft typed under status "open" is NOT stage\'s live "open" lane after a regroup', () => {
		const stage = f('select', { key: 'stage', options: ['open', 'later'] });
		expect(draftSaveTarget(draftKey('status', 'open'), stage, {})).toEqual({
			kind: 'rehomed',
			lostLane: 'open',
			lostField: 'status'
		});
	});

	it('a draft under the CURRENT field keeps its live lane, as before', () => {
		const stage = f('select', { key: 'stage', options: ['open'] });
		expect(draftSaveTarget(draftKey('stage', 'open'), stage, {})).toEqual({ kind: 'lane', lane: 'open' });
	});

	it('a regrouped draft that Uncategorized cannot receive is blocked, naming its field', () => {
		const score = f('number', { key: 'score', options: ['1'], required: true });
		const t = draftSaveTarget(draftKey('status', 'open'), score, {});
		expect(t).toEqual({ kind: 'blocked', lostLane: 'open', lostField: 'status', reason: 'required' });
		expect(lostLaneLabel(t as never, (k) => (k === 'status' ? 'Status' : k))).toBe('Open (Status)');
	});
});
