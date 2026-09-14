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
			// ONE element, not a merge. A multi_select board's lanes come from
			// the field's options, so an item with two tags matches no lane and
			// already sits in UNCATEGORIZED; dropping it into a lane is the user
			// saying the item belongs THERE. Replacing the list is lossy for
			// that item, and it is what the gesture means — the alternative is
			// to refuse the drop, which is a product decision and not this
			// module's to take silently.
			if (laneKey === UNCATEGORIZED) return { ok: true, value: [] };
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
