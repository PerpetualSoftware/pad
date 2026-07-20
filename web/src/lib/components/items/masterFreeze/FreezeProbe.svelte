<script lang="ts">
	// BUG-2263 — the master/pane freeze is INVISIBLE to the user.
	//
	// The freeze exists ONLY to keep exactly one TYPEABLE collab content editor
	// (so the three singleton UX scalars — editorStore.dirty/lastSaveTime,
	// collectionStore.activeItem, tab title — stay single-owner). It is NOT a
	// data-collision barrier: master and pane are always DIFFERENT items, whose
	// collab state is fully itemID-keyed / instance-local. So only the CONTENT
	// surfaces stay frozen while peeking; every REST surface (fields, buttons,
	// title, children, timeline, star, share, …) stays interactive on both sides
	// and is gated on `canEdit` (permission) alone. A click flips activePane first
	// (host pointerdown-capture), so the interaction lands in one gesture.
	//
	// This probe renders the CANONICAL gate EXPRESSIONS from ItemDetail.svelte so
	// the two can't drift — same pattern as GuardProbe.svelte for the localDirty
	// shadow. `mutationsEnabled` (= canEdit && !peeking) survives, but now gates
	// ONLY the content-editor chrome (bubble/link popover); the running-app
	// assertions live in the pane-full-page e2e specs.
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

	// The exact derived from ItemDetail.svelte — now scopes to content chrome only.
	let mutationsEnabled = $derived(computeMutationsEnabled(canEdit, peeking));
</script>

<!-- Scalar gate props threaded to child components (mirror the exact
     ItemDetail expressions). -->
<div data-testid="mutationsEnabled">{mutationsEnabled}</div>

<!-- CONTENT bucket — stays frozen while peeking (the one collision surface).
     The editor stays visually identical (same live view, editable flipped in
     place, no remount); a click activates the side and lands the caret. -->
<div data-testid="editor-editable">{!peeking}</div>
<div data-testid="raw-readonly">{!canEdit || peeking}</div>

<!-- REST bucket — INVISIBLE freeze: interactive on both sides, gated on canEdit. -->
<div data-testid="field-readonly">{!canEdit}</div>
<div data-testid="child-canEdit">{canEdit}</div>
<div data-testid="child-frozen">{false}</div>
<div data-testid="timeline-frozen">{false}</div>

<!-- The Rich⇄Markdown mode toggle is a provider-LIFECYCLE control (switching to
     Markdown destroys the retained collab provider), so it stays hidden on the
     passive preview (`!peeking`); it reappears the instant you click in (which
     you must do to edit content anyway). The one accepted visible exception. -->
{#if !peeking}
	<button data-testid="mode-toggle">Rich / Markdown</button>
{/if}

<!-- REST mutation UI — gated on `canEdit` alone, present on both sides. -->
{#if canEdit}
	<button data-testid="delete-btn">Delete</button>
{/if}
{#if canEdit}
	<button data-testid="move-btn">Move to…</button>
{/if}
{#if canEdit}
	<button data-testid="add-relationship-btn">+ Add relationship</button>
{/if}

<!-- Editor bubble/link popover is CONTENT chrome — it only appears on an editor
     interaction (selection), which requires the side to be active, so it stays
     gated on `mutationsEnabled` (invisible: never shows on the passive side). -->
{#if mutationsEnabled}
	<button data-testid="editor-mutation-ui">bubble/link popover</button>
{/if}

<!-- Title: editable click-to-edit on both sides (canEdit); read-only heading is
     for TRUE viewers (no canEdit) only. -->
{#if canEdit}
	<button data-testid="title-editable">Edit title</button>
{:else}
	<h1 data-testid="title-readonly">Title</h1>
{/if}

<!-- Archived restore rides `canRestore` (canEdit is forced false for archived
     items); NOT frozen while peeking — restore is a side-independent REST op. -->
{#if canRestore}
	<button data-testid="archived-restore-btn">Restore</button>
{/if}

<!-- Star: per-user, itemId-keyed REST toggle — no canEdit or peeking gate. -->
<button data-testid="star-btn">Star</button>

<!-- Share: side-independent owner-only REST op — present on both sides. -->
{#if isOwner}
	<button data-testid="share-btn">Share</button>
{/if}

<!-- Quick-actions menu: the prompt-copy actions are read-only and present on both
     sides; the owner "Manage/New" controls WRITE the whole collection settings
     from a per-item snapshot (last-write-wins across two items in one collection),
     so they gate on `isOwner && !peeking` — the write is confined to the active
     side (BUG-2263 / Codex P1). ItemDetail forwards `canEdit={isOwner && !peeking}`. -->
{#if quickActionsPresent || (isOwner && !peeking)}
	<div data-testid="quickactions-menu">
		{#if quickActionsPresent}
			<button data-testid="quickactions-prompt">Copy a prompt</button>
		{/if}
		{#if isOwner && !peeking}
			<button data-testid="quickactions-manage">Manage actions</button>
		{/if}
	</div>
{/if}

<!-- Version restore REST-writes this item's `items.content` directly, colliding
     with the retained Y.Doc on a peeking side — a SAME-ITEM collision. So it stays
     FROZEN while peeking (`restoreFrozen={peeking}`), unlike comments/reactions
     which are side-independent and stay live (BUG-2263 / Codex P1). -->
{#if !peeking}
	<button data-testid="version-restore-btn">Restore this version</button>
{/if}
