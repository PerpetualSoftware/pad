<script lang="ts">
	// PLAN-2154 Phase 2 / D2 / R12 (TASK-2172) — master-freeze wiring probe.
	//
	// HT-2176 Option A: the freeze blocks the INITIATION of NEW edits while the
	// master peeks; a save the user debounced BEFORE the pane opened completes
	// normally (no suppression, no data loss, no provider teardown). So this probe
	// asserts the NEW-EDIT GATES only — every new-edit surface is disabled/gated
	// while peeking, and byte-identical to the canEdit-only baseline when not.
	// (That a pre-pane in-flight save is NOT suppressed is a runtime property of
	// ItemDetail's saver/flush paths — no recheck blocks them — not assertable in
	// this static gate probe.)
	//
	// The running-app assertion is deferred to TASK-2175 (F) — no host passes
	// `peeking={true}` until TASK-2174 (E). The probe imports the SAME
	// `computeMutationsEnabled` helper `ItemDetail` uses and renders the CANONICAL
	// gate EXPRESSIONS from ItemDetail.svelte, so the two can't drift — same
	// pattern as GuardProbe.svelte for the localDirty shadow.
	import { computeMutationsEnabled } from '../mutationGate';

	let {
		canEdit = true,
		peeking = false,
		canRestore = true,
		isOwner = true,
		quickActionsPresent = false,
	}: {
		canEdit?: boolean;
		peeking?: boolean;
		canRestore?: boolean;
		isOwner?: boolean;
		quickActionsPresent?: boolean;
	} = $props();

	// The exact derived from ItemDetail.svelte.
	let mutationsEnabled = $derived(computeMutationsEnabled(canEdit, peeking));
</script>

<!-- Scalar gate props threaded to child components (mirror the exact
     ItemDetail expressions). -->
<div data-testid="mutationsEnabled">{mutationsEnabled}</div>
<div data-testid="editor-editable">{!peeking}</div>
<div data-testid="raw-readonly">{!canEdit || peeking}</div>
<!-- FieldEditor input is readonly while peeking → NO new field edit can be
     started; a value typed BEFORE the pane opened still saves (updateField has
     no peeking recheck — its debounce completes). -->
<div data-testid="field-readonly">{!mutationsEnabled}</div>
<!-- ChildItems receives the REAL canEdit (reorder authorizes off parent edit)
     plus a SEPARATE frozen — NOT mutationsEnabled — so add-child's independent
     capability logic is untouched when not peeking. Timeline mirrors this. -->
<div data-testid="child-canEdit">{canEdit}</div>
<div data-testid="child-frozen">{peeking}</div>
<div data-testid="timeline-frozen">{peeking}</div>

<!-- The Rich⇄Markdown mode toggle is a provider-LIFECYCLE control (switching to
     Markdown destroys the retained collab provider), so it hides on `!peeking`
     — NOT on `mutationsEnabled` (a genuine read-only viewer keeps the toggle;
     only a peeking master must not tear the provider down). -->
{#if !peeking}
	<button data-testid="mode-toggle">Rich / Markdown</button>
{/if}

<!-- Mutation UI gated on `mutationsEnabled` — unmounted while peeking. -->
{#if mutationsEnabled}
	<button data-testid="delete-btn">Delete</button>
{/if}
{#if mutationsEnabled}
	<button data-testid="move-btn">Move to…</button>
{/if}
{#if mutationsEnabled}
	<button data-testid="add-relationship-btn">+ Add relationship</button>
{/if}
{#if mutationsEnabled}
	<button data-testid="editor-mutation-ui">bubble/link popover</button>
{/if}

<!-- Title: editable click-to-edit vs read-only heading. -->
{#if mutationsEnabled}
	<button data-testid="title-editable">Edit title</button>
{:else}
	<h1 data-testid="title-readonly">Title</h1>
{/if}

<!-- Archived restore uses the `canRestore && !peeking` split (canEdit is forced
     false for archived items, so it can't ride `mutationsEnabled`). -->
{#if canRestore && !peeking}
	<button data-testid="archived-restore-btn">Restore</button>
{/if}

<!-- Star gates on `peeking` ONLY (viewers can star; mutationsEnabled would
     wrongly disable a non-peeking viewer). -->
<button data-testid="star-btn" disabled={peeking}>Star</button>

<!-- Share + the whole quick-actions menu gate on `!peeking` (the menu unmounts
     while peeking, dismissing any open dropdown/create-form). NOT mutationsEnabled
     — byte-identical for an archived-item owner. -->
{#if isOwner && !peeking}
	<button data-testid="share-btn">Share</button>
{/if}
{#if (quickActionsPresent || isOwner) && !peeking}
	<button data-testid="quickactions-menu">Quick actions</button>
{/if}
