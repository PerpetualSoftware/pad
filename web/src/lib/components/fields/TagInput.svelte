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
	// The keyboard-highlighted suggestion (BUG-3150), held by the tag itself
	// rather than by its position: typing re-filters the list, and an index
	// would silently land on whatever slid into that slot (ItemPicker's
	// activeId, same reason). Focus never leaves the input; the highlight is
	// announced through aria-activedescendant.
	let activeTag = $state<string | null>(null);
	const uid = $props.id();

	function hasTag(value: string): boolean {
		const v = value.trim().toLowerCase();
		return tags.some((t) => t.toLowerCase() === v);
	}

	function addTag(raw: string) {
		const value = raw.trim();
		inputValue = '';
		showSuggestions = false;
		activeTag = null;
		if (!value || hasTag(value)) return;
		onchange([...tags, value]);
	}

	function removeTag(index: number) {
		onchange(tags.filter((_, i) => i !== index));
	}

	function handleKeydown(e: KeyboardEvent) {
		if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
			if (filteredSuggestions.length === 0) return;
			e.preventDefault();
			showSuggestions = true;
			const last = filteredSuggestions.length - 1;
			if (e.key === 'ArrowDown') {
				activeTag = filteredSuggestions[Math.min(activeIndex + 1, last)];
			} else {
				activeTag = activeIndex > 0 ? filteredSuggestions[activeIndex - 1] : null;
			}
		} else if (e.key === 'Enter' || e.key === ',') {
			e.preventDefault();
			addTag(e.key === 'Enter' && listOpen && activeIndex >= 0 ? filteredSuggestions[activeIndex] : inputValue);
		} else if (e.key === 'Backspace' && inputValue === '' && tags.length > 0) {
			removeTag(tags.length - 1);
		} else if (e.key === 'Escape') {
			showSuggestions = false;
			activeTag = null;
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
	let listOpen = $derived(showSuggestions && filteredSuggestions.length > 0);
	// -1 when nothing is highlighted, or the highlighted tag was filtered out.
	let activeIndex = $derived(activeTag === null ? -1 : filteredSuggestions.indexOf(activeTag));
</script>

{#if readonly}
	<div class="tag-chips">
		{#if tags.length === 0}
			<span class="tag-empty">No tags</span>
		{:else}
			<!-- Key by index: the write path doesn't enforce per-item tag
			     uniqueness, so a value key would collide on ["ux","ux"]. -->
			{#each tags as tag, i (i)}
				<span class="tag-chip readonly">{tag}</span>
			{/each}
		{/if}
	</div>
{:else}
	<div class="tag-input">
		<div class="tag-chips">
			{#each tags as tag, i (i)}
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
				role="combobox"
				aria-expanded={listOpen}
				aria-controls="tag-suggestions-{uid}"
				aria-autocomplete="list"
				aria-activedescendant={listOpen && activeIndex >= 0 ? `tag-suggestion-${uid}-${activeIndex}` : undefined}
				onkeydown={handleKeydown}
				onfocus={() => (showSuggestions = true)}
				onblur={() => setTimeout(() => ((showSuggestions = false), (activeTag = null)), 120)}
			/>
		</div>
		{#if listOpen}
			<div class="tag-suggestions" role="listbox" id="tag-suggestions-{uid}" aria-label="Tag suggestions">
				{#each filteredSuggestions as s, i (s)}
					<!-- Not a tab stop (BUG-3148): the list unmounts 120ms after the input
					     blurs, so tabbing onto a suggestion landed focus on a button about
					     to disappear, dropping it to <body>. Pointer picks via onmousedown
					     (the input keeps focus); the keyboard picks with the arrow keys and
					     Enter from the INPUT, via aria-activedescendant (BUG-3150), so focus
					     never has to move here. -->
					<button
						type="button"
						class="tag-suggestion"
						class:active={i === activeIndex}
						role="option"
						id="tag-suggestion-{uid}-{i}"
						aria-selected={i === activeIndex}
						tabindex="-1"
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
	.tag-suggestion:hover,
	.tag-suggestion.active {
		background: var(--bg-hover, var(--bg-secondary));
	}
</style>
