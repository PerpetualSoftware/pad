import { describe, it, expect } from 'vitest';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

/**
 * TASK-2921 — a SOURCE PIN for the workspace layout's reconcile drivers, and an
 * honest statement of what it is worth.
 *
 * This unit DELETED a file exactly like this one (`localIndexAccessEpochPage-
 * Wiring.test.ts`) on its author's written instruction, because the call it
 * pinned moved into the store where behaviour can be tested. It then created a
 * new binding with the same problem: `+layout.svelte` is a route component with
 * no component-level harness, and whether it SUBSCRIBES is not reachable from a
 * store test. The loop it drives is now covered behaviourally
 * (localIndexReconcileSharedLoop, localIndexReconcileAuthDrop); the WIRING is
 * not, and pretending otherwise because the deleted pin was unsatisfying would
 * be the worse mistake.
 *
 * WHAT THIS PROVES: that both drivers are still wired at that call site, so
 * their silent removal fails a test instead of nothing. If they are removed, a
 * `sync_required` or an item event reaches no reconcile on any route — the exact
 * defect IDEA-2901 filed, restored, with every behavioural test still green
 * because they call `localIndex.reconcile` directly.
 *
 * WHAT IT DOES NOT PROVE: that the calls are REACHED, that the surrounding
 * conditions are right, or that the workspace slug is the correct one. Its
 * measured limit is the one the deleted pin recorded and is worth restating: a
 * short-circuit (`if (false && …)`) SURVIVES this, because the matched text is
 * still on the line. A source pin cannot see reachability.
 *
 * DELETE IT the day `+layout.svelte` grows a component harness, or the drivers
 * move somewhere testable. Same condition, same reasoning, as the file it
 * replaces.
 */
const layoutSource = readFileSync(
	fileURLToPath(new URL('../../routes/[username]/[workspace]/+layout.svelte', import.meta.url)),
	'utf8',
);

describe('TASK-2921 — the workspace layout drives the reconcile for every route', () => {
	it('reconciles on a sync signal', () => {
		// The anchor is the enclosing call rather than a bare identifier:
		// `reconcile` appears in that file for unrelated reasons and a substring
		// match would pass while the call was gone.
		expect(layoutSource).toContain('await localIndex.reconcile(wsSlug)');
	});

	it('reconciles on a non-stale item event', () => {
		expect(layoutSource).toContain("localIndex.classifySSEEvent(wsSlug, event) !== 'stale'");
	});

	it('still owns markSynced, which depends on the reconcile outcome', () => {
		expect(layoutSource).toContain('syncService.markSynced()');
	});
});
