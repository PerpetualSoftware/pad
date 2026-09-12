import { describe, expect, it } from 'vitest';
import {
	RELATION_FIELD_TYPES,
	isMultiRelationType,
	isRelationType,
	isResolvableRelation
} from './relationFieldTypes';

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
