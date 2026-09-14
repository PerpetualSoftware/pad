/**
 * Which field types name other items (PLAN-2857 U4).
 *
 * The TypeScript mirror of Go's `models.FieldDef.IsRelation` /
 * `IsMultiRelation` / `RelationFieldTypes`, and it exists for the same reason:
 * before U4 the only spelling was the literal `'relation'` at 16 sites across
 * 12 web files with nothing behind it, so "which code decides relation
 * behaviour?" was a question only a string search could answer — and that
 * answer is wrong the moment a second relation type exists, which is what U4
 * adds.
 *
 * Deliberately TWO predicates, not one:
 *
 *   - `isRelationType` is the FIELD-level question — does this field point at
 *     other items at all. Use it for "does it need a target collection", "may a
 *     public share group by it", "does the copy dialog need a value for it".
 *   - `isMultiRelationType` is the VALUE-level question — is the value an
 *     ORDERED LIST rather than a single reference. Every site that reads or
 *     writes a value has to ask this too, because the shapes differ, and a
 *     single predicate answering both alike makes that distinction
 *     inexpressible.
 *
 * Kept in `lib/items/` rather than `lib/components/collections/` because
 * non-component code asks these questions too (`shareView`, `copyNeedsValue`),
 * and a predicate living under `components/` invites a second copy for
 * everything that is not a component.
 */
import type { FieldDef } from '$lib/types';

/** Every field type whose value names other items. */
export const RELATION_FIELD_TYPES = ['relation', 'multi_relation'] as const;

export type RelationFieldType = (typeof RELATION_FIELD_TYPES)[number];

/** Does this field point at other items — one of them, or a list? */
export function isRelationType(type: string | undefined | null): boolean {
	return type === 'relation' || type === 'multi_relation';
}

/** Is this field's value an ORDERED LIST of references rather than a single one? */
export function isMultiRelationType(type: string | undefined | null): boolean {
	return type === 'multi_relation';
}

/**
 * Does this field point at other items AND declare where they live?
 *
 * The pairing appears at five sites — board grouping, list grouping, the filter
 * bar, and both collection modals — because a relation with no declared target
 * is a field nothing can resolve, and every one of those surfaces has to
 * withhold its affordance rather than offer one that cannot work.
 */
export function isResolvableRelation(field: Pick<FieldDef, 'type' | 'collection'> | undefined | null): boolean {
	return !!field && isRelationType(field.type) && !!field.collection;
}

/**
 * A relation field's references as raw strings, in order — the READ shape.
 *
 * ONE element for a scalar `relation`, N for a `multi_relation`, none when the
 * field is empty, so a caller renders a list and the scalar case is the
 * one-element case rather than a second code path. `FieldEditor` worked this
 * out first and still owns the WRITE-side hold that sits in front of it (a list
 * it has sent but not yet seen echoed); this is only the part that turns a
 * stored value into references, which every read surface needs.
 *
 * Blank and non-string elements are DROPPED rather than rendered. The write
 * doors refuse both outright (`internal/items/validate.go`, the multi_relation
 * arm — an empty element is an error, not a skip, precisely so an ordered
 * list's length cannot depend on which elements were blank), so this is
 * defence against a value no door will accept, not a policy of its own.
 *
 * A NON-ARRAY value on a `multi_relation` yields NOTHING, deliberately: the
 * type was changed under a stored scalar, and one arbitrary element is a worse
 * answer than an empty cell, which at least reads as "this field has nothing
 * this view can show".
 */
export function relationValuesOf(type: string | undefined | null, value: unknown): string[] {
	if (!isRelationType(type)) return [];
	if (isMultiRelationType(type)) {
		if (!Array.isArray(value)) return [];
		return value
			.map((entry) => (typeof entry === 'string' ? entry.trim() : ''))
			.filter((entry) => entry !== '');
	}
	const raw = typeof value === 'string' ? value.trim() : '';
	return raw ? [raw] : [];
}
