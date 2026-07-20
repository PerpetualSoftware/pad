<script lang="ts">
	// BUG-2263 — INVISIBLE-freeze field probe.
	//
	// Mounts the REAL `FieldEditor` to prove that a field stays INTERACTIVE across
	// a peeking flip: ItemDetail now drives `readonly={!canEdit}` (NOT
	// `!mutationsEnabled`), so opening the pane does NOT flip the field read-only —
	// the input stays mounted and a typed value's 500ms debounce still fires
	// onchange (→ updateField, which is side-independent). A `<button>` flips
	// peeking so the test can simulate the pane opening mid-edit; canEdit is fixed
	// true here, so `readonly` is constant false regardless of peeking.
	import FieldEditor from '$lib/components/fields/FieldEditor.svelte';
	import type { FieldDef } from '$lib/types';

	let { onchange }: { onchange: (v: any) => void } = $props();

	// `peeking` is flipped in-test to simulate the pane opening. It is surfaced
	// (below) so the test can confirm the flip happened — but it is DELIBERATELY
	// NOT wired into the field's `readonly`, which tracks `!canEdit` alone. That is
	// the whole point: peeking must NOT change the field's interactivity.
	let peeking = $state(false);
	const canEdit = true;
	const field: FieldDef = { key: 'component', label: 'Component', type: 'text' };
</script>

<button data-testid="begin-peek" onclick={() => (peeking = true)}>peek</button>
<span data-testid="probe-peeking">{peeking}</span>

<FieldEditor {field} value="" {onchange} readonly={!canEdit} />
