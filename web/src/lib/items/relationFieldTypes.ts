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
