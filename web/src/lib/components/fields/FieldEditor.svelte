<!--
@component
Custom-styled field editor for item detail pages.
Supports text, number, select, multi_select, date, checkbox, and url field types.

Usage:
```svelte
<FieldEditor {field} {value} onchange={(v) => handleChange(v)} />
```
-->
<script lang="ts">
	import type { FieldDef } from '$lib/types';
	import BottomSheet from '$lib/components/common/BottomSheet.svelte';

	interface Props {
		field: FieldDef;
		value: any;
		onchange: (value: any) => void;
	}

	let { field, value, onchange }: Props = $props();

	// ── Viewport detection ───────────────────────────────────────────────
	// On mobile the absolute-positioned select dropdown can clip at the
	// edge of the properties panel; swap it for a BottomSheet of options.
	// Desktop keeps the inline dropdown with keyboard nav unchanged.
	let isMobile = $state(false);
	$effect(() => {
		if (typeof window === 'undefined') return;
		const mq = window.matchMedia('(max-width: 639.98px)');
		isMobile = mq.matches;
		const onChange = (e: MediaQueryListEvent) => {
			isMobile = e.matches;
			// If the viewport crosses above mobile while the sheet is open
			// (e.g. rotation), close it so it doesn't spring back on return.
			if (!e.matches) {
				dropdownOpen = false;
			}
		};
		mq.addEventListener('change', onChange);
		return () => mq.removeEventListener('change', onChange);
	});

	// ── Date input state ──────────────────────────────────────────────────

	let dateInputEl: HTMLInputElement | undefined = $state(undefined);

	function formatDate(dateStr: string): string {
		try {
			const d = new Date(dateStr + 'T00:00:00');
			return d.toLocaleDateString('en-US', { month: 'short', day: 'numeric', year: 'numeric' });
		} catch {
			return dateStr;
		}
	}

	// ── Select dropdown state ──────────────────────────────────────────────

	let dropdownOpen = $state(false);
	let focusedIndex = $state(-1);
	let triggerEl: HTMLButtonElement | undefined = $state(undefined);
	let dropdownEl: HTMLDivElement | undefined = $state(undefined);

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
		// On mobile the BottomSheet owns dismissal (backdrop tap + Escape +
		// close button) — skip this handler so it doesn't race the sheet.
		if (isMobile) return;
		if (
			dropdownOpen &&
			triggerEl &&
			dropdownEl &&
			!triggerEl.contains(e.target as Node) &&
			!dropdownEl.contains(e.target as Node)
		) {
			dropdownOpen = false;
		}
	}

	// ── Input handlers ─────────────────────────────────────────────────────

	function handleTextInput(e: Event) {
		const target = e.target as HTMLInputElement;
		onchange(target.value);
	}

	function handleNumberInput(e: Event) {
		const target = e.target as HTMLInputElement;
		if (target.value === '') { onchange(null); return; }
		const num = Number(target.value);
		if (!isNaN(num)) onchange(num);
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

</script>

<svelte:window onclick={handleWindowClick} />

{#if field.type === 'select'}
	{#snippet selectOptions()}
		{#if field.options}
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
		{/if}
	{/snippet}

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

		{#if isMobile && dropdownOpen}
			<!--
				Mobile: full-width BottomSheet of options, titled with the
				field label (e.g. "Set status"). Gated on `dropdownOpen`
				(gate-on-open pattern) so the sheet + its global keydown
				listener isn't mounted per idle FieldEditor instance.
			-->
			<BottomSheet
				open={dropdownOpen}
				onclose={() => (dropdownOpen = false)}
				title="Set {field.label.toLowerCase()}"
			>
				<div class="select-sheet-body" role="listbox" aria-label="{field.label} options">
					{@render selectOptions()}
				</div>
			</BottomSheet>
		{:else if dropdownOpen && field.options}
			<div
				bind:this={dropdownEl}
				class="select-dropdown"
				role="listbox"
				aria-label="{field.label} options"
			>
				{@render selectOptions()}
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
	<!-- Custom date picker -->
	<div class="date-wrapper">
		<button
			class="select-trigger date-trigger"
			type="button"
			onclick={() => dateInputEl?.showPicker()}
		>
			{#if value}
				<span class="date-label">{formatDate(value)}</span>
				<span
					class="clear-btn"
					role="button"
					tabindex="0"
					onclick={(e) => { e.stopPropagation(); onchange(null); }}
					onkeydown={(e) => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); e.stopPropagation(); onchange(null); } }}
				>&#x2715;</span>
			{:else}
				<span class="date-placeholder">Pick a date...</span>
			{/if}
			<svg class="select-chevron" width="14" height="14" viewBox="0 0 14 14" fill="none" aria-hidden="true">
				<rect x="2" y="3" width="10" height="9" rx="1.5" stroke="currentColor" stroke-width="1.2" />
				<line x1="2" y1="6" x2="12" y2="6" stroke="currentColor" stroke-width="1.2" />
				<line x1="5" y1="1.5" x2="5" y2="4" stroke="currentColor" stroke-width="1.2" stroke-linecap="round" />
				<line x1="9" y1="1.5" x2="9" y2="4" stroke="currentColor" stroke-width="1.2" stroke-linecap="round" />
			</svg>
		</button>
		<input
			bind:this={dateInputEl}
			class="date-hidden-input"
			type="date"
			value={value ?? ''}
			onchange={handleDateInput}
			tabindex={-1}
			aria-hidden="true"
		/>
	</div>

{:else if field.type === 'number'}
	<!-- Custom number input with +/- buttons -->
	<div class="number-wrapper">
		<button
			class="number-btn"
			type="button"
			tabindex={-1}
			aria-label="Decrease"
			onclick={() => onchange((Number(value) || 0) - 1)}
		>
			<svg width="10" height="10" viewBox="0 0 10 10" fill="none" aria-hidden="true">
				<line x1="2" y1="5" x2="8" y2="5" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" />
			</svg>
		</button>
		<input
			class="number-input"
			type="text"
			inputmode="numeric"
			value={value ?? ''}
			oninput={handleNumberInput}
			placeholder="—"
		/>
		{#if field.suffix}
			<span class="number-suffix">{field.suffix}</span>
		{/if}
		<button
			class="number-btn"
			type="button"
			tabindex={-1}
			aria-label="Increase"
			onclick={() => onchange((Number(value) || 0) + 1)}
		>
			<svg width="10" height="10" viewBox="0 0 10 10" fill="none" aria-hidden="true">
				<line x1="2" y1="5" x2="8" y2="5" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" />
				<line x1="5" y1="2" x2="5" y2="8" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" />
			</svg>
		</button>
	</div>

{:else if field.type === 'url'}
	<!-- URL input with link icon -->
	<div class="url-wrapper">
		<input
			class="field-input url-input"
			type="url"
			value={value ?? ''}
			oninput={handleTextInput}
			placeholder="https://..."
		/>
		{#if value}
			<a
				class="url-open"
				href={value}
				target="_blank"
				rel="noopener noreferrer"
				title="Open link"
			>
				<svg width="12" height="12" viewBox="0 0 12 12" fill="none" aria-hidden="true">
					<path d="M5 1H2.5A1.5 1.5 0 001 2.5v7A1.5 1.5 0 002.5 11h7A1.5 1.5 0 0011 9.5V7" stroke="currentColor" stroke-width="1.2" stroke-linecap="round" />
					<path d="M7 1h4v4M11 1L5.5 6.5" stroke="currentColor" stroke-width="1.2" stroke-linecap="round" stroke-linejoin="round" />
				</svg>
			</a>
		{/if}
	</div>

{:else}
	<!-- Text / Multi-select fallback -->
	<input
		class="field-input"
		type="text"
		value={value ?? ''}
		oninput={handleTextInput}
	/>
{/if}

<style>
	/* ── Shared input styles ──────────────────────────────────────────── */

	.field-input {
		width: 100%;
		padding: var(--space-1) var(--space-2);
		min-height: 30px;
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

	/* ── Date picker ─────────────────────────────────────────────────── */

	.date-wrapper {
		position: relative;
		width: 100%;
	}

	.date-trigger {
		position: relative;
	}

	.date-label {
		flex: 1;
		min-width: 0;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}

	.date-placeholder {
		flex: 1;
		color: var(--text-muted);
	}

	.date-hidden-input {
		position: absolute;
		inset: 0;
		opacity: 0;
		width: 100%;
		height: 100%;
		cursor: pointer;
		pointer-events: none;
	}

	/* ── Number input ────────────────────────────────────────────────── */

	.number-wrapper {
		display: flex;
		align-items: center;
		width: 100%;
		background: var(--bg-tertiary);
		border: 1px solid transparent;
		border-radius: var(--radius-sm);
		transition: border-color 0.15s;
		overflow: hidden;
	}

	.number-wrapper:hover {
		border-color: var(--border);
	}

	.number-wrapper:focus-within {
		border-color: var(--accent-blue);
	}

	.number-btn {
		display: flex;
		align-items: center;
		justify-content: center;
		width: 28px;
		min-height: 30px;
		padding: 0;
		background: none;
		border: none;
		color: var(--text-muted);
		cursor: pointer;
		flex-shrink: 0;
		transition: color 0.1s, background 0.1s;
	}

	.number-btn:hover {
		color: var(--text-primary);
		background: var(--bg-hover);
	}

	.number-btn:active {
		background: var(--bg-active);
	}

	.number-input {
		flex: 1;
		min-width: 0;
		padding: var(--space-1) var(--space-1);
		font-size: 0.88em;
		font-family: inherit;
		color: var(--text-primary);
		background: transparent;
		border: none;
		outline: none;
		text-align: center;
		box-sizing: border-box;
	}

	.number-input::placeholder {
		color: var(--text-muted);
	}

	.number-suffix {
		font-size: 0.82em;
		color: var(--text-muted);
		padding-right: var(--space-2);
		pointer-events: none;
		user-select: none;
		flex-shrink: 0;
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
		min-height: 30px;
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

	/* ── Mobile sheet body — roomier rows for touch ──────────────────── */

	.select-sheet-body {
		display: flex;
		flex-direction: column;
		padding: 0 var(--space-2) var(--space-3);
	}

	.select-sheet-body .select-option {
		padding: var(--space-3);
		font-size: 1em;
		border-radius: var(--radius-sm);
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

	/* ── URL input ────────────────────────────────────────────────────── */

	.url-wrapper {
		position: relative;
		display: flex;
		align-items: center;
		width: 100%;
	}

	.url-input {
		padding-right: calc(var(--space-2) + 20px);
	}

	.url-open {
		position: absolute;
		right: var(--space-2);
		display: flex;
		align-items: center;
		justify-content: center;
		color: var(--text-muted);
		padding: 2px;
		border-radius: var(--radius-sm);
		transition: color 0.1s;
	}

	.url-open:hover {
		color: var(--accent-blue);
	}
</style>
