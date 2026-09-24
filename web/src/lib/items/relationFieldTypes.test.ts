import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { describe, expect, it } from 'vitest';
import {
	RELATION_FIELD_TYPES,
	isMultiRelationType,
	isRelationType,
	isResolvableRelation,
	relationValuesOf, isRelationValueStoredAsText } from './relationFieldTypes';

/**
 * The TS relation-type predicates (PLAN-2857 U4).
 *
 * The Go side has the same pair with the same split, and the split is
 * load-bearing: field-level code asks `isRelationType`, value-level code must
 * ask `isMultiRelationType` too, because the shapes differ. A single predicate
 * answering both alike would make that inexpressible, which is the defect these
 * two exist to prevent.
 */
describe('isRelationType', () => {
	it('covers both relation types and nothing else', () => {
		for (const t of ['relation', 'multi_relation']) {
			expect(isRelationType(t), t).toBe(true);
		}
		// Every other type the app ships, listed explicitly rather than derived,
		// so a new type forces a decision here instead of inheriting one.
		for (const t of [
			'text',
			'number',
			'select',
			'multi_select',
			'date',
			'checkbox',
			'url',
			'json',
			'',
			'Relation',
			'relations'
		]) {
			expect(isRelationType(t), t).toBe(false);
		}
	});

	it('treats undefined and null as not-a-relation', () => {
		// Field `type` is optional on several of the shapes these predicates are
		// handed (public-share payloads, preflight rows), and a thrown TypeError
		// there would take out a whole view.
		expect(isRelationType(undefined)).toBe(false);
		expect(isRelationType(null)).toBe(false);
	});
});

describe('isMultiRelationType', () => {
	it('is the NARROWER question', () => {
		expect(isMultiRelationType('multi_relation')).toBe(true);
		expect(isMultiRelationType('relation')).toBe(false);
		expect(isMultiRelationType('multi_select')).toBe(false);
		expect(isMultiRelationType(undefined)).toBe(false);
	});
});

describe('isResolvableRelation', () => {
	it('requires BOTH a relation type and a declared target', () => {
		expect(isResolvableRelation({ type: 'relation', collection: 'people' })).toBe(true);
		expect(isResolvableRelation({ type: 'multi_relation', collection: 'people' })).toBe(true);
		// A relation with no target is a field nothing can resolve, and every
		// surface that pairs these two withholds its affordance rather than
		// offering one that cannot work.
		expect(isResolvableRelation({ type: 'relation', collection: '' })).toBe(false);
		expect(isResolvableRelation({ type: 'multi_relation' })).toBe(false);
		expect(isResolvableRelation({ type: 'select', collection: 'people' })).toBe(false);
		expect(isResolvableRelation(undefined)).toBe(false);
		expect(isResolvableRelation(null)).toBe(false);
	});
});

describe('RELATION_FIELD_TYPES', () => {
	it('agrees with isRelationType', () => {
		// Two expressions of one fact drift; this is the only place that can
		// notice. Mirrors the Go test on RelationFieldTypes().
		expect(RELATION_FIELD_TYPES.length).toBe(2);
		for (const t of RELATION_FIELD_TYPES) {
			expect(isRelationType(t), t).toBe(true);
		}
	});
});

describe('relationValuesOf', () => {
	// The READ shape, extracted from FieldEditor under BUG-3016 so the table cell
	// resolves the same references the properties chip does. FieldEditor keeps
	// its write-side hold in front of this; nothing about the hold is here.
	it('gives one element for a scalar relation and N in order for a list', () => {
		expect(relationValuesOf('relation', 'id-red')).toEqual(['id-red']);
		expect(relationValuesOf('multi_relation', ['id-blue', 'id-red'])).toEqual(['id-blue', 'id-red']);
	});

	it('trims, and drops blank or non-string elements', () => {
		// The write doors refuse both outright, so this is defence against a value
		// no door will accept — not a policy of its own.
		expect(relationValuesOf('relation', '  id-red  ')).toEqual(['id-red']);
		expect(relationValuesOf('relation', '   ')).toEqual([]);
		expect(relationValuesOf('multi_relation', ['id-red', '', '  ', 7, null])).toEqual(['id-red']);
	});

	it('gives nothing for a non-relation field, whatever it holds', () => {
		expect(relationValuesOf('text', 'id-red')).toEqual([]);
		expect(relationValuesOf(undefined, 'id-red')).toEqual([]);
	});

	it('gives NOTHING for a scalar stored under a multi_relation', () => {
		// The type was changed under a stored value. One arbitrary element is a
		// worse answer than an empty cell, which at least reads as "nothing this
		// view can show".
		expect(relationValuesOf('multi_relation', 'id-red')).toEqual([]);
		expect(relationValuesOf('multi_relation', null)).toEqual([]);
	});

	it('gives nothing for a LIST stored under a scalar relation', () => {
		// The mirror case, and it must not stringify: `String(['a','b'])` is
		// "a,b", which would render as a reference to an item named a,b.
		expect(relationValuesOf('relation', ['id-red', 'id-blue'])).toEqual([]);
	});
});

/**
 * BUG-3014 shared vectors: the other half of
 * TestRelationValueStoredAsText_SharedVectors in
 * internal/store/relation_stored_as_text_test.go. KEEP IN SYNC: both suites
 * read the same table, so the web's copy of the `stored_as_text` predicate and
 * the server's can only drift by turning one of them red. The table includes
 * U+0085 (a space to Go only) and U+FEFF (to JS only), which is what makes the
 * trim a contract rather than a detail.
 */
const storedAsTextFixture = JSON.parse(
	readFileSync(
		fileURLToPath(new URL('../../../../internal/store/testdata/relation_stored_as_text_cases.json', import.meta.url)),
		'utf8',
	),
) as { cases: { name: string; value: string; stored_as_text: boolean }[] };

describe('isRelationValueStoredAsText — server contract fixture', () => {
	// An empty or truncated fixture would make every it() below vacuous.
	it('loads the full shared fixture', () => {
		expect(storedAsTextFixture.cases.length).toBeGreaterThanOrEqual(20);
	});

	for (const tc of storedAsTextFixture.cases) {
		it(tc.name, () => {
			expect(isRelationValueStoredAsText(tc.value)).toBe(tc.stored_as_text);
		});
	}
});
