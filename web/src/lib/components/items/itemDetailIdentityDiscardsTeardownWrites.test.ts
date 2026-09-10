// Node-project test (no DOM): a SOURCE-level guard that ItemDetail's teardown
// writes DISCARD when the signed-in identity has moved (BUG-3005, codex round
// 4).
//
// WHY THIS IS LOAD-BEARING RATHER THAN BELT-AND-BRACES. A real identity change
// now RELOADS the tab, and a reload fires `beforeunload`. Both persistence
// paths in that handler use `keepalive`, a request deliberately built to
// outlive the page — so without a guard, the previous user's Y.Doc snapshot and
// pending markdown are PATCHed carrying the NEW user's cookie. The fix that
// reloads is what makes this reachable; it ships with it.
//
// The prompt half matters as much as the flush half: `preventDefault()` on a
// dirty editor raises the native "unsaved changes" dialog, and "Stay" CANCELS
// the reload — leaving the new user in the previous user's page shell with the
// stores already cleared and (since round 3) no remount left to rebuild it.
//
// WHY SOURCE TEXT AND NOT A RENDER: same reasoning as
// `itemDetailLinksSurviveFailedRefresh.test.ts` in this directory — ItemDetail
// is ~7,900 lines of collab, SSE and pane wiring, and the property is
// structural. WHAT A SOURCE GUARD CANNOT DO, stated so it is not mistaken for
// proof: it checks spellings, not behaviour. It would not catch a guard that
// compares the wrong two values, or one made unreachable by an earlier return.
import { describe, expect, it } from 'vitest';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';

const SRC = readFileSync(resolve(__dirname, './ItemDetail.svelte'), 'utf8');

/** Strip line and block comments so a guard cannot be satisfied by prose. */
function stripComments(src: string): string {
	return src.replace(/\/\*[\s\S]*?\*\//g, '').replace(/^[ \t]*\/\/.*$/gm, '');
}

const CODE = stripComments(SRC);

describe('ItemDetail teardown writes under an identity change', () => {
	it('captures the identity epoch at LOAD and re-stamps it in loadData', () => {
		// The comparison is only meaningful against the identity whose typing
		// the editor holds, which is the one current when the item was loaded —
		// not the one current when the save fires.
		expect(CODE).toMatch(/let\s+identityEpochAtLoad\s*=\s*authStore\.identityEpoch/);
		const loadData = CODE.slice(CODE.indexOf('async function loadData()'));
		const upToFirstAwait = loadData.slice(0, loadData.indexOf('await'));
		expect(upToFirstAwait).toContain('identityEpochAtLoad = authStore.identityEpoch');
	});

	it('makes the identity check the FIRST thing beforeunload does', () => {
		// Ahead of both flushes AND ahead of `preventDefault`, or the handler
		// still writes or still blocks the reload.
		const start = CODE.indexOf('const onBeforeUnload = (event: BeforeUnloadEvent) => {');
		expect(start, 'the beforeunload handler was renamed or removed').toBeGreaterThan(-1);
		const body = CODE.slice(start, start + 1200);
		const guard = body.indexOf('authStore.identityEpoch !== identityEpochAtLoad');
		expect(guard, 'beforeunload does not check the identity epoch').toBeGreaterThan(-1);

		for (const persisting of ['collabFlusher.flushNow', 'rawContentSaver.flushNow', 'preventDefault']) {
			const at = body.indexOf(persisting);
			expect(at, `${persisting} is no longer in the handler — re-point this guard`).toBeGreaterThan(-1);
			expect(at, `${persisting} runs BEFORE the identity check`).toBeGreaterThan(guard);
		}
	});

	it('gates the collab cleanup flush on the identity too', () => {
		// The $effect cleanup is a second door into the same write, and it runs
		// on the same identity change. My own grep for `keepalive: true` missed
		// it, because this one passes the flag positionally.
		expect(CODE).toMatch(
			/const\s+identityHeld\s*=\s*authStore\.identityEpoch\s*===\s*identityEpochAtLoad/,
		);
		expect(CODE).toMatch(/if\s*\(!rawMode\s*&&\s*!skipFlush\s*&&\s*identityHeld\)/);
	});

	it('gates the raw saver, which is the third door', () => {
		const saver = CODE.slice(CODE.indexOf('const rawContentSaver = createContentSaver('));
		const save = saver.slice(saver.indexOf('save: (markdown'));
		const guard = save.indexOf('authStore.identityEpoch !== identityEpochAtLoad');
		const patch = save.indexOf('api.items');
		expect(guard).toBeGreaterThan(-1);
		expect(patch).toBeGreaterThan(guard);
	});
});
