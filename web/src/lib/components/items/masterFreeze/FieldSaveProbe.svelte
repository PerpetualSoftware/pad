<script lang="ts">
	// PLAN-2154 Phase 2 / HT-2176 Option A (TASK-2172) — pre-pane field-save probe.
	//
	// Mounts the REAL `FieldEditor` to prove the Option A invariant for FIELDS: a
	// value the user typed BEFORE the pane opened still SAVES (its 500ms debounce
	// fires onchange even after the field flips read-only on peeking-begin), while
	// a NEW field edit cannot be started once read-only. `peeking` drives the
	// FieldEditor's `readonly` exactly as ItemDetail does (`readonly={!mutationsEnabled}`,
	// with canEdit=true here). A `<button>` flips peeking so the test can simulate
	// the pane opening mid-edit without prop-rerender gymnastics.
	import FieldEditor from '$lib/components/fields/FieldEditor.svelte';
	import type { FieldDef } from '$lib/types';

	let { onchange }: { onchange: (v: any) => void } = $props();

	let peeking = $state(false);
	const field: FieldDef = { key: 'component', label: 'Component', type: 'text' };
</script>

<button data-testid="begin-peek" onclick={() => (peeking = true)}>peek</button>

<FieldEditor {field} value="" {onchange} readonly={peeking} />
