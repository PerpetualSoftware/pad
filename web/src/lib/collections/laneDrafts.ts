// Where an unsaved lane draft goes when it is saved (BUG-3043).
//
// A lane draft (TASK-1676) lives on the collection PAGE, keyed by the lane
// value it was typed in, so it survives a board↔list switch. Nothing tied that
// key to a lane that still EXISTS: rename or delete the option, retype the
// grouping field, or regroup the board by another field, and the draft's card
// vanished while its text stayed in the map. "Save all" then either sent the
// dead lane value — the server refused it and the dialog could not complete,
// blocked by a draft the user could not see — or, where the client refused the
// value first, reported success and deleted the text.
//
// Lead ruling (RE-HOME; typed text is never dropped): an orphaned draft moves
// to the Uncategorized lane, visibly marked with the lane it came from, and is
// saved there. Where Uncategorized cannot actually RECEIVE it — the create
// would land somewhere else, or be refused — the draft is kept in place of a
// guess, a notice names the lost lane, and Save all stays blocked for that
// reason.
//
// "Can Uncategorized receive it" is a property of the FIELD, not of the lane,
// and it follows what the server stores (internal/items/validate.go). Those
// server facts are PINNED from the Go side by TestUncategorizedCreateFacts_BUG3043
// (internal/items/lane_draft_bug3043_test.go), so a validator change that would
// make this mirror wrong fails there rather than drifting in silence:
//   * select / text-like: `''` is stored even on a REQUIRED field (required
//     fires only on an absent or null key; `''` skips the options check), and
//     `''` is exactly the Uncategorized lane.
//   * multi_select: `[]` is stored — Uncategorized.
//   * relation / multi_relation: a blank is normalised to ABSENT (v0.40), so
//     the key is omitted rather than sent as a string the write door would
//     have to interpret.
//   * number / checkbox: `''` is not a value, so the key is omitted.
// An omitted key is Uncategorized only if nothing fills it in: a client
// default (`createDefaultFields`) or a schema default puts the item in a real
// lane, and `required` with neither refuses the create.
import type { FieldDef } from '$lib/types';
import { UNCATEGORIZED, formatLaneLabel } from './boardColumns';
import { laneWriteValue, laneWriteRefusalMessage } from './laneWriteValue';
import { relationGroupingRefusal } from './relationGroups';

const REFERENCE_TYPES = new Set(['relation', 'multi_relation']);

/**
 * The lanes a draft can be typed in — the ones BoardView offers "+" on. Its
 * `columns` are `field.options` for every grouping that is neither refused nor
 * a relation (whose lanes are items, and which offer no "+"), so this is that
 * same set.
 */
export function draftableLanes(field: FieldDef | undefined | null): Set<string> {
	if (!field) return new Set();
	if (relationGroupingRefusal(field)) return new Set();
	if (field.type === 'relation') return new Set();
	return new Set(field.options ?? []);
}

/** What a create from the Uncategorized lane writes to the group field. */
export type UncategorizedWrite = { send: true; value: unknown } | { send: false };

export function uncategorizedWrite(field: FieldDef | undefined | null): UncategorizedWrite {
	if (field && REFERENCE_TYPES.has(field.type)) return { send: false };
	const write = laneWriteValue(field, UNCATEGORIZED);
	if (!write.ok || write.value === null) return { send: false };
	return { send: true, value: write.value };
}

export type BlockedReason = 'required' | 'defaulted';

/**
 * A draft's map key: the GROUP FIELD it was typed under, and the lane
 * (BUG-3214). A bare lane value said nothing about which field it belonged to,
 * and the map is not cleared on a regroup, so a draft typed in status's `open`
 * lane was read, after regrouping by a field that also has an `open` option, as
 * THAT field's live lane and saved into it. The separator is the ASCII unit
 * separator, which no schema option is expected to contain.
 */
const DRAFT_KEY_SEP = '\u001f';

export function draftKey(field: string, lane: string): string {
	return `${field}${DRAFT_KEY_SEP}${lane}`;
}

/** The inverse of `draftKey`. A key without a field (a bare lane) is read as one. */
export function parseDraftKey(key: string): { field: string | null; lane: string } {
	const i = key.indexOf(DRAFT_KEY_SEP);
	return i < 0 ? { field: null, lane: key } : { field: key.slice(0, i), lane: key.slice(i + 1) };
}

export type DraftSaveTarget =
	/** The draft's lane is still offered: save it there, as always. */
	| { kind: 'lane'; lane: string }
	/** The lane is gone: the draft shows in, and saves to, Uncategorized. */
	| { kind: 'rehomed'; lostLane: string; lostField?: string }
	/** The lane is gone and Uncategorized cannot receive the create. */
	| { kind: 'blocked'; lostLane: string; reason: BlockedReason; lostField?: string };

/**
 * Where the draft keyed `key` (a `draftKey`, or a bare lane) saves to.
 *
 * A draft typed under ANOTHER group field is orphaned whatever its lane is
 * called (BUG-3214): its lane belongs to that field, and a same-named lane of
 * the current one is a coincidence, not its home. `lostField` names that field
 * so the mark can say where the draft came from.
 *
 * `clientDefaults` is what the page's create pre-fills (`createDefaultFields`),
 * because an omitted group key is only Uncategorized if that does not fill it.
 */
export function draftSaveTarget(
	key: string,
	field: FieldDef | undefined | null,
	clientDefaults: Record<string, unknown>,
): DraftSaveTarget {
	const { field: typedUnder, lane } = parseDraftKey(key);
	const otherField = typedUnder !== null && typedUnder !== (field?.key ?? null);
	const lost = otherField ? { lostLane: lane, lostField: typedUnder } : { lostLane: lane };
	if (!otherField && draftableLanes(field).has(lane)) return { kind: 'lane', lane };
	const write = uncategorizedWrite(field);
	if (write.send || !field) return { kind: 'rehomed', ...lost };
	const filled =
		(Object.hasOwn(clientDefaults, field.key) && clientDefaults[field.key] != null) ||
		field.default != null;
	if (filled) return { kind: 'blocked', ...lost, reason: 'defaulted' };
	if (field.required) return { kind: 'blocked', ...lost, reason: 'required' };
	return { kind: 'rehomed', ...lost };
}

/**
 * How a lost lane is named to the user: the lane, plus the field it belonged to
 * when that is not the board's current grouping (BUG-3214). `labelFor` maps a
 * field key to its label.
 */
export function lostLaneLabel(
	target: { lostLane: string; lostField?: string },
	labelFor: (fieldKey: string) => string,
): string {
	const lane = formatLaneLabel(target.lostLane);
	return target.lostField ? `${lane} (${labelFor(target.lostField)})` : lane;
}

/** The notice for a draft that cannot be saved, naming the lane it lost. */
export function blockedDraftMessage(lostLaneLabel: string, fieldLabel: string, reason: BlockedReason): string {
	const why =
		reason === 'required'
			? `${fieldLabel} is required, so it can't be saved without a lane`
			: `saving it without a lane would give ${fieldLabel} its default value instead`;
	return `An unsaved draft was in the “${lostLaneLabel}” lane, which no longer exists. It can't move to Uncategorized: ${why}. Discard it, or restore the lane.`;
}

/**
 * The fields a draft's create sends: `clientDefaults` plus the group field as
 * the target decides, or the refusal to send at all. A refusal carries the
 * message the user sees; the caller THROWS it, because a throw is what keeps
 * the draft's text on every path that saves one.
 */
export function draftCreateFields(
	key: string,
	field: FieldDef | undefined | null,
	groupKey: string,
	clientDefaults: Record<string, unknown>,
	labelFor: (fieldKey: string) => string = (k) => k,
): { ok: true; fields: Record<string, unknown> } | { ok: false; message: string } {
	const label = field?.label || groupKey;
	const target = draftSaveTarget(key, field, clientDefaults);
	if (target.kind === 'blocked') {
		return { ok: false, message: blockedDraftMessage(lostLaneLabel(target, labelFor), label, target.reason) };
	}
	const fields = { ...clientDefaults };
	if (target.kind === 'rehomed') {
		const write = uncategorizedWrite(field);
		if (write.send) fields[groupKey] = write.value;
		return { ok: true, fields };
	}
	// Converted through the declared type, for the reason on the drag path
	// (BUG-3057): a `0` lane of a number field used to send the string "0".
	const laneWrite = laneWriteValue(field, target.lane);
	if (!laneWrite.ok) return { ok: false, message: laneWriteRefusalMessage(laneWrite.reason, label) };
	// A null is the PATCH path's delete sentinel and means nothing on create —
	// the field is simply absent from a new item.
	if (laneWrite.value !== null) fields[groupKey] = laneWrite.value;
	return { ok: true, fields };
}

export type SaveAllOutcome = 'done' | 'failed' | 'identity_moved';

/**
 * The leave dialog's "Save all": create each non-empty draft in turn, clearing
 * EACH as its create lands (so a retry after a partial failure cannot create
 * the saved ones twice), and stop at the first that does not land.
 *
 * "Does not land" includes a create that RETURNED null with the identity held
 * (BUG-3043). That used to count as a save, and the draft was deleted behind
 * the toast its refusal had shown. The identity check comes first, because a
 * continuation that lost the identity may write nothing at all (BUG-3084).
 */
export async function saveAllDrafts(
	draftText: Record<string, string>,
	create: (lane: string, title: string) => Promise<unknown>,
	identityHeld: () => boolean,
	clear: (lane: string) => void,
): Promise<SaveAllOutcome> {
	for (const [lane, text] of Object.entries(draftText)) {
		const title = text.trim();
		if (!title) continue;
		let created: unknown;
		try {
			created = await create(lane, title);
		} catch {
			return identityHeld() ? 'failed' : 'identity_moved';
		}
		if (!identityHeld()) return 'identity_moved';
		if (created === null || created === undefined) return 'failed';
		clear(lane);
	}
	return 'done';
}

/**
 * Every non-empty draft's target, keyed by its lane. Empty drafts are skipped:
 * they are not unsaved work, and Save all skips them too.
 */
export function draftTargets(
	draftText: Record<string, string>,
	field: FieldDef | undefined | null,
	clientDefaults: Record<string, unknown>,
): Record<string, DraftSaveTarget> {
	const out: Record<string, DraftSaveTarget> = {};
	for (const [lane, text] of Object.entries(draftText)) {
		if (!text?.trim()) continue;
		out[lane] = draftSaveTarget(lane, field, clientDefaults);
	}
	return out;
}
