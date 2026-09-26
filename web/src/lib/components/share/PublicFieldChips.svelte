<script lang="ts">
	// The field chips of a shared item, one component for both share views
	// (TASK-2248 U3): the collection inline-expand and the direct item share.
	// Presentational only. `fieldChips` in shareView.ts decides what shows and
	// how it reads.
	import { formatLabel, PUBLIC_RELATION_TITLE, type FieldChip } from './shareView';

	let { chips }: { chips: FieldChip[] } = $props();
</script>

{#if chips.length > 0}
	<dl class="public-field-chips">
		{#each chips as { field, text, placeholder, color } (field.key)}
			<div class="field-chip">
				<dt class="field-chip-label">{field.label || formatLabel(field.key)}</dt>
				{#if placeholder}
					<dd class="field-chip-value is-placeholder" title={PUBLIC_RELATION_TITLE}>{text}</dd>
				{:else}
					<dd class="field-chip-value" style:color>{text}</dd>
				{/if}
			</div>
		{/each}
	</dl>
{/if}

<style>
	.public-field-chips {
		display: flex;
		flex-wrap: wrap;
		gap: var(--space-2);
		margin: 0;
	}

	.field-chip {
		display: inline-flex;
		align-items: center;
		gap: var(--space-1);
		padding: var(--space-1) var(--space-3);
		background: var(--bg-tertiary);
		border-radius: 999px;
		font-size: 0.82em;
	}

	.field-chip-label {
		color: var(--text-muted);
		font-weight: 500;
		margin: 0;
	}

	.field-chip-value {
		color: var(--text-primary);
		margin: 0;
	}

	/* A value this share cannot resolve (BUG-3016): reads as a note about the
	   field rather than as its content. Matches PublicTableView's cell. */
	.field-chip-value.is-placeholder {
		color: var(--text-muted);
		font-style: italic;
	}
</style>
