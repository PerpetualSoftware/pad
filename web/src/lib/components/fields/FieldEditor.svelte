<!--
@component
Custom-styled field editor for item detail pages.
Supports text, number, select, multi_select, date, checkbox, url, and relation field types.

Usage:
```svelte
<FieldEditor {field} {value} onchange={(v) => handleChange(v)} wsSlug="my-workspace" />
```
-->
<script lang="ts">
	import type { FieldDef } from '$lib/types';
	import { api } from '$lib/api/client';
	import { formatItemRef } from '$lib/types';
	import type { Item } from '$lib/types';

	interface Props {
		field: FieldDef;
		value: any;
		onchange: (value: any) => void;
		wsSlug?: string;
	}

	let { field, value, onchange, wsSlug }: Props = $props();

	// ── Select dropdown state ──────────────────────────────────────────────

	let dropdownOpen = $state(false);
	let focusedIndex = $state(-1);
	let triggerEl: HTMLButtonElement | undefined = $state(undefined);
	let dropdownEl: HTMLDivElement | undefined = $state(undefined);

	// ── Relation picker state ─────────────────────────────────────────────

	let relationOpen = $state(false);
	let relationItems = $state<Item[]>([]);
	let relationLoading = $state(false);
	let relationSearch = $state('');
	let relationFocusedIndex = $state(-1);
	let relationTriggerEl: HTMLButtonElement | undefined = $state(undefined);
	let relationDropdownEl: HTMLDivElement | undefined = $state(undefined);

	let selectedRelationItem = $derived(
		value ? relationItems.find((i) => i.id === value) : null
	);

	let filteredRelationItems = $derived.by(() => {
		if (!relationSearch.trim()) return relationItems;
		const q = relationSearch.toLowerCase();
		return relationItems.filter(
			(i) =>
				i.title.toLowerCase().includes(q) ||
				(formatItemRef(i) ?? '').toLowerCase().includes(q)
		);
	});

	async function openRelationPicker() {
		relationOpen = true;
		relationSearch = '';
		relationFocusedIndex = -1;
		if (relationItems.length === 0 && field.collection) {
			relationLoading = true;
			try {
				if (wsSlug && field.collection) {
					relationItems = await api.items.listByCollection(wsSlug, field.collection);
				}
			} catch {
				relationItems = [];
			} finally {
				relationLoading = false;
			}
		}
	}

	function selectRelation(item: Item) {
		onchange(item.id);
		relationOpen = false;
		relationTriggerEl?.focus();
	}

	function clearRelation() {
		onchange('');
		relationOpen = false;
	}

	function handleRelationKeydown(e: KeyboardEvent) {
		if (!relationOpen) return;
		const items = filteredRelationItems;

		if (e.key === 'ArrowDown') {
			e.preventDefault();
			relationFocusedIndex =
				items.length > 0 ? (relationFocusedIndex + 1) % items.length : -1;
		} else if (e.key === 'ArrowUp') {
			e.preventDefault();
			relationFocusedIndex =
				items.length > 0
					? (relationFocusedIndex - 1 + items.length) % items.length
					: -1;
		} else if (e.key === 'Enter') {
			e.preventDefault();
			if (relationFocusedIndex >= 0 && relationFocusedIndex < items.length) {
				selectRelation(items[relationFocusedIndex]);
			}
		} else if (e.key === 'Escape') {
			e.preventDefault();
			relationOpen = false;
			relationTriggerEl?.focus();
		}
	}

	const STATUS_COLORS: Record<string, string> = {
		open: 'var(--accent-blue)',
		new: 'var(--accent-blue)',
		in_progress: 'var(--accent-amber)',
		active: 'var(--accent-green)',
		done: 'var(--accent-green)',
		completed: 'var(--accent-green)',
		published: 'var(--accent-green)',
		blocked: 'var(--accent-orange)',
		rejected: 'var(--accent-orange)',
		critical: 'var(--accent-orange)',
		high: 'var(--accent-amber)',
		medium: 'var(--accent-blue)',
		low: 'var(--text-muted)',
		draft: 'var(--text-muted)',
		closed: 'var(--text-muted)',
		archived: 'var(--text-muted)'
	};

	function getStatusColor(val: string): string | null {
		return STATUS_COLORS[val] ?? null;
	}

	function formatLabel(val: string): string {
		return val
			.replace(/_/g, ' ')
			.replace(/\b\w/g, (c) => c.toUpperCase());
	}

	function toggleDropdown() {
		dropdownOpen = !dropdownOpen;
		if (dropdownOpen) {
			focusedIndex = field.options?.indexOf(value) ?? -1;
		}
	}

	function selectOption(opt: string) {
		onchange(opt);
		dropdownOpen = false;
		triggerEl?.focus();
	}

	function handleDropdownKeydown(e: KeyboardEvent) {
		if (!dropdownOpen || !field.options) return;
		const opts = field.options;

		if (e.key === 'ArrowDown') {
			e.preventDefault();
			focusedIndex = (focusedIndex + 1) % opts.length;
		} else if (e.key === 'ArrowUp') {
			e.preventDefault();
			focusedIndex = (focusedIndex - 1 + opts.length) % opts.length;
		} else if (e.key === 'Enter') {
			e.preventDefault();
			if (focusedIndex >= 0 && focusedIndex < opts.length) {
				selectOption(opts[focusedIndex]);
			}
		} else if (e.key === 'Escape') {
			e.preventDefault();
			dropdownOpen = false;
			triggerEl?.focus();
		}
	}

	function handleWindowClick(e: MouseEvent) {
		if (
			dropdownOpen &&
			triggerEl &&
			dropdownEl &&
			!triggerEl.contains(e.target as Node) &&
			!dropdownEl.contains(e.target as Node)
		) {
			dropdownOpen = false;
		}
		if (
			relationOpen &&
			relationTriggerEl &&
			relationDropdownEl &&
			!relationTriggerEl.contains(e.target as Node) &&
			!relationDropdownEl.contains(e.target as Node)
		) {
			relationOpen = false;
		}
	}

	// ── Input handlers ─────────────────────────────────────────────────────

	function handleTextInput(e: Event) {
		const target = e.target as HTMLInputElement;
		onchange(target.value);
	}

	function handleNumberInput(e: Event) {
		const target = e.target as HTMLInputElement;
		const num = target.value === '' ? null : Number(target.value);
		onchange(num);
	}

	function handleDateInput(e: Event) {
		const target = e.target as HTMLInputElement;
		onchange(target.value || null);
	}

	function handleCheckboxToggle() {
		onchange(!value);
	}

	// ── Scroll focused option into view ────────────────────────────────────

	$effect(() => {
		if (dropdownOpen && focusedIndex >= 0 && dropdownEl) {
			const items = dropdownEl.querySelectorAll('[role="option"]');
			const item = items[focusedIndex] as HTMLElement | undefined;
			item?.scrollIntoView({ block: 'nearest' });
		}
	});

	$effect(() => {
		if (relationOpen && relationFocusedIndex >= 0 && relationDropdownEl) {
			const items = relationDropdownEl.querySelectorAll('[role="option"]');
			const item = items[relationFocusedIndex] as HTMLElement | undefined;
			item?.scrollIntoView({ block: 'nearest' });
		}
	});
</script>

<svelte:window onclick={handleWindowClick} />

{#if field.type === 'select'}
	<!-- Custom select dropdown -->
	<div class="select-wrapper">
		<button
			bind:this={triggerEl}
			class="select-trigger"
			type="button"
			aria-haspopup="listbox"
			aria-expanded={dropdownOpen}
			onclick={toggleDropdown}
			onkeydown={handleDropdownKeydown}
		>
			{#if value && getStatusColor(value)}
				<span class="color-dot" style:background={getStatusColor(value)}></span>
			{/if}
			<span class="select-label">
				{value ? formatLabel(value) : '\u2014'}
			</span>
			<svg class="select-chevron" width="12" height="12" viewBox="0 0 12 12" fill="none" aria-hidden="true">
				<path d="M3 4.5L6 7.5L9 4.5" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" />
			</svg>
		</button>

		{#if dropdownOpen && field.options}
			<div
				bind:this={dropdownEl}
				class="select-dropdown"
				role="listbox"
				aria-label="{field.label} options"
			>
				{#each field.options as option, i (option)}
					<button
						class="select-option"
						class:selected={option === value}
						class:focused={i === focusedIndex}
						type="button"
						role="option"
						aria-selected={option === value}
						onclick={() => selectOption(option)}
						onmouseenter={() => (focusedIndex = i)}
					>
						{#if getStatusColor(option)}
							<span class="color-dot" style:background={getStatusColor(option)}></span>
						{/if}
						<span>{formatLabel(option)}</span>
					</button>
				{/each}
			</div>
		{/if}
	</div>

{:else if field.type === 'checkbox'}
	<!-- Toggle switch -->
	<button
		class="toggle"
		class:on={!!value}
		type="button"
		role="switch"
		aria-checked={!!value}
		aria-label={field.label}
		onclick={handleCheckboxToggle}
	>
		<span class="toggle-knob"></span>
	</button>

{:else if field.type === 'date'}
	<!-- Date input -->
	<input
		class="field-input date-input"
		type="date"
		value={value ?? ''}
		onchange={handleDateInput}
	/>

{:else if field.type === 'number'}
	<!-- Number input with optional suffix -->
	{#if field.suffix}
		<div class="number-wrapper">
			<input
				class="field-input number-input"
				type="number"
				value={value ?? ''}
				oninput={handleNumberInput}
			/>
			<span class="number-suffix">{field.suffix}</span>
		</div>
	{:else}
		<input
			class="field-input"
			type="number"
			value={value ?? ''}
			oninput={handleNumberInput}
		/>
	{/if}

{:else if field.type === 'relation'}
	<!-- Relation picker dropdown -->
	<div class="select-wrapper">
		<button
			bind:this={relationTriggerEl}
			class="select-trigger"
			type="button"
			aria-haspopup="listbox"
			aria-expanded={relationOpen}
			onclick={openRelationPicker}
			onkeydown={handleRelationKeydown}
		>
			<span class="select-label">
				{#if selectedRelationItem}
					{#if formatItemRef(selectedRelationItem)}
						<span class="relation-ref">{formatItemRef(selectedRelationItem)}</span>
					{/if}
					{selectedRelationItem.title}
				{:else if value}
					{value}
				{:else}
					<span style="color: var(--text-muted)">Select {field.label}...</span>
				{/if}
			</span>
			{#if value}
				<!-- Use span instead of button to avoid nesting <button> inside <button> -->
				<span
					class="clear-btn"
					role="button"
					tabindex="0"
					onclick={(e) => {
						e.stopPropagation();
						clearRelation();
					}}
					onkeydown={(e) => {
						if (e.key === 'Enter' || e.key === ' ') {
							e.preventDefault();
							e.stopPropagation();
							clearRelation();
						}
					}}
				>&#x2715;</span>
			{/if}
			<svg class="select-chevron" width="12" height="12" viewBox="0 0 12 12" fill="none" aria-hidden="true">
				<path d="M3 4.5L6 7.5L9 4.5" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" />
			</svg>
		</button>

		{#if relationOpen}
			<div
				bind:this={relationDropdownEl}
				class="select-dropdown relation-dropdown"
				role="listbox"
				aria-label="{field.label} options"
			>
				<input
					class="relation-search"
					type="text"
					placeholder="Search..."
					bind:value={relationSearch}
					onkeydown={handleRelationKeydown}
				/>
				{#if relationLoading}
					<div class="relation-loading">Loading...</div>
				{:else if filteredRelationItems.length === 0}
					<div class="relation-empty">No items found</div>
				{:else}
					{#each filteredRelationItems as item, i (item.id)}
						<button
							class="select-option"
							class:selected={item.id === value}
							class:focused={i === relationFocusedIndex}
							type="button"
							role="option"
							aria-selected={item.id === value}
							onclick={() => selectRelation(item)}
							onmouseenter={() => (relationFocusedIndex = i)}
						>
							{#if formatItemRef(item)}
								<span class="relation-ref">{formatItemRef(item)}</span>
							{/if}
							<span>{item.title}</span>
						</button>
					{/each}
				{/if}
			</div>
		{/if}
	</div>

{:else}
	<!-- Text / URL / Multi-select fallback -->
	<input
		class="field-input"
		type="text"
		value={value ?? ''}
		oninput={handleTextInput}
		placeholder={field.type === 'url' ? 'https://...' : ''}
	/>
{/if}

<style>
	/* ── Shared input styles ──────────────────────────────────────────── */

	.field-input {
		width: 100%;
		padding: var(--space-1) var(--space-2);
		font-size: 0.88em;
		font-family: inherit;
		color: var(--text-primary);
		background: var(--bg-tertiary);
		border: 1px solid transparent;
		border-radius: var(--radius-sm);
		outline: none;
		transition: border-color 0.15s;
		box-sizing: border-box;
	}

	.field-input:hover {
		border-color: var(--border);
	}

	.field-input:focus {
		border-color: var(--accent-blue);
	}

	.field-input::placeholder {
		color: var(--text-muted);
	}

	/* ── Date input ───────────────────────────────────────────────────── */

	.date-input {
		color-scheme: dark;
	}

	:global([data-theme='light']) .date-input {
		color-scheme: light;
	}

	/* ── Number with suffix ───────────────────────────────────────────── */

	.number-wrapper {
		position: relative;
		display: flex;
		align-items: center;
		width: 100%;
	}

	.number-input {
		padding-right: calc(var(--space-2) + 2em);
	}

	.number-suffix {
		position: absolute;
		right: var(--space-2);
		font-size: 0.88em;
		color: var(--text-muted);
		pointer-events: none;
		user-select: none;
	}

	/* ── Select dropdown ──────────────────────────────────────────────── */

	.select-wrapper {
		position: relative;
		width: 100%;
	}

	.select-trigger {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		width: 100%;
		padding: var(--space-1) var(--space-2);
		font-size: 0.88em;
		font-family: inherit;
		color: var(--text-primary);
		background: var(--bg-tertiary);
		border: 1px solid transparent;
		border-radius: var(--radius-sm);
		cursor: pointer;
		outline: none;
		transition: border-color 0.15s;
		text-align: left;
		box-sizing: border-box;
	}

	.select-trigger:hover {
		border-color: var(--border);
	}

	.select-trigger:focus {
		border-color: var(--accent-blue);
	}

	.select-label {
		flex: 1;
		min-width: 0;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}

	.select-chevron {
		flex-shrink: 0;
		color: var(--text-muted);
		transition: transform 0.15s;
	}

	.color-dot {
		display: inline-block;
		width: 8px;
		height: 8px;
		border-radius: 50%;
		flex-shrink: 0;
	}

	.select-dropdown {
		position: absolute;
		top: calc(100% + 4px);
		left: 0;
		right: 0;
		z-index: 50;
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius-sm);
		padding: var(--space-1) 0;
		max-height: 200px;
		overflow-y: auto;
		box-shadow: 0 4px 12px rgba(0, 0, 0, 0.3);
	}

	.select-option {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		width: 100%;
		padding: var(--space-1) var(--space-2);
		font-size: 0.88em;
		font-family: inherit;
		color: var(--text-primary);
		background: none;
		border: none;
		cursor: pointer;
		text-align: left;
		box-sizing: border-box;
	}

	.select-option:hover,
	.select-option.focused {
		background: var(--bg-hover);
	}

	.select-option.selected {
		background: var(--bg-active);
	}

	/* ── Checkbox toggle switch ───────────────────────────────────────── */

	.toggle {
		position: relative;
		width: 36px;
		height: 20px;
		padding: 0;
		background: var(--bg-tertiary);
		border: none;
		border-radius: 10px;
		cursor: pointer;
		transition: background-color 0.15s;
		flex-shrink: 0;
	}

	.toggle.on {
		background: var(--accent-blue);
	}

	.toggle-knob {
		position: absolute;
		top: 2px;
		left: 2px;
		width: 16px;
		height: 16px;
		background: white;
		border-radius: 50%;
		transition: transform 0.15s;
		pointer-events: none;
	}

	.toggle.on .toggle-knob {
		transform: translateX(16px);
	}

	/* ── Remove spinner from number inputs (optional) ─────────────────── */

	.field-input[type='number']::-webkit-inner-spin-button,
	.field-input[type='number']::-webkit-outer-spin-button {
		-webkit-appearance: none;
		margin: 0;
	}

	.field-input[type='number'] {
		-moz-appearance: textfield;
	}

	/* ── Relation picker ──────────────────────────────────────────────── */

	.relation-dropdown {
		max-height: 280px;
	}

	.relation-search {
		width: 100%;
		padding: var(--space-1) var(--space-2);
		font-size: 0.85em;
		font-family: inherit;
		color: var(--text-primary);
		background: var(--bg-tertiary);
		border: none;
		border-bottom: 1px solid var(--border);
		outline: none;
		box-sizing: border-box;
	}

	.relation-search:focus {
		background: var(--bg-primary);
	}

	.relation-ref {
		font-family: var(--font-mono);
		font-size: 0.85em;
		color: var(--text-muted);
		margin-right: 4px;
		flex-shrink: 0;
	}

	.relation-loading,
	.relation-empty {
		padding: var(--space-2) var(--space-3);
		font-size: 0.85em;
		color: var(--text-muted);
		text-align: center;
	}

	.clear-btn {
		background: none;
		border: none;
		color: var(--text-muted);
		font-size: 0.75em;
		cursor: pointer;
		padding: 2px;
		border-radius: var(--radius-sm);
		flex-shrink: 0;
	}

	.clear-btn:hover {
		color: var(--accent-orange);
		background: var(--bg-hover);
	}
</style>
