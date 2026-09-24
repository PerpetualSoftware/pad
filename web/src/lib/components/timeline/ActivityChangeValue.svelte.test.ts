import { describe, it, expect, beforeEach, afterEach } from 'vitest';
import { render, cleanup } from '@testing-library/svelte';
import { existenceClaimsIn } from '../../../test/existenceClaim';
import type { FieldDef } from '$lib/types';

// BUG-2872 — one side of an activity field change. A relation side renders the
// target (or an honest state), never the item ID the field stores; any other
// field's side renders its text untouched. The resolver and readiness come from
// the owning page's ChangeContext (tested in changeContext.test.ts); here a
// fake one drives each state.
import type { ChangeContext } from '$lib/timeline/changeContext';
import type { ItemIndexRow } from '$lib/types';

import ActivityChangeValue from './ActivityChangeValue.svelte';

const ID = '11111111-2222-3333-4444-555555555555';
const OWNER: FieldDef = { key: 'owner', label: 'Owner', type: 'relation', collection: 'people' } as FieldDef;
const NOTE: FieldDef = { key: 'note', label: 'Note', type: 'text' } as FieldDef;
const row = (over: Record<string, unknown> = {}) => ({
	id: ID,
	slug: 'ada',
	title: 'Ada Lovelace',
	collection_slug: 'people',
	collection_prefix: 'PEOP',
	item_number: 7,
	...over,
});

let ready = true;
let lookup: (id: string, declared: string | undefined) => ItemIndexRow | null = () => null;
const context: ChangeContext = {
	fieldFor: () => undefined,
	resolveRow: (id, declared) => lookup(id, declared),
	indexReady: () => ready,
};

function textOf(props: { text: string; field?: FieldDef | null; noContext?: boolean }) {
	const { container } = render(ActivityChangeValue, {
		props: { text: props.text, field: props.field, context: props.noContext ? undefined : context },
	});
	return container.textContent?.replace(/\s+/g, ' ').trim() ?? '';
}

describe('ActivityChangeValue (BUG-2872)', () => {
	beforeEach(() => {
		ready = true;
		lookup = () => null;
	});
	afterEach(() => cleanup());

	it('a resolved relation renders REF and title, never the id', () => {
		let declaredSeen: string | undefined;
		lookup = (v, declared) => {
			declaredSeen = declared;
			return v === ID ? (row() as unknown as ItemIndexRow) : null;
		};
		const t = textOf({ text: ID, field: OWNER });
		expect(t).toContain('Ada Lovelace');
		expect(t).toMatch(/PEOP-7/);
		expect(t).not.toContain(ID);
		expect(declaredSeen, 'resolved against the field\'s declared target').toBe('people');
	});

	it('a deleted target says so', () => {
		lookup = () => row({ deleted_at: '2026-09-01T00:00:00Z' }) as unknown as ItemIndexRow;
		const t = textOf({ text: ID, field: OWNER });
		expect(t).toContain('(deleted)');
		expect(t).not.toContain(ID);
	});

	it('with the index READY, a value naming nothing is "Unavailable item"', () => {
		const t = textOf({ text: ID, field: OWNER });
		expect(t).toBe('Unavailable item');
	});

	it('an unresolved value says nothing about whether its target exists (BUG-3013)', () => {
		const { container } = render(ActivityChangeValue, { props: { text: ID, field: OWNER, context } });
		const chip = container.querySelector('.change-relation.is-unresolved');
		expect(chip, 'precondition: the unresolved chip rendered').not.toBeNull();
		expect(existenceClaimsIn(chip!)).toEqual([]);
	});

	it('with the index NOT ready, a miss is "Linked item" — not a claim that it names nothing', () => {
		ready = false;
		const t = textOf({ text: ID, field: OWNER });
		expect(t).toBe('Linked item');
		expect(t).not.toContain(ID);
	});

	it('a non-relation field renders its text verbatim, even an id-shaped one', () => {
		lookup = () => row() as unknown as ItemIndexRow;
		expect(textOf({ text: ID, field: NOTE })).toBe(ID);
		expect(textOf({ text: 'plain words', field: null })).toBe('plain words');
	});

	it('with no context (a caller that supplies none) a relation side is "Linked item", never the id', () => {
		expect(textOf({ text: ID, field: OWNER, noContext: true })).toBe('Linked item');
	});

	it('an empty relation side stays empty', () => {
		expect(textOf({ text: '', field: OWNER })).toBe('');
	});
});
