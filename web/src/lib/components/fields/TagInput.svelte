<script lang="ts">
	/**
	 * Free-form tag chip editor. Tags live on `item.tags` (a JSON-array
	 * string), NOT in the collection schema — so this is a sibling of
	 * FieldEditor rather than a field type. Emits the full new tag array via
	 * `onchange`; the parent persists it (PATCH items.tags). Dedupe is
	 * case-insensitive but tags are stored as typed (human-readable).
	 */
	interface Props {
		tags: string[];
		onchange: (tags: string[]) => void;
		suggestions?: string[];
		readonly?: boolean;
	}

	let { tags, onchange, suggestions = [], readonly = false }: Props = $props();

	let inputValue = $state('');
	let showSuggestions = $state(false);

	function hasTag(value: string): boolean {
		const v = value.trim().toLowerCase();
		return tags.some((t) => t.toLowerCase() === v);
	}

	function addTag(raw: string) {
		const value = raw.trim();
		inputValue = '';
		showSuggestions = false;
		if (!value || hasTag(value)) return;
		onchange([...tags, value]);
	}

	function removeTag(index: number) {
		onchange(tags.filter((_, i) => i !== index));
	}

	function handleKeydown(e: KeyboardEvent) {
		if (e.key === 'Enter' || e.key === ',') {
			e.preventDefault();
			addTag(inputValue);
		} else if (e.key === 'Backspace' && inputValue === '' && tags.length > 0) {
			removeTag(tags.length - 1);
		} else if (e.key === 'Escape') {
			showSuggestions = false;
		}
	}

	// Suggestions not already applied, matched case-insensitively to the input.
	let filteredSuggestions = $derived.by(() => {
		const q = inputValue.trim().toLowerCase();
		return suggestions
			.filter((s) => !hasTag(s))
			.filter((s) => q === '' || s.toLowerCase().includes(q))
			.slice(0, 8);
	});
</script>

{#if readonly}
	<div class="tag-chips">
		{#if tags.length === 0}
			<span class="tag-empty">No tags</span>
		{:else}
			{#each tags as tag (tag)}
				<span class="tag-chip readonly">{tag}</span>
			{/each}
		{/if}
	</div>
{:else}
	<div class="tag-input">
		<div class="tag-chips">
			{#each tags as tag, i (tag)}
				<span class="tag-chip">
					{tag}
					<button
						type="button"
						class="tag-remove"
						aria-label={`Remove ${tag}`}
						onclick={() => removeTag(i)}>×</button
					>
				</span>
			{/each}
			<input
				bind:value={inputValue}
				class="tag-entry"
				type="text"
				placeholder={tags.length === 0 ? 'Add tags…' : ''}
				onkeydown={handleKeydown}
				onfocus={() => (showSuggestions = true)}
				onblur={() => setTimeout(() => (showSuggestions = false), 120)}
			/>
		</div>
		{#if showSuggestions && filteredSuggestions.length > 0}
			<div class="tag-suggestions">
				{#each filteredSuggestions as s (s)}
					<button
						type="button"
						class="tag-suggestion"
						onmousedown={(e) => {
							e.preventDefault();
							addTag(s);
						}}>{s}</button
					>
				{/each}
			</div>
		{/if}
	</div>
{/if}

<style>
	.tag-input {
		position: relative;
		width: 100%;
	}
	.tag-chips {
		display: flex;
		flex-wrap: wrap;
		gap: var(--space-1, 0.25rem);
		align-items: center;
	}
	.tag-chip {
		display: inline-flex;
		align-items: center;
		gap: 0.25em;
		padding: 0.1em 0.5em;
		font-size: var(--text-xs, 0.75rem);
		line-height: 1.5;
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: 999px;
		color: var(--text-primary);
		white-space: nowrap;
	}
	.tag-chip.readonly {
		color: var(--text-secondary);
	}
	.tag-remove {
		display: inline-flex;
		align-items: center;
		justify-content: center;
		padding: 0;
		width: 1.1em;
		height: 1.1em;
		border: none;
		background: transparent;
		color: var(--text-secondary);
		font-size: 1.1em;
		line-height: 1;
		cursor: pointer;
		border-radius: 50%;
	}
	.tag-remove:hover {
		color: var(--text-primary);
		background: var(--bg-hover, rgba(0, 0, 0, 0.06));
	}
	.tag-entry {
		flex: 1;
		min-width: 6ch;
		border: none;
		background: transparent;
		color: var(--text-primary);
		font-size: var(--text-sm, 0.875rem);
		padding: 0.15em 0.1em;
		outline: none;
	}
	.tag-empty {
		font-size: var(--text-xs, 0.75rem);
		color: var(--text-tertiary, var(--text-secondary));
	}
	.tag-suggestions {
		position: absolute;
		top: calc(100% + 2px);
		left: 0;
		z-index: 20;
		min-width: 10rem;
		max-width: 100%;
		display: flex;
		flex-direction: column;
		background: var(--bg-primary);
		border: 1px solid var(--border);
		border-radius: var(--radius-md, 6px);
		box-shadow: 0 4px 16px rgba(0, 0, 0, 0.12);
		overflow: hidden;
	}
	.tag-suggestion {
		text-align: left;
		padding: 0.35em 0.6em;
		border: none;
		background: transparent;
		color: var(--text-primary);
		font-size: var(--text-sm, 0.875rem);
		cursor: pointer;
	}
	.tag-suggestion:hover {
		background: var(--bg-hover, var(--bg-secondary));
	}
</style>
