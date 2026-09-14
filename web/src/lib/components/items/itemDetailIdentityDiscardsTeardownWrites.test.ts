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

	it('routes every teardown write through the one guarded function', () => {
		// RE-POINTED BY BUG-3030, and the re-point is the point. This used to
		// slice a 1200-character POSITION WINDOW out of the beforeunload handler
		// and check the guard preceded the two flushes inside it. BUG-3030 added
		// `pagehide` and `visibilitychange` handlers that also write — and a
		// window anchored on one handler's spelling would have kept PASSING
		// while no longer covering the teardown writes at all. That is the
		// containment failure from BUG-2840 in miniature: the window felt like
		// it covered the subject because it covered the code I was looking at.
		//
		// So the shape changed. There is now ONE writing function, the guard
		// lives inside it, and the enumeration below FAILS CLOSED on a teardown
		// listener it has not been told about.
		const fn = CODE.indexOf('function runTeardownFlush()');
		expect(fn, 'runTeardownFlush was renamed or removed — re-point this guard').toBeGreaterThan(-1);
		const body = CODE.slice(fn, CODE.indexOf('\n\t}', fn));

		const guard = body.indexOf('authStore.identityEpoch !== identityEpochAtLoad');
		expect(guard, 'the teardown flush does not check the identity epoch').toBeGreaterThan(-1);
		for (const persisting of ['collabFlusher.flushNow', 'rawContentSaver.flushNow']) {
			const at = body.indexOf(persisting);
			expect(at, `${persisting} is no longer in runTeardownFlush — re-point this guard`).toBeGreaterThan(-1);
			expect(at, `${persisting} runs BEFORE the identity check`).toBeGreaterThan(guard);
		}

		// FAIL CLOSED on a teardown listener that writes directly instead of
		// going through the guarded function — it would bypass both the identity
		// check and the once-latch, and the assertions above would never see it.
		//
		// SCOPED TO THE LISTENER BLOCK, deliberately. The first version of this
		// asked whether ANY keepalive raw flush existed outside
		// runTeardownFlush, and it fired — on `loadData`'s item-switch flush
		// (PLAN-2105 / TASK-2112), which is a legitimately different thing: it
		// is not a teardown, it MUST NOT be latched (every switch owes its own
		// flush), and its identity protection lives inside the saver's own
		// `save` callback, pinned by the third test below. A real site, and the
		// wrong question — the guard's question was wider than the claim resting
		// on it, which is the failure this file exists to catch in the source.
		const reg = CODE.indexOf("const onBeforeUnload = (event: BeforeUnloadEvent) => {");
		const regEnd = CODE.indexOf("document.removeEventListener('visibilitychange'", reg);
		expect(regEnd, 'the teardown listener block was restructured — re-point this guard').toBeGreaterThan(reg);
		const listeners = CODE.slice(reg, regEnd);
		for (const direct of ['collabFlusher.flushNow', 'rawContentSaver.flushNow']) {
			expect(
				listeners.includes(direct),
				`a teardown listener calls ${direct} directly instead of runTeardownFlush() — it bypasses the identity guard and the once-latch`,
			).toBe(false);
		}
		expect(listeners).toContain('runTeardownFlush()');
	});

	it('fires the teardown flush ONCE, and re-arms only on a restore', () => {
		// The latch is justified by measurement, not tidiness: three lifecycle
		// events produce THREE identical PATCHes while the keepalive save is in
		// flight, and one when it resolves between them (BUG-3030). The teardown
		// path is fire-and-forget, so in-flight is the case that actually runs.
		const fn = CODE.indexOf('function runTeardownFlush()');
		const body = CODE.slice(fn, CODE.indexOf('\n\t}', fn));
		expect(body).toMatch(/if\s*\(teardownFlushed\)\s*return/);
		expect(body).toMatch(/teardownFlushed\s*=\s*true/);

		// Re-arm on the two ways a torn-down page comes back, or its NEXT
		// teardown flushes nothing.
		expect(CODE).toMatch(/onPageShow\s*=\s*\(\)\s*=>\s*\{[^}]*teardownFlushed\s*=\s*false/);
		expect(CODE).toMatch(/else\s+teardownFlushed\s*=\s*false/);
	});

	it('registers the flush on the events a suspended tab delivers', () => {
		// beforeunload alone is the defect (BUG-3030). It is kept — it is the
		// only one that can raise the unsaved-changes prompt — but it is no
		// longer the only registration.
		for (const ev of ['beforeunload', 'pagehide', 'pageshow']) {
			expect(CODE, `${ev} is not registered`).toContain(`window.addEventListener('${ev}'`);
		}
		expect(CODE).toContain("document.addEventListener('visibilitychange'");
		// And each is removed again, or a remount leaves a handler holding a
		// dead component's context.
		for (const ev of ['beforeunload', 'pagehide', 'pageshow']) {
			expect(CODE, `${ev} is never removed`).toContain(`window.removeEventListener('${ev}'`);
		}
		expect(CODE).toContain("document.removeEventListener('visibilitychange'");
	});

	it('keeps the unsaved-changes prompt guarded and independent of the latch', () => {
		const start = CODE.indexOf('const onBeforeUnload = (event: BeforeUnloadEvent) => {');
		expect(start, 'the beforeunload handler was renamed or removed').toBeGreaterThan(-1);
		const body = CODE.slice(start, start + 1200);
		const guard = body.indexOf('authStore.identityEpoch !== identityEpochAtLoad');
		expect(guard, 'beforeunload does not check the identity epoch before prompting').toBeGreaterThan(-1);
		const prevent = body.indexOf('preventDefault');
		expect(prevent, 'preventDefault is no longer in the handler — re-point this guard').toBeGreaterThan(-1);
		expect(prevent, 'preventDefault runs BEFORE the identity check').toBeGreaterThan(guard);
		// The prompt must NOT be inside the latch: if an earlier pagehide
		// flushed, `dirty` is still true until the PATCH lands, and the user
		// must still be warned.
		expect(body).toMatch(/if\s*\(rawContentSaver\.dirty\s*&&\s*item\)\s*\{\s*event\.preventDefault/);
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
