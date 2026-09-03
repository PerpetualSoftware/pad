<script lang="ts">
	// Test-only host for ItemPicker (TASK-2862). Same role as
	// `masterFreeze/FreezeProbe.svelte` and `localDirtyGuard/GuardProbe.svelte`:
	// a component that exists so a test can drive a behaviour the test file
	// cannot reach on its own.
	//
	// WHY IT IS NEEDED. `excludeIds` arriving LATE is a real case — `ItemDetail`
	// loads `itemLinks` asynchronously, so a picker opened first must re-filter
	// when they land (codex round 4 P2). Testing-library's `rerender` cannot
	// prove that: it replaces the whole props object, which re-runs the picker's
	// refresh effect whether or not the effect tracks `excludeIds` — so the
	// mutant that removes that dependency SURVIVES a rerender-driven test while
	// the production path (a single prop changing) would be broken.
	//
	// This host changes exactly ONE prop, the way a real parent does.
	import ItemPicker from './ItemPicker.svelte';
	import type { ItemIndexRow } from '$lib/types';

	interface Props {
		wsSlug: string;
		collection?: string;
		source?: 'index' | 'server';
		onselect?: (item: ItemIndexRow) => void;
	}

	let { wsSlug, collection, source = 'index', onselect = () => {} }: Props = $props();

	// Starts empty and is only ever changed through `setExcludeIds` — seeding
	// it from a prop would capture the initial value and warn.
	let excludeIds = $state<string[]>([]);

	/** Driven by the test to simulate the async `itemLinks` load resolving. */
	export function setExcludeIds(next: string[]) {
		excludeIds = next;
	}
</script>

<ItemPicker {wsSlug} {collection} {source} {excludeIds} {onselect} />
