// Bring a workspace's local index up and catch it up — THE way a page does it
// (BUG-3181). Extracted from the collection page, where this order was the
// BUG-3084 fix, so a second page (the workspace activity page) reuses it
// rather than hand-rolling a copy that drifts.
//
// The order, and why each step is where it is:
//   1. `localIndex.bootstrap` — idempotent (a no-op once 'ready'); warm IDB or
//      a cold /items-index. Its errors are swallowed on purpose: the STORE
//      flips to 'error' and runs the 401/403 purge itself, and the BUG-2983
//      terminal refusal lives inside it, so every caller gets both.
//   2. the identity check — OUTSIDE that try and BEFORE the commit. Inside, a
//      bootstrap REJECTION would skip it and reach the reconcile anyway (the
//      catch swallows by design); after the reconcile it would guard nothing.
//   3. `localIndex.reconcile` — the catch-up for anything written elsewhere
//      while this page was unmounted (the loop is the store's since TASK-2921).
//
// The CALLER captures the identity epoch, synchronously, at its effect's entry
// (so the capture is the current one and the fence stops the PREVIOUS run's
// continuation) and passes it in.
//
// `web/src/lib/stores/workspaceIndexEntry.test.ts` pins both halves: this
// order, and that nothing else in web/src calls `localIndex.bootstrap`
// directly (one named exception — see that test).
import { localIndex } from './localIndex.svelte';
import { authStore } from './auth.svelte';

/**
 * Bootstrap `ws`'s local index, then catch it up. Resolves true on a clean
 * catch-up, false on a failure, a capped reconcile, or an identity change
 * along the way (an answer fetched for someone else is not a catch-up).
 */
export async function enterWorkspaceIndex(ws: string, userId: string | null, epochAtEntry: number): Promise<boolean> {
	try {
		await localIndex.bootstrap(ws, { userId });
	} catch {
		// The store has already recorded 'error' (and purged on 401/403).
	}
	if (authStore.identityEpoch !== epochAtEntry) return false;
	try {
		const caughtUp = await localIndex.reconcile(ws);
		return caughtUp && authStore.identityEpoch === epochAtEntry;
	} catch {
		return false;
	}
}
