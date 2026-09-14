// The `status` a create should start with (BUG-3078).
//
// Six create paths independently did this:
//
//     const statusField = schema.fields.find((f) => f.key === 'status');
//     if (statusField?.options?.length) {
//         defaultFields.status = statusField.options[0];
//     }
//
// Nothing asked whether `status` could still HOLD a string. `options` survives a
// retype — the premise BUG-3057, BUG-3067, BUG-3068 and BUG-3074 all turn on —
// so on a collection whose `status` was retyped to `multi_select`, `number` or
// `checkbox` while keeping its options, every one of those sites sent a scalar
// into a field that cannot hold it and the server REFUSED the create outright.
// Not a degraded create: no item, an error toast, and no route to a good
// outcome. Quick capture simply stopped working on that collection.
//
// ONE MODULE for the same reason `laneKeyCallers.test.ts` exists: six sites
// asking one question in six places is how the question comes to have six
// answers. `createDefaultCallers.test.ts` pins that they all call this.
//
// WHAT EACH ARM DOES, and why it is not uniform:
//
//   * The conversion is `laneWriteValue`'s, not a second copy of it. For a
//     `multi_select` it yields `['open']` rather than `'open'`, which the server
//     ACCEPTS and stores correctly typed (measured against the create handler,
//     not assumed — see the BUG-3078 trail for the four-arm probe).
//   * Where `laneWriteValue` REFUSES — `number`, `checkbox` — the key is OMITTED
//     and the server applies the collection's own schema default. Omitting is
//     the only remaining move: a create has no lane to refuse into, and failing
//     the create is the behaviour being fixed.
//   * REFERENCE-SHAPED types are omitted HERE rather than by `laneWriteValue`,
//     and that asymmetry is the one thing in this module that is not obvious.
//     `laneWriteValue` answers "can this LANE KEY become a value", and a lane
//     key for a relation is ALREADY the target's item id — its own comment says
//     so — which is why it passes `relation` straight through. This caller does
//     not hold a lane key. It holds `options[0]`, a leftover `select` option,
//     which is never an item id. So the same string is a good value there and a
//     bad one here, and changing `laneWriteValue` to refuse it would break the
//     lane path it was written for.
//
//     Both arms measured against the create handler rather than reasoned about:
//     `relation` answers 400 `"open" does not name an item in collection
//     "tasks"`, and `multi_relation` answers 400 `must be an array of strings
//     (item IDs, refs, or exact titles)`. There is no honest conversion for
//     either — wrapping it as `['open']` passes the shape check and is then
//     refused by referent validation, because the option names no item — so the
//     key is omitted and the collection's own default applies.
//
//     Codex found `multi_relation`; `relation` is the same class and was found
//     by asking what else reaches that branch (CONVE-18).
//
// A CAVEAT THE OMIT ARM INHERITS, recorded because it is invisible from here.
// `ValidateFieldsDetailed` assigns a schema default and skips its own type
// check, so a collection whose stale SCALAR default also survived the retype
// stores that scalar unvalidated when the key is omitted. That is a server-side
// hole with its own item, reachable today by any caller that creates without a
// status (CLI, MCP, plain API) — this module routes onto it rather than opening
// it. It is still strictly better than the refusal it replaces, because the
// alternative is no item at all.
import type { Collection } from '$lib/types';
import { parseSchema } from '$lib/types';
import { laneWriteValue } from './laneWriteValue';

/** Field types whose values are item references, never a declared option. */
const REFERENCE_TYPES = new Set(['relation', 'multi_relation']);

/**
 * The `fields` object a new item should be created with.
 *
 * Returns an EMPTY object rather than omitting `status` selectively, so callers
 * keep their existing `JSON.stringify(defaultFields)` shape and no caller has to
 * know which arm it landed on.
 */
export function createDefaultFields(collection: Collection | null | undefined): Record<string, unknown> {
	const defaults: Record<string, unknown> = {};
	if (!collection) return defaults;

	const statusField = parseSchema(collection).fields.find((f) => f.key === 'status');
	const first = statusField?.options?.[0];
	if (first === undefined) return defaults;

	// See the header: a stale `select` option is never an item id, so no
	// reference-shaped field can be defaulted from one.
	if (REFERENCE_TYPES.has(statusField?.type ?? '')) return defaults;

	const write = laneWriteValue(statusField, first);
	if (write.ok) defaults.status = write.value;
	return defaults;
}
