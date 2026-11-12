<script lang="ts" generics="T extends string">
	interface Option {
		value: T;
		label: string;
		/** Shown after the label in a quieter weight. Text carries a count that is not exact, such as "12+". */
		count?: number | string;
		disabled?: boolean;
		/** `tablist` mode: id of the panel this tab shows. */
		controls?: string;
		/** Element id of the option, so a panel can name the tab that labels it. */
		id?: string;
	}

	interface Props {
		options: Option[];
		/** The selected option's value. */
		value: T;
		onchange: (value: T) => void;
		/** Accessible name of the group or tablist. */
		label: string;
		/** `pressed`: toggle buttons that switch a view in place (aria-pressed).
		 *  `tablist`: tabs that each show a panel (role=tab, aria-selected). */
		mode?: 'pressed' | 'tablist';
		/** Stretch to the container, sharing the width between options. */
		fill?: boolean;
	}

	let { options, value, onchange, label, mode = 'pressed', fill = false }: Props = $props();

	let root = $state<HTMLDivElement>();
	const enabled = $derived(options.filter(option => !option.disabled));
	// One tab stop: the selected option, or the first enabled one when nothing selectable is selected.
	const stop = $derived((enabled.find(option => option.value === value) ?? enabled[0])?.value);

	function select(option: Option) {
		if (!option.disabled && option.value !== value) onchange(option.value);
	}

	function onkeydown(event: KeyboardEvent, option: Option) {
		if (event.altKey || event.ctrlKey || event.metaKey || enabled.length === 0) return;
		const at = enabled.findIndex(candidate => candidate.value === option.value);
		const rtl = root ? getComputedStyle(root).direction === 'rtl' : false;
		const step = (delta: number) => enabled[(Math.max(at, 0) + delta + enabled.length) % enabled.length];
		let next: Option;
		switch (event.key) {
			case 'ArrowRight': next = step(rtl ? -1 : 1); break;
			case 'ArrowLeft': next = step(rtl ? 1 : -1); break;
			case 'ArrowDown': next = step(1); break;
			case 'ArrowUp': next = step(-1); break;
			case 'Home': next = enabled[0]; break;
			case 'End': next = enabled[enabled.length - 1]; break;
			default: return;
		}
		event.preventDefault();
		root?.querySelector<HTMLButtonElement>(`[data-index="${options.indexOf(next)}"]`)?.focus();
		select(next);
	}
</script>

<div bind:this={root} class="seg" class:fill role={mode === 'tablist' ? 'tablist' : 'group'} aria-label={label}>
	{#each options as option, index (index)}
		{@const selected = option.value === value}
		<button
			type="button"
			id={option.id}
			class="option"
			class:selected
			data-index={index}
			role={mode === 'tablist' ? 'tab' : undefined}
			aria-pressed={mode === 'pressed' ? selected : undefined}
			aria-selected={mode === 'tablist' ? selected : undefined}
			aria-controls={mode === 'tablist' ? option.controls : undefined}
			tabindex={option.value === stop ? 0 : -1}
			disabled={option.disabled}
			onclick={() => select(option)}
			onkeydown={event => onkeydown(event, option)}
		>{option.label}{#if option.count !== undefined}{' '}<span class="count">{option.count}</span>{/if}</button>
	{/each}
</div>

<style>
	.seg {
		display: inline-flex;
		gap: 2px;
		padding: 2px;
		max-width: 100%;
		background: var(--bg-tertiary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
	}

	.seg.fill {
		display: flex;
	}

	.option {
		display: inline-flex;
		align-items: center;
		justify-content: center;
		gap: 6px;
		min-height: 28px;
		padding: 3px 12px;
		border: 1px solid transparent;
		border-radius: 6px;
		background: transparent;
		color: var(--text-secondary);
		font-family: inherit;
		font-size: 13px;
		font-weight: 600;
		line-height: 1.5;
		white-space: nowrap;
		cursor: pointer;
		transition: background 0.12s, color 0.12s, border-color 0.12s;
	}

	.fill .option {
		flex: 1;
		padding-inline: 6px;
	}

	.option:hover:not(:disabled):not(.selected) {
		background: var(--bg-hover);
		color: var(--text-primary);
	}

	/* The selected segment is a raised tile with a strong edge, so it reads in
	   both themes without relying on colour alone. */
	.option.selected {
		background: var(--bg-raised);
		border-color: var(--border-strong);
		color: var(--text-primary);
		box-shadow: var(--shadow-sm);
	}

	.option:disabled {
		opacity: 0.5;
		cursor: default;
	}

	.option:focus-visible {
		outline: 2px solid var(--accent-primary);
		outline-offset: 1px;
	}

	.count {
		font-size: 11px;
		color: var(--text-muted);
		font-variant-numeric: tabular-nums;
	}

	@media (pointer: coarse) {
		.option {
			min-height: 36px;
		}
	}

	@media (prefers-reduced-motion: reduce) {
		.option {
			transition: none;
		}
	}
</style>
