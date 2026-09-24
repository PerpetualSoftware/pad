<script lang="ts">
	// One side of an activity field change ("old → new"), shared by the item
	// timeline and the workspace activity page (BUG-2872).
	//
	// The server writes a change as display text (`diffFields` /
	// `formatChangeValue` in internal/server/handlers_documents.go), which is
	// right for every scalar type and wrong for exactly one: a `relation` stores
	// an item ID, so its side is a UUID nobody can read. For a key the item's
	// collection declares as `relation`, the side is rendered in the chip
	// vocabulary every other relation surface uses — `REF · title`, "(deleted)",
	// or `UNRESOLVED_LABEL` — resolved through the SAME `narrowRelationRow`
	// (id-only, scoped to the declared target collection) the table, board,
	// list, filter and properties chip call. Never the id.
	//
	// `multi_relation` needs nothing here: `formatChangeValue` collapses any list
	// to a count ("(2 items)"), so no id reaches the text in the first place.
	import type { FieldDef } from '$lib/types';
	import type { ChangeContext } from '$lib/timeline/changeContext';
	import { relationChipFor, UNRESOLVED_TITLE } from '$lib/collections/relationGroups';

	let {
		text,
		field,
		context,
	}: {
		/** The side's display text as the server wrote it. */
		text: string;
		/** The changed key's field definition, when known. */
		field?: FieldDef | null;
		/** Resolution context from the owning page; absent means render text. */
		context?: ChangeContext;
	} = $props();

	let isRelation = $derived(field?.type === 'relation' && !!text.trim());
	// No context means nothing to resolve against: the chip below then reads
	// as a miss before the index is ready — "Linked item", never the id.
	let chip = $derived(
		isRelation && field
			? relationChipFor(text, (id) => context?.resolveRow(id, field.collection) ?? null)
			: null,
	);
	// `UNRESOLVED_LABEL` says the value does not resolve to an item the viewer
	// can see, and that is only true once the workspace index has loaded. Before
	// that a miss means "not looked up yet", so say that instead, and still never
	// show the id.
	let indexReady = $derived(!!context && context.indexReady());
</script>

{#if !isRelation || !chip}
	{text}
{:else if chip.state === 'unresolved' && !indexReady}
	<span class="change-relation is-unresolved" title="The linked item's details load with the workspace index.">Linked item</span>
{:else if chip.state === 'unresolved'}
	<span class="change-relation is-unresolved" title={UNRESOLVED_TITLE}>{chip.label}</span>
{:else}
	<span class="change-relation" class:is-deleted={chip.state === 'deleted'} title={chip.state === 'deleted' ? 'This item has been deleted.' : undefined}>
		{#if chip.ref}<span class="change-relation-ref">{chip.ref}</span>{/if}
		<span class="change-relation-title">{chip.title ?? ''}</span>
		{#if chip.state === 'deleted'}<span class="change-relation-note">(deleted)</span>{/if}
	</span>
{/if}

<style>
	.change-relation {
		display: inline-flex;
		align-items: baseline;
		gap: var(--space-1);
		max-width: 100%;
		min-width: 0;
	}
	.change-relation-ref {
		flex-shrink: 0;
		color: var(--text-muted);
		font-family: var(--font-mono);
		font-size: 0.94em;
	}
	.change-relation-title {
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}
	/* Same treatment as TableView's relation cell. */
	.change-relation.is-deleted,
	.change-relation.is-unresolved {
		color: var(--text-muted);
	}
	.change-relation-note {
		flex-shrink: 0;
		font-style: italic;
	}
</style>
