import { describe, it, expect } from 'vitest';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

/**
 * IDEA-2898 — a SOURCE PIN for the page-level call site, and an honest
 * statement of what it is worth.
 *
 * The collection route's `deltaSync` is the second driver of the access-epoch
 * comparison (the first, bootstrap's reconcile loop, is pinned BEHAVIOURALLY in
 * localIndexAccessEpoch.svelte.test.ts). It lives inside a ~3700-line page
 * component with no component-level harness, and a mutation run confirmed the
 * gap rather than assuming it: disabling the call left every other test in this
 * unit green.
 *
 * NARROWED from the version on the full branch, which pinned TWO call sites.
 * The second — `applyDelta` declaring the epoch its batch was built under — was
 * part of the cross-tab pairing guard, which moved to PLAN-2903 along with the
 * code it guarded. This pin covers only what this branch ships.
 *
 * WHAT THIS PROVES: that the comparison is still invoked at that call site, so
 * its silent removal fails a test instead of nothing.
 *
 * WHAT IT DOES NOT PROVE: that the surrounding logic is correct, that the call
 * is reached, or that the value is the right one. It is a tripwire, not a test
 * of behaviour, and it should be DELETED the day the page's deltaSync grows a
 * real harness or moves into the store (IDEA-2901 would move it).
 *
 * AND ITS MEASURED LIMIT, because a pin that is trusted further than it reaches
 * is worse than none. Two mutants were run against it. Deleting the call KILLS
 * it, which is the silent-removal shape it exists for. Short-circuiting the
 * call — `if (false && await localIndex.ensureAccessScope(...))` — SURVIVES,
 * because the text this pin matches is still on the line. A source pin cannot
 * see reachability; only a harness on that component could, and that is the
 * same missing harness that makes this file necessary.
 *
 * The anchor is deliberately the enclosing call rather than a bare identifier —
 * `access_epoch` appears in that file for several unrelated reasons and a
 * substring match would pass while the call was gone.
 */
const pageSource = readFileSync(
	fileURLToPath(
		new URL('../../routes/[username]/[workspace]/[collection]/+page.svelte', import.meta.url),
	),
	'utf8',
);

describe('IDEA-2898 — the collection route still drives the access-epoch comparison', () => {
	it('compares the delta epoch before applying, via ensureAccessScope', () => {
		expect(pageSource).toContain('localIndex.ensureAccessScope(ws, delta.access_epoch)');
	});
});
