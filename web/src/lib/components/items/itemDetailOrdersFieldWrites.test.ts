// Node-project test (no DOM): a SOURCE-level guard that the item pane's field
// saver is WIRED to the write ordering it is supposed to use (PLAN-2857 U4,
// codex round 8, R8-1).
//
// WHY A SOURCE GUARD. The ordering itself is tested for real in
// `$lib/items/fieldWriteOrder.test.ts`, which drives the actual retry loop
// through the interleaving that loses the update, with a negative control for
// each single-counter alternative. What that CANNOT say is that `ItemDetail`
// calls it — CONVE-19's split: a direct-call test vouches for the component,
// never for its binding. ItemDetail is ~7,900 lines of collab, SSE and pane
// wiring and is not renderable in a unit test (the two sibling guards in this
// directory say the same).
//
// WHAT A SOURCE GUARD CANNOT DO, stated so it is not mistaken for proof: it
// checks spellings, not behaviour. It would not catch a gate comparing the
// wrong two values, or one made unreachable by an earlier return. It catches
// the thing that actually goes wrong over time — a new branch that assigns
// `item` from a response, or a second hand-rolled retry loop, added without the
// gate.
import { describe, expect, it } from 'vitest';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';

const SRC = readFileSync(resolve(__dirname, './ItemDetail.svelte'), 'utf8');

/** Strip line comments so a guard cannot be satisfied by prose. */
function stripLineComments(src: string): string {
	return src.replace(/^[ \t]*\/\/.*$/gm, '');
}

/** `updateField`'s body, which is the only place these rules apply. */
const UPDATE_FIELD = (() => {
	const start = SRC.indexOf('async function updateField(');
	const end = SRC.indexOf('function updateTags(', start);
	if (start < 0 || end < 0) throw new Error('updateField no longer bounded by updateTags — re-anchor this guard');
	return stripLineComments(SRC.slice(start, end));
})();

describe('ItemDetail field writes are ordered', () => {
	it('takes a ticket for THIS field of THIS item', () => {
		// Per (item, field), not per item: `fields_patch` carries one key and the
		// server merges it, so a status change must not be abandoned because a
		// relation chip was clicked, and an item-wide counter would do exactly
		// that. Killed by retargeting the key — the mutation run's M8.
		//
		// There is deliberately NO assertion that the ticket is taken before the
		// first await. It cannot be otherwise: the ticket is read inside the
		// submit loop's own configuration, so any placement after the send fails
		// to compile. A leg for it survived its mutant (M6) because the only
		// mutation available was a behaviour-preserving move, which is a fact
		// about the mutant, not a weakness in the test — so the leg is gone
		// rather than left as rigour nothing can falsify.
		expect(UPDATE_FIELD).toMatch(
			/const ticket = fieldWrites\.take\(fieldWriteTarget\(targetItem\.id, key\)\)/
		);
	});

	it('submits through the ordered OCC loop, carrying that ticket', () => {
		// The loop under test in fieldWriteOrder.test.ts has to be the loop that
		// runs here. A second, hand-rolled `for`/`catch` retry in this function
		// would pass every other assertion in this file while replaying exactly
		// the stale body R8-1 is about.
		expect(UPDATE_FIELD).toMatch(/submitOrderedOCC<Item>\(\{/);
		const call = UPDATE_FIELD.slice(UPDATE_FIELD.indexOf('submitOrderedOCC<Item>({'));
		expect(call.slice(0, call.indexOf('});'))).toMatch(/order: fieldWrites/);
		expect(call.slice(0, call.indexOf('});'))).toMatch(/\bticket,/);
		expect(UPDATE_FIELD).not.toMatch(/isUpdateConflictError\(e\)\s*&&/);
	});

	it('opens its failure handling by asking whether it was superseded', () => {
		// A superseded write's error is not news — the newer write owns the toast
		// and the save indicator. This must be FIRST, or the open-children dialog
		// and the conflict toast fire for a write the user has already replaced.
		const catchAt = UPDATE_FIELD.indexOf('} catch (e) {');
		expect(catchAt).toBeGreaterThan(-1);
		const firstStatement = UPDATE_FIELD.slice(catchAt)
			.split('\n')
			.map((l) => l.trim())
			.filter(Boolean)[1];
		expect(firstStatement).toBe('if (fieldWrites.superseded(ticket)) return;');
	});

	it('gates every write of `item` on claiming the ticket, with no await in between', () => {
		// The display half. Two responses for one field can resolve in either
		// order and the older row looks exactly as healthy as the newer one, so
		// each assignment claims first — and claims with nothing awaited in
		// between, since an await after the claim reopens the window it closed.
		//
		// The 300-character bound is a proximity heuristic, not a scope analysis.
		// Measured on the guarded tree: the five assignments sit 35, 35, 60, 156
		// and 207 characters after their gate (the last two are the
		// OCC-exhausted branch, whose one gate covers an if/else). Deleting that
		// branch's gate pushes its two assignments to 359 and 410, so the bound
		// has to sit between 207 and 359. It was 400 first, which caught only the
		// second of the two and by ten characters — a margin any edit to the
		// comment-free text in between would have erased.
		//
		// It is here to fail on an assignment added in a branch of its own,
		// which is the way this drifts.
		const code = UPDATE_FIELD;
		const claims = [...code.matchAll(/fieldWrites\.claim\(ticket\)/g)].map((m) => m.index ?? 0);
		const assigns = [...code.matchAll(/^\s*item = /gm)].map((m) => m.index ?? 0);
		expect(assigns.length).toBeGreaterThan(0);
		expect(claims.length).toBeGreaterThan(0);

		for (const at of assigns) {
			const previous = claims.filter((c) => c < at).pop();
			const where = JSON.stringify(code.slice(at, at + 60).trim());
			expect(previous, `no claim gate before ${where}`).toBeDefined();
			const between = code.slice(previous as number, at);
			expect(between.length, `claim gate too far from ${where}`).toBeLessThan(300);
			expect(between, `await between claim and ${where}`).not.toMatch(/\bawait\b/);
		}
	});
});
