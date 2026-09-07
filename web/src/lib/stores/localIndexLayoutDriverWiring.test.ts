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
 *
 * IT WILL FAIL ON A RENAME, and that is correct rather than annoying — it did,
 * once, within an hour of being written, when codex round 5 made both callbacks
 * capture the workspace slug before awaiting. The failure is the prompt to go and
 * look at what changed; the anchors are then updated deliberately. An anchor
 * loose enough to survive a rename would also survive the call being deleted,
 * which is the only thing this file exists to catch.
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
		expect(layoutSource).toContain('await localIndex.reconcile(ws)');
	});

	it('reconciles on a non-stale item event', () => {
		expect(layoutSource).toContain("localIndex.classifySSEEvent(eventWs, event) !== 'stale'");
	});

	it('still owns markSynced, which depends on the reconcile outcome', () => {
		expect(layoutSource).toContain('syncService.markSynced()');
	});
});

describe('TASK-2200 — the same layout drives shell recovery', () => {
	/**
	 * Same instrument, same limits, same delete-condition as the block above —
	 * this is a SOURCE PIN and it cannot see reachability. What it is worth: the
	 * two recovery calls cannot be silently deleted, which would restore a shell
	 * that stays navigation-less after the server returns while every
	 * behavioural test stays green, because they call `recoverIfMissing` and
	 * `loadCollections` directly.
	 */
	it('re-acquires workspace identity from the sync subscriber', () => {
		expect(layoutSource).toContain('await workspaceStore.recoverIfMissing(ws)');
	});

	it('re-acquires the collection list on the freshness CONDITION, not on a signal type alone', () => {
		// The MISSING term is the anchor, not the call. `loadCollections(ws)`
		// would still appear with the recovery term deleted and the
		// changed-signal term left standing — which is precisely the state this
		// unit found the file in, and the state a returning server does not
		// reach (see below).
		expect(layoutSource).toContain(
			'const collectionsMissing = !collectionStore.collectionsAreFreshFor(ws)',
		);
		expect(layoutSource).toContain('if (collectionsMissing || collectionsChanged)');
	});

	it('does not gate recovery on the full_refresh signal', () => {
		// Measured, not assumed (syncPostOutageResultType.svelte.test.ts): a
		// returning server reports `caught_up`, so recovery reachable only via
		// `full_refresh` would not run in the case this unit exists for. The
		// pin: identity recovery is not inside a result-type test, and the
		// collection load is reachable on the missing term alone.
		const recovery = layoutSource.indexOf('await workspaceStore.recoverIfMissing(ws)');
		const branch = layoutSource.indexOf("result.type === 'full_refresh'");
		expect(recovery).toBeGreaterThan(-1);
		expect(branch).toBeGreaterThan(-1);
		expect(recovery).toBeLessThan(branch);
	});
});
