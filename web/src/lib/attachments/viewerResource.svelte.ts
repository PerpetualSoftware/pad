import { untrack } from 'svelte';
import { nextViewerResourceGen, viewerResourceKey } from './viewerResource';

/**
 * The viewer-resource generation as a live counter (TASK-2428).
 *
 * The pure rule lives next door in `viewerResource.ts`; this is the reactive
 * wrapper `ItemDetail` actually uses. It is a module rather than four lines
 * inline in a 7,000-line component for one reason: the `$effect` below is the
 * subtlest thing in this task, and inline it could only ever be exercised
 * through `AttachmentSurfaceHost`'s props — which are handed the answer and so
 * cannot tell a correct counter from one that never moves, nor a healthy flush
 * from a self-invalidating one (Codex round 6).
 *
 * SELF-WRITE DISCIPLINE (CONVE-1688). The effect writes `gen`, which it must
 * therefore never READ in its tracked scope: an `$effect` that depends on what
 * it writes aborts the flush, and an aborted flush strands unrelated reactivity
 * elsewhere in the same batch while reporting nothing in a production build.
 * So the tracked scope reads ONLY the caller's inputs, and both the comparison
 * and the write happen inside `untrack`. `lastKey` is a plain `let` for the
 * same reason — as `$state` it would reintroduce exactly that dependency.
 *
 * Must be called during component initialisation, like any `$effect` owner.
 */
export interface ViewerResourceInput {
	/** Workspace the CURRENTLY LOADED item belongs to, not the route's. */
	workspaceSlug: string;
	/** UUID of the loaded item. */
	itemId: string | null | undefined;
	/** Whether the loaded item matches the requested ref (the switch boundary). */
	loaded: boolean;
}

export interface ViewerResourceGen {
	/** Advances only on a real loaded-item resource change. */
	readonly current: number;
}

export function createViewerResourceGen(read: () => ViewerResourceInput): ViewerResourceGen {
	let gen = $state(0);
	let lastKey = '';

	$effect(() => {
		const input = read();
		const key = viewerResourceKey(input.workspaceSlug, input.itemId, input.loaded);
		untrack(() => {
			const next = nextViewerResourceGen(key, lastKey, gen);
			if (next === gen) return;
			lastKey = key;
			gen = next;
		});
	});

	return {
		get current() {
			return gen;
		},
	};
}
