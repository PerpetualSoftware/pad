// Grouping a board or list by a RELATION field (TASK-2998 / PLAN-2857 U7).
//
// Every other groupable field type carries its own lane vocabulary: a `select`
// has `options`, and `bucketByColumn` buckets against that fixed list. A
// relation has no options — its lanes are whatever target items the rows
// actually point at — so the lanes have to be DERIVED from the data, and their
// labels resolved against the local index.
//
// WHY A PURE MODULE, like `unparentedFilter` and `boardColumns` beside it: the
// rules below (which lanes exist, how each is labelled, which lane a dangling
// value lands in, which lanes accept a drop) are decisions, and a decision that
// can only be exercised by mounting a board view is a decision nobody tests.
// Nothing here imports a rune.

import type { Item, ItemIndexRow } from '$lib/types';
import { formatItemRef, parseFields } from '$lib/types';
import { UNCATEGORIZED } from './boardColumns';

/**
 * The lane a value with no resolvable target lands in.
 *
 * Deliberately NOT a bare UUID and not the stored string. A relation value that
 * resolves to nothing is usually free text an older, unvalidated write left
 * behind (the pre-TASK-2878 fallback accepted any string), so showing it as
 * though it were a lane name would present a typo as a category. `FieldEditor`
 * says "Unresolved reference" for exactly this state; the lane says the same.
 *
 * Reserved with a `$` prefix for the reason `$unparented` is: a real field
 * value can never collide with it, because schema validation refuses new field
 * keys starting with `$` and an item id is a UUID.
 */
export const UNRESOLVED_LANE = '$unresolved';

export type RelationLaneState = 'empty' | 'live' | 'deleted' | 'unresolved';

/**
 * How a single relation VALUE presents — the lane header and the filter chip
 * say the same things about the same value, so they are built from one
 * function rather than two that drift.
 */
export interface RelationChip {
	value: string;
	ref: string | null;
	title: string | null;
	/** What to say when there is no ref/title — never the stored value. */
	label: string;
	state: RelationLaneState;
}

/**
 * Describe one relation value. `null` for an empty value (the caller decides
 * what "no filter" or "uncategorised" looks like); an unresolvable value
 * becomes the honest unresolved chip rather than the string it holds.
 */
export function relationChipFor(value: string, resolve: ResolveRow): RelationChip | null {
	const raw = typeof value === 'string' ? value.trim() : '';
	if (!raw) return null;
	const row = resolve(raw);
	if (!row) {
		return {
			value: UNRESOLVED_LANE,
			ref: null,
			title: null,
			label: 'Unresolved reference',
			state: 'unresolved',
		};
	}
	return {
		value: raw,
		ref: formatItemRef(row),
		title: row.title ?? null,
		label: row.title ?? '',
		state: row.deleted_at ? 'deleted' : 'live',
	};
}

/** A lane IS a chip plus its position on the board. Same vocabulary. */
export type RelationLane = RelationChip;

/** Resolve an item id against whatever row source the caller has. */
export type ResolveRow = (id: string) => ItemIndexRow | null | undefined;

/**
 * The two narrowings a relation value needs on top of a workspace-wide lookup.
 *
 * `localIndex.findByIdOrSlug` resolves by id OR SLUG and searches the whole
 * workspace, and both of those are wrong for a relation:
 *
 *  1. ID ONLY. A legacy free-text value like "red" — exactly what the
 *     pre-TASK-2878 fallback wrote — resolves to whatever item is SLUGGED
 *     "red" and would render as a working reference. The field stores an item
 *     id; a slug match makes the label lie about what is stored, and slugs are
 *     mutable, so the same value could point somewhere else tomorrow.
 *  2. TARGET COLLECTION. Without it, a relation declared against `colors` can
 *     label a lane with an item from `tasks` that happens to share an
 *     identifier.
 *
 * The collection check only fires when both the declared target and the row's
 * own collection are slugs the current list knows — a renamed collection, or a
 * rename applied to the index before the collection list caught up, are not the
 * value's fault and must not turn every lane unresolved.
 *
 * WORKED OUT FIRST IN `FieldEditor.svelte`'s relation branch, which now calls
 * this rather than carrying its own copy. It was briefly duplicated — that file
 * was being edited under TASK-2996 when the board needed the same rule — and
 * the fork was closed as soon as 2996 merged without touching it. The reason to
 * keep it single is concrete: a divergence shows up as a board lane and a
 * properties chip disagreeing about what the same id is called.
 */
export function narrowRelationRow(
	row: ItemIndexRow | null | undefined,
	value: string,
	declaredCollection: string | undefined,
	// `null` accepted as well as omitted: FieldEditor's own set is
	// `Set<string> | null` — null while the collection list has not loaded —
	// and "we do not know the collections yet" is exactly the case this
	// argument's guard already treats as "do not judge".
	knownCollectionSlugs?: ReadonlySet<string> | null,
): ItemIndexRow | null {
	if (!row) return null;
	if (row.id !== value) return null;
	if (
		declaredCollection &&
		knownCollectionSlugs?.has(row.collection_slug ?? '') &&
		knownCollectionSlugs.has(declaredCollection) &&
		row.collection_slug !== declaredCollection
	) {
		return null;
	}
	return row;
}

/**
 * TRIMMED (codex round 1, P1). `relationChipFor` has always trimmed, so a
 * legacy value stored as `" id-red "` produced a live CHIP and an unresolved
 * LANE — the same value described two ways by the two functions this module
 * exists to keep in agreement. The pre-TASK-2878 write path accepted any
 * string, padding included, so those rows exist.
 */
function laneValue(raw: unknown): string {
	return typeof raw === 'string' ? raw.trim() : '';
}

/**
 * The lanes a relation-grouped view should render, in display order.
 *
 * Derived from the ITEMS rather than from the schema, because a relation field
 * has no option list — the lanes are the targets the rows point at. An empty
 * result means nothing in view carries a value, which the caller renders as the
 * uncategorised lane alone.
 *
 * ORDERING: live lanes first, alphabetically by title, then deleted ones, then
 * the single unresolved lane. Alphabetical rather than first-appearance because
 * items arrive asynchronously through the local index, and a first-appearance
 * order would reshuffle the board as they land.
 *
 * DELETED TARGETS KEEP THEIR OWN LANE. They still have a ref and a title, so
 * they can be labelled honestly, and merging them would silently combine rows
 * that point at different things. Only values that resolve to NOTHING share a
 * lane, because there is nothing to tell them apart with.
 */
export function relationLanes(items: Item[], fieldKey: string, resolve: ResolveRow): RelationLane[] {
	const seen = new Set<string>();
	const live: RelationLane[] = [];
	const deleted: RelationLane[] = [];
	let hasUnresolved = false;

	for (const item of items) {
		const value = laneValue(parseFields(item)[fieldKey]);
		if (!value || seen.has(value)) continue;
		seen.add(value);
		const chip = relationChipFor(value, resolve);
		if (!chip || chip.state === 'unresolved') {
			hasUnresolved = true;
			continue;
		}
		(chip.state === 'deleted' ? deleted : live).push(chip);
	}

	const byTitle = (a: RelationLane, b: RelationLane) =>
		(a.title ?? '').localeCompare(b.title ?? '');
	live.sort(byTitle);
	deleted.sort(byTitle);

	const lanes = [...live, ...deleted];
	if (hasUnresolved) {
		lanes.push({
			value: UNRESOLVED_LANE,
			ref: null,
			title: null,
			label: 'Unresolved reference',
			state: 'unresolved',
		});
	}
	return lanes;
}

/**
 * The value each item should be bucketed on, with unresolvable values folded
 * onto the sentinel.
 *
 * `bucketByColumn` routes any value not in its column list to UNCATEGORIZED,
 * which would put "points at a deleted item" and "points at nothing" in the
 * same lane as "has no value at all" — three different facts. Rewriting the
 * value first keeps them apart while leaving that helper's own rule intact:
 * every item still lands in exactly one lane.
 */
export function relationLaneValueFor(item: Item, fieldKey: string, resolve: ResolveRow): string {
	const value = laneValue(parseFields(item)[fieldKey]);
	if (!value) return UNCATEGORIZED;
	return resolve(value) ? value : UNRESOLVED_LANE;
}

/**
 * The lane label for assistive technology — the same words the eye gets.
 *
 * The board's group `aria-label` used to format the raw column VALUE, so a lane
 * reading "COLOR-1 Red" announced as "Id-Red column" and the unresolved lane as
 * "$Unresolved column" (codex round 1). The claim this unit makes is that a
 * lane is named from its target rather than from the stored id, and a claim
 * that holds only for sighted users is not the claim.
 */
export function relationLaneAriaName(lane: RelationLane | undefined, fallback: string): string {
	if (!lane) return fallback;
	const parts = [lane.ref, lane.title ?? lane.label].filter(Boolean);
	const name = parts.join(' ');
	return lane.state === 'deleted' ? `${name} (deleted)` : name;
}

/**
 * Can a board drop write this lane's value onto an item?
 *
 * A live lane is a legitimate drop target — dragging a card there sets the
 * relation to that item, which is the natural board gesture. The other three
 * are not: UNCATEGORIZED would clear the field (a delete disguised as a move),
 * and the deleted and unresolved lanes would write a reference the validator
 * refuses anyway (TASK-2878 — a relation value must name a LIVE item in the
 * declared collection), so the drop would fail at the server after moving the
 * card on screen.
 */
export function relationLaneAcceptsDrop(lane: Pick<RelationLane, 'state'>): boolean {
	return lane.state === 'live';
}

/**
 * Does this item's value for a RELATION field match a filter value?
 *
 * Strict `===` was the whole comparison, and the board had already started
 * trimming — so an item storing `" id-red "` appeared in the `Red` LANE and
 * vanished when you filtered for `Red` (codex round 2). The same value, two
 * answers, one screen apart.
 *
 * Trimming on BOTH sides rather than normalising the stored data: the write
 * path has refused padded values since TASK-2878, so there is nothing new to
 * clean up and nothing to migrate — only old rows to read correctly.
 *
 * Scalar only, deliberately. `multi_relation` (U4) stores an ARRAY and no
 * amount of trimming makes `===` match one; matching an array is that unit's
 * to define, and guessing here would fix half of it in a way U4 would have to
 * undo.
 */
export function relationFilterMatches(stored: unknown, filterValue: string): boolean {
	if (typeof stored !== 'string') return false;
	return stored.trim() === filterValue.trim();
}
