<script lang="ts">
	import { editorStore } from '$lib/stores/editor.svelte';

	// Minimal probe mirroring ItemDetail's per-instance localDirty shadow
	// (PLAN-2154 Phase 0 / R4, TASK-2156). An edit writes BOTH the shared
	// editorStore.dirty singleton (still consumed by other single-per-page
	// callers, e.g. +layout.svelte) and this instance's own `localDirty`
	// $state; the destructive-guard formula reads ONLY the local shadow,
	// mirroring ItemDetail.svelte's SSE archive/delete guards (~line 600,
	// ~708): `saveStatus === 'saving' || editingTitle || localDirty`.
	let localDirty = $state(false);

	export function edit() {
		editorStore.setDirty(true);
		localDirty = true;
	}
	export function save() {
		editorStore.setDirty(false);
		localDirty = false;
	}

	// The fixed guard (post-TASK-2156): reads the per-instance shadow.
	let guardTripped = $derived(localDirty);
	// Contrast probe: what the PRE-fix formula (reading the global
	// singleton directly) would have evaluated for this instance.
	let guardTrippedIfGlobal = $derived(editorStore.dirty);
</script>

<button onclick={edit} data-testid="edit">edit</button>
<button onclick={save} data-testid="save">save</button>
<span data-testid="guard">{guardTripped}</span>
<span data-testid="guard-global">{guardTrippedIfGlobal}</span>
