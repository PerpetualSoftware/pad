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
		// BUG-3037 re-pointed this: the call is now `submitOrderedOCC<Item, OCCToken>`
		// because the token is the row's `seq`, not its second-resolution
		// `updated_at`. The generic list is matched loosely — what this leg is
		// about is that ONE shared loop runs here, not which token it carries.
		expect(UPDATE_FIELD).toMatch(/submitOrderedOCC<Item[^>]*>\(\{/);
		const call = UPDATE_FIELD.slice(UPDATE_FIELD.search(/submitOrderedOCC<Item[^>]*>\(\{/));
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

	it('every claim is a GATE — the answer is acted on, never discarded', () => {
		// Round 8 enumeration. The leg above matched `fieldWrites.claim(ticket)`
		// anywhere, so replacing `if (!fieldWrites.claim(ticket)) return;` with a
		// bare `fieldWrites.claim(ticket);` kept the proximity and no-await
		// assertions green while every response was assigned regardless of the
		// answer. A call whose result nothing reads is not a guard.
		const calls = [...UPDATE_FIELD.matchAll(/fieldWrites\.claim\(ticket\)/g)];
		expect(calls.length).toBeGreaterThan(0);
		for (const m of calls) {
			const line = UPDATE_FIELD.slice(
				UPDATE_FIELD.lastIndexOf('\n', m.index ?? 0) + 1,
				UPDATE_FIELD.indexOf('\n', m.index ?? 0),
			).trim();
			expect(line, `claim result discarded: ${line}`).toBe('if (!fieldWrites.claim(ticket)) return;');
		}
	});

	it('the forced-write success is gated on CLAIM and never on supersession', () => {
		// Round 8 enumeration, and a defect this unit's own first version
		// introduced. A forced write that SUCCEEDED has changed the row; if the
		// newer write then failed without writing, returning on supersession
		// leaves the server holding the confirmed value and the pane showing the
		// old one, with nothing left to correct it. `claim` asks whether anything
		// newer actually WROTE, which is the question that matters here.
		// Bounded at the branch's own `showSaved(saveTok)`, not at the next landmark:
		// the DECLINED branch below it legitimately mentions supersession (for
		// the save indicator), and a slice running into it reports that as a
		// violation here.
		// The window starts where the dialog RETURNS, not at `if (forced) {`:
		// a supersession check placed just above that line returns before the
		// forced branch is ever reached, which is the defect itself. A slice
		// anchored on the branch would miss it — measured, it survived.
		const windowAt = UPDATE_FIELD.indexOf('if (!stillCurrent() || !item) return;');
		const forcedAt = UPDATE_FIELD.indexOf('if (forced) {', windowAt);
		const savedAt = UPDATE_FIELD.indexOf('showSaved(saveTok);', forcedAt);
		expect(windowAt).toBeGreaterThan(-1);
		expect(forcedAt).toBeGreaterThan(windowAt);
		expect(savedAt).toBeGreaterThan(forcedAt);
		const forcedBranch = UPDATE_FIELD.slice(windowAt, savedAt);
		expect(forcedBranch).toContain('if (!fieldWrites.claim(ticket)) return;');
		expect(forcedBranch).not.toMatch(/fieldWrites\.superseded\(ticket\)/);
	});

	it('the DECLINED branch takes no claim — it writes no server truth', () => {
		// The other half of the same defect. Declining writes nothing: it re-props
		// the client value it already had. Claiming advanced the applied mark all
		// the same, so a write that had genuinely COMMITTED and was still in
		// flight lost its claim on return. A branch that asserts no new server
		// truth must not take the mark for one.
		const declined = UPDATE_FIELD.slice(
			UPDATE_FIELD.indexOf('if (forced) {'),
			UPDATE_FIELD.indexOf('BUG-2273: OCC retries were exhausted') > -1
				? UPDATE_FIELD.indexOf('if (isUpdateConflictError(e)) {')
				: UPDATE_FIELD.length,
		);
		const afterForced = declined.slice(declined.indexOf('toastStore.show(\'Status change cancelled\''));
		expect(declined).toContain("toastStore.show('Status change cancelled'");
		// Between the end of the forced branch and the cancel toast there is no
		// claim. (It used to hold the save-indicator guard; the indicator now
		// settles in updateField's finally, BUG-3044.)
		const cancelAt = declined.indexOf("toastStore.show('Status change cancelled'");
		const succeedAt = declined.indexOf('showSaved(saveTok);');
		expect(succeedAt, 'the forced branch ends at its success record').toBeGreaterThan(-1);
		const declinedBody = declined.slice(declined.indexOf('}', succeedAt), cancelAt);
		expect(declinedBody).not.toContain('fieldWrites.claim(ticket)');
		expect(afterForced.length).toBeGreaterThan(0);
	});

	it('the confirm callback asks about supersession BEFORE it sends', () => {
		// The dialog can sit open for as long as the user takes, and this
		// callback SENDS. Checking only after it returns suppresses the DISPLAY
		// of a write that has already reached the server (round 8 enumeration).
		const callback = UPDATE_FIELD.slice(
			UPDATE_FIELD.indexOf('confirmOpenChildrenOrThrow('),
			UPDATE_FIELD.indexOf('} catch (retryErr) {'),
		);
		const guardAt = callback.indexOf('fieldWrites.superseded(ticket)');
		const sendAt = callback.indexOf('submitWithOCC(true)');
		expect(guardAt).toBeGreaterThan(-1);
		expect(sendAt).toBeGreaterThan(-1);
		expect(guardAt).toBeLessThan(sendAt);
	});
});
