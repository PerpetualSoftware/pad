import { describe, it, expect } from 'vitest';
import { resolveRenameNavTarget, type RenameNavInput } from './renameNav';

// Convenience builder — every field is named so each scenario reads as the
// concrete route/snapshot/tracker state at the moment the SSE event arrives.
function input(over: Partial<RenameNavInput>): RenameNavInput {
	return {
		eventOldSlug: 'a',
		eventNewSlug: 'b',
		loadedCollectionSlug: 'a',
		routeSlug: 'a',
		renameNav: null,
		...over,
	};
}

describe('resolveRenameNavTarget', () => {
	it('applies a normal single rename in steady state (snapshot matches route)', () => {
		// Viewing collection X at slug A; a remote A→B rename arrives.
		expect(
			resolveRenameNavTarget(
				input({ eventOldSlug: 'a', eventNewSlug: 'b', loadedCollectionSlug: 'a', routeSlug: 'a', renameNav: null }),
			),
		).toBe('b');
	});

	it('converges a synchronous chained burst A→B→C (pre-goto-commit) on the final slug', () => {
		// Both events fire before any goto commits: routeSlug + snapshot stay at A.
		const afterAB = resolveRenameNavTarget(
			input({ eventOldSlug: 'a', eventNewSlug: 'b', loadedCollectionSlug: 'a', routeSlug: 'a', renameNav: null }),
		);
		expect(afterAB).toBe('b'); // handler sets renameNav = 'b'
		const afterBC = resolveRenameNavTarget(
			input({ eventOldSlug: 'b', eventNewSlug: 'c', loadedCollectionSlug: 'a', routeSlug: 'a', renameNav: 'b' }),
		);
		expect(afterBC).toBe('c');
	});

	it('REGRESSION: applies a chained B→C during the goto→reload window (stale snapshot, continuation)', () => {
		// A→B has committed: routeSlug = B and renameNav = B, but loadCollection(B)
		// is still in flight so the loaded snapshot is briefly the pre-rename slug A.
		// A live B→C must still retarget to C rather than strand on dead slug B.
		expect(
			resolveRenameNavTarget(
				input({ eventOldSlug: 'b', eventNewSlug: 'c', loadedCollectionSlug: 'a', routeSlug: 'b', renameNav: 'b' }),
			),
		).toBe('c');
	});

	it('rejects a stale FOREIGN snapshot in the reused-slug X/B → Y/A window (renameNav reset)', () => {
		// X renamed A→B settled; user navigates X/B → Y/A (Y reused the freed slug
		// A). renameNav is reset to null on the cross-collection nav. A replayed X
		// A→B event arrives while `collection` is still stale X (its current slug
		// is B). It must NOT hijack the Y navigation.
		expect(
			resolveRenameNavTarget(
				input({ eventOldSlug: 'a', eventNewSlug: 'b', loadedCollectionSlug: 'b', routeSlug: 'a', renameNav: null }),
			),
		).toBeNull();
	});

	it('rejects the reused-slug hijack even before renameNav is reset (renameNav still B)', () => {
		// Timing sub-window: collSlug already flipped to A (Y route) but the effect
		// has not yet reset renameNav from B. pendingContinuation needs
		// renameNav === routeSlug (B === A → false), so it stays rejected.
		expect(
			resolveRenameNavTarget(
				input({ eventOldSlug: 'a', eventNewSlug: 'b', loadedCollectionSlug: 'b', routeSlug: 'a', renameNav: 'b' }),
			),
		).toBeNull();
	});

	it('drops a duplicate/superseded replay of an already-applied rename (fresh snapshot at B)', () => {
		// Settled at B; a duplicate A→B replay arrives. believed = B, eventOldSlug
		// = A ≠ believed → no redundant goto.
		expect(
			resolveRenameNavTarget(
				input({ eventOldSlug: 'a', eventNewSlug: 'b', loadedCollectionSlug: 'b', routeSlug: 'b', renameNav: 'b' }),
			),
		).toBeNull();
	});

	it('skips a no-op where the new slug already equals the believed slug', () => {
		expect(
			resolveRenameNavTarget(
				input({ eventOldSlug: 'a', eventNewSlug: 'a', loadedCollectionSlug: 'a', routeSlug: 'a', renameNav: null }),
			),
		).toBeNull();
	});

	it('does not treat a same-slug snapshot match as a continuation shortcut for a foreign old-slug', () => {
		// Snapshot matches route (fresh, at A), renameNav null: only an event whose
		// OLD slug is the current slug A applies. An event routed by some other old
		// slug is dropped.
		expect(
			resolveRenameNavTarget(
				input({ eventOldSlug: 'z', eventNewSlug: 'q', loadedCollectionSlug: 'a', routeSlug: 'a', renameNav: null }),
			),
		).toBeNull();
	});
});
