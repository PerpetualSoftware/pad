import { describe, it, expect } from 'vitest';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

/**
 * IDEA-2898 round 2 — a SOURCE PIN for the two page-level call sites, and an
 * honest statement of what it is worth.
 *
 * The collection route's `deltaSync` is the second driver of the access-epoch
 * comparison (the first, bootstrap's reconcile loop, is pinned behaviourally in
 * localIndexAccessEpochWiring.svelte.test.ts). It lives inside a ~3700-line
 * page component with no component-level harness, and a mutation run confirmed
 * the gap rather than assuming it: deleting `delta.access_epoch` from that call
 * left every other test in this unit green.
 *
 * WHAT THIS PROVES: that the two arguments are still passed at those call
 * sites, so their silent removal fails a test instead of nothing.
 *
 * WHAT IT DOES NOT PROVE: that the surrounding logic is correct, that the calls
 * are reached, or that the values are the right ones. It is a tripwire, not a
 * test of behaviour, and it should be DELETED the day the page's deltaSync
 * grows a real harness or moves into the store (IDEA-2901 would move it).
 *
 * The anchors are deliberately narrow — the enclosing call, not a bare
 * identifier — because a substring like `access_epoch` appears in this file for
 * several unrelated reasons and would pass while the argument was gone.
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

	it('declares the delta epoch when it applies a batch', () => {
		// The whole call, so a removed argument cannot be masked by the same
		// identifier appearing in a comment or a neighbouring statement.
		// A bounded window rather than `[^)]*`: the argument carries an
		// explanatory comment that itself contains parentheses, and the
		// negated-class form silently failed to match because of it — the
		// pin's first version was refuted by the code it was pinning.
		expect(pageSource).toMatch(
			/localIndex\.applyDelta\(\s*ws,\s*delta\.changes,\s*delta\.cursor,\s*delta\.includes_unparented_metadata,[\s\S]{0,900}?delta\.access_epoch,/,
		);
	});
});
