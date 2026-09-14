// Turning a board/list LANE KEY back into a field VALUE (BUG-3057).
//
// A lane key is a STRING — `laneValue` in `boardColumns` projects any stored
// value onto one, which is what makes a view able to group by anything. The
// write paths then assigned that string straight back to the field, whatever
// the field's declared type. That projection has no free inverse: `'0'` came
// from `0`, but the key alone cannot say whether the field wanted `0`, `'0'` or
// `false`. The declared TYPE is what disambiguates, so the conversion belongs
// here rather than inline at either call site.
//
// WHAT THE SERVER ACTUALLY DOES, measured rather than assumed (probe against
// `internal/items.ValidatePartialFields`, the validator behind `fields_patch`):
//
//     number       "0"     -> field "score" must be a number
//     number       ""      -> field "score" must be a number
//     number       nil     -> ok   (nil is the patch path's DELETE sentinel)
//     checkbox     "false" -> field "shipped" must be a boolean
//     multi_select "c"     -> field "tags" must be an array of strings
//     multi_select ["c"]   -> ok
//     multi_select []      -> ok
//     select       ""      -> ok   (clearing a select is a legal write)
//
// So the pre-fix symptom is NOT the silent retyping the item was filed as: the
// server REFUSES these writes. A legitimate drag between lanes, and a
// legitimate create-in-lane, simply FAIL on any board whose group field is not
// string-shaped. Nothing is corrupted; the user's move does not take.
//
// WHICH TYPES ARE REACHABLE. The display-settings picker offers `select` AND
// `multi_select` (EditCollectionModal's `selectFieldKeys`), so a multi_select
// board is reachable with no retype at all. Everything else arrives the way
// BUG-3041 and BUG-3016's cases arrive — a field retyped under a setting that
// still names it, or a `board_group_by` written through the API.
import type { FieldDef } from '$lib/types';
import { UNCATEGORIZED } from './boardColumns';

/**
 * Why a lane key cannot become a value for this field.
 *
 * Only reachable when the lane key and the declared type disagree about what
 * the field holds — a stale lane from before a retype, or a `board_group_by`
 * pointing at a field whose values were never lane keys.
 */
export type LaneWriteRefusal = 'not_a_number' | 'not_a_boolean';

export type LaneWrite = { ok: true; value: unknown } | { ok: false; reason: LaneWriteRefusal };

/**
 * The value to write for a drop into `laneKey`, or a refusal.
 *
 * UNCATEGORIZED (`''`) means "no value", and the honest spelling of that
 * differs by type: `''` for a string-shaped field (a legal, established clear —
 * `boardColumns` documents the empty lane as writing it back), `[]` for a
 * multi_select, and the patch path's DELETE sentinel `null` for a number or a
 * checkbox, which have no empty string to hold.
 *
 * An UNDECLARED field (no schema entry) keeps today's behaviour and writes the
 * key: orphan keys are accepted and persist as-is server-side, and this module
 * has nothing better to go on than the string it was handed.
 */
export function laneWriteValue(field: FieldDef | undefined | null, laneKey: string): LaneWrite {
	const type = field?.type;
	if (!field || !type) return { ok: true, value: laneKey };

	switch (type) {
		case 'number': {
			if (laneKey === UNCATEGORIZED) return { ok: true, value: null };
			// `Number('')` is 0 and `Number(' ')` is 0, which would turn an
			// unlabelled lane into a real zero; the empty case is handled above
			// and whitespace is not a number anyone typed.
			const n = laneKey.trim() === '' ? NaN : Number(laneKey);
			if (!Number.isFinite(n)) return { ok: false, reason: 'not_a_number' };
			return { ok: true, value: n };
		}
		case 'checkbox': {
			if (laneKey === UNCATEGORIZED) return { ok: true, value: null };
			// Exactly the two spellings `laneValue(true)` / `laneValue(false)`
			// produce. Nothing else is accepted: 'yes', '1' and 'on' are guesses
			// about what a user meant, and this seam never had a user typing.
			if (laneKey === 'true') return { ok: true, value: true };
			if (laneKey === 'false') return { ok: true, value: false };
			return { ok: false, reason: 'not_a_boolean' };
		}
		case 'multi_select': {
			// A LANE IS A COMBINATION, so the write is the combination — which
			// makes this the one arm that has to INVERT the projection rather
			// than wrap the key.
			//
			// `laneValue(['a','b'])` is `'a,b'`, and ListView MINTS a lane for
			// every distinct value it finds, so a list grouped by a multi_select
			// really does show an `a,b` lane (measured; the board instead seeds
			// its lanes from the field's options, so the same item lands in
			// UNCATEGORIZED there). Dropping onto that lane means "give this item
			// that lane's tags". Wrapping the key would have written the single
			// tag `"a,b"`, which is not an option and not a tag anyone has.
			//
			// Resolved against the DECLARED OPTIONS rather than split blindly,
			// so an option that CONTAINS a comma is not torn in half:
			//   1. the key IS an option        -> that one option
			//   2. every comma-part is an option -> those options, in lane order
			//   3. no options declared         -> split, the plain inverse of the
			//      join, since there is no vocabulary to check against
			//   4. otherwise                   -> the key as a single value, which
			//      is what a lane minted from an undeclared value means
			//
			// Rule 1 before rule 2 is a deliberate tie-break: with options
			// `['a,b','a','b']` the lane `a,b` is genuinely ambiguous — the
			// projection is lossy — and the reading that names a REAL option beats
			// the one that reconstructs two.
			if (laneKey === UNCATEGORIZED) return { ok: true, value: [] };
			const options = field.options ?? [];
			if (options.length === 0) {
				return { ok: true, value: laneKey.split(',').filter((p) => p !== '') };
			}
			if (options.includes(laneKey)) return { ok: true, value: [laneKey] };
			const parts = laneKey.split(',');
			if (parts.length > 1 && parts.every((p) => options.includes(p))) {
				return { ok: true, value: parts };
			}
			return { ok: true, value: [laneKey] };
		}
		default:
			// `select`, `text`, `url`, `date`, `relation`, `json` — all hold a
			// string, and a relation lane key is already the target's item id
			// (the relation lane path resolves it before it gets here).
			return { ok: true, value: laneKey };
	}
}

/** What to tell the user when a lane key cannot become a value. */
export function laneWriteRefusalMessage(reason: LaneWriteRefusal, fieldLabel: string): string {
	return reason === 'not_a_number'
		? `Can't move: ${fieldLabel} holds a number and this lane isn't one`
		: `Can't move: ${fieldLabel} holds a checkbox and this lane isn't true or false`;
}

/**
 * May the board lane menu offer `laneKey` as a bulk "Move all to" destination?
 * (BUG-3074.)
 *
 * The bulk endpoint's `move` verb carries its destination as `Status string`
 * (`internal/server/handlers_items_bulk.go`), so the ONLY writes it can express
 * are the ones `laneWriteValue` resolves to a string. That is the question this
 * asks, and it is asked through `laneWriteValue` rather than against a type list
 * so that the menu cannot drift from the drag and quick-create paths. That drift
 * is a real failure mode rather than a hypothetical one: `laneKeyCallers.test.ts`
 * exists because the same question about a lane KEY had already grown four
 * private spellings across three surfaces, which had drifted to different
 * answers.
 *
 * Two shapes are refused, and they fail differently on the way in:
 *
 *   * `number` / `checkbox` — `laneWriteValue` already refuses these outright.
 *     Reachable because a retype does NOT strip the field's `options`, so a
 *     `status` retyped away from `select` keeps offering its old lanes. The
 *     `moveTargets.length > 0` guard covers a field that never had options and
 *     covers nothing at all for one that did.
 *   * `multi_select` — `laneWriteValue` SUCCEEDS here and returns an array, so
 *     the refusal is not its refusal but this one: an array cannot ride a
 *     `string` field. Withheld rather than converted, and that is a semantic
 *     call rather than a plumbing one. "Move all to X" has no honest meaning on
 *     a multi-valued field: an item can sit in several lanes at once, so the
 *     verb would have to choose between REPLACING the whole list and swapping
 *     one element, and the menu has no way to say which it did. A grouping
 *     refusal is what a `multi_relation` gets for the same reason.
 *
 * `select` and the string-shaped types (`text`, `url`, `date`, `json`) pass, as
 * does an UNDECLARED field, which `laneWriteValue` documents as writing the key.
 */
export function laneKeyIsBulkMovable(field: FieldDef | undefined | null, laneKey: string): boolean {
	const write = laneWriteValue(field, laneKey);
	return write.ok && typeof write.value === 'string';
}
