/**
 * The attachment surface's delete-confirmation machine (PLAN-2392 3c-i /
 * TASK-2473), lifted VERBATIM from the options panel's internals so the panel
 * and the converged surface shared ONE implementation. The panel was retired in
 * the T2b cutover (TASK-2488); the sole consumer now is the grown `Lightbox`.
 *
 * THE PROMISE / UI SPLIT STAYS (DR-18). The delete DESCRIPTOR (`actions.ts`)
 * owns the delete itself: it snapshots identity, awaits `ctx.confirmDelete()`
 * (this module's gate), re-checks permission + identity on the far side, and
 * only then calls the API. So this module owns the confirmation STATE — the
 * pending resolver, the warning string, and the abandon rules — and the
 * existing `AttachmentDeleteConfirm.svelte` keeps RENDERING it. It does NOT own
 * the delete action; a module-owned `onConfirmed` would fire a second delete
 * beside the descriptor's and defeat the identity snapshot the descriptor keeps.
 *
 * TWO ABANDON RULES, both of which reject the gate promise so the descriptor's
 * `await` never dangles:
 *   - PERMISSION WITHDRAWN mid-decision — the pane goes peeked or a role
 *     changes while the confirmation is up; the surface is supposed to offer no
 *     delete at all in that state, so the confirmation is abandoned (watched
 *     here via `mutationsEnabled`).
 *   - SUBJECT CHANGE / teardown — the surface moves to another attachment or is
 *     torn down; the caller drives these through `cancel()` / `dispose()`.
 *
 * WHY A `.svelte.ts` MODULE. It holds `$state` and a `$effect` (the permission
 * watch), so it is a rune module and must be created inside a component's init.
 */
import { untrack } from 'svelte';
import { attachmentDeletePrompt } from '$lib/components/attachments/AttachmentDeleteConfirm.svelte';

export interface DeleteConfirmDeps {
	/** "May this user delete here" — the host's answer. The gate no-ops without it. */
	mutationsEnabled: () => boolean;
	/**
	 * Whether THIS body references the attachment (the boolean `referencedHere()`
	 * the panel computes from its live/saved content). Read at request time so it
	 * sees unflushed editor edits.
	 */
	isReferenced: () => boolean;
	/** The attachment's display name, for the prompt. */
	displayName: () => string;
}

export interface SurfaceDeleteConfirm {
	/** True while the confirmation sub-view is up. */
	readonly pending: boolean;
	/** The prompt string while pending (the shared two-arm wording), else null. */
	readonly warning: string | null;
	/**
	 * Open the confirmation and resolve when the user answers — the descriptor's
	 * `ctx.confirmDelete`. A no-op that resolves false when the caller may not
	 * mutate. Supersedes any confirmation already up.
	 */
	request(): Promise<boolean>;
	/** The sub-view's Delete row — resolve the gate true. */
	confirm(): void;
	/** The sub-view's Cancel row, and the subject-change abandon — resolve false. */
	cancel(): void;
	/** Teardown — reject any pending confirmation so the descriptor's await settles. */
	dispose(): void;
}

/**
 * Build the delete-confirmation machine. `mutationsEnabled` and `isReferenced`
 * MUST read live reactive values on every call.
 */
export function createDeleteConfirm(deps: DeleteConfirmDeps): SurfaceDeleteConfirm {
	/** 'root' = actions shown; 'delete' = confirmation up. */
	let view = $state<'root' | 'delete'>('root');
	let prompt = $state('');
	/** Resolver for the confirmation currently on screen, if any. */
	let pendingConfirm: ((confirmed: boolean) => void) | null = null;

	function settle(confirmed: boolean) {
		const resolve = pendingConfirm;
		pendingConfirm = null;
		view = 'root';
		resolve?.(confirmed);
	}

	/**
	 * Permission withdrawn while the confirmation is open (DR-8). Blocking the
	 * request is not enough — the user is left looking at a live "Delete file"
	 * button for an action that can no longer happen — so it is abandoned as a
	 * rejection, exactly as a subject change abandons it.
	 *
	 * Plain latch + `untrack` for the writes: as `$state` this effect would depend
	 * on what it writes, which aborts the flush and strands unrelated reactivity
	 * (CONVE-1688).
	 */
	$effect(() => {
		const mayMutate = deps.mutationsEnabled();
		untrack(() => {
			if (mayMutate || view !== 'delete') return;
			settle(false);
		});
	});

	function request(): Promise<boolean> {
		// The gate no-ops for a caller who may not mutate — the descriptor already
		// gates, but the module's own contract holds without it.
		if (!deps.mutationsEnabled()) return Promise.resolve(false);
		// The wording comes from the shared prompt — the same two arms the strip's
		// tile shows (DR-18). Composed at request time so it reflects the live body.
		prompt = attachmentDeletePrompt(deps.displayName(), deps.isReferenced());
		return new Promise<boolean>((resolve) => {
			// Supersede any confirmation already up — two open at once would leave a
			// resolver dangling forever.
			pendingConfirm?.(false);
			pendingConfirm = resolve;
			view = 'delete';
		});
	}

	return {
		get pending() {
			return view === 'delete';
		},
		get warning() {
			return view === 'delete' ? prompt : null;
		},
		request,
		confirm: () => settle(true),
		cancel: () => settle(false),
		dispose: () => {
			const resolve = pendingConfirm;
			pendingConfirm = null;
			resolve?.(false);
		},
	};
}
