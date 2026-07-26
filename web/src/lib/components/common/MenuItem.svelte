<script lang="ts">
	import type { Snippet } from 'svelte';

	interface Props {
		/** Leading icon/emoji. */
		icon?: string;
		/** Right-aligned hint (shortcut, count). */
		hint?: string;
		/** Red row for destructive actions. */
		danger?: boolean;
		/** When defined the row is a menuitemradio with a trailing check. */
		checked?: boolean;
		disabled?: boolean;
		/** id of an element describing this row — e.g. the prompt line above a
		 *  destructive confirmation, which is presentational and otherwise
		 *  never announced when the row takes focus (PLAN-2326). */
		describedBy?: string;
		/** Extra class on the row, for callers that need to address it from
		 *  their own stylesheet — e.g. a container query that shows a row only
		 *  at widths where the equivalent toolbar button is folded away
		 *  (ItemDetail's Star row, TASK-2329). Reach it with `:global()`: the
		 *  caller's scoping hash is not applied to this element. */
		class?: string;
		onclick?: (e: MouseEvent) => void;
		children: Snippet;
	}

	let {
		icon,
		hint,
		danger = false,
		checked,
		disabled = false,
		describedBy,
		class: extraClass,
		onclick,
		children
	}: Props = $props();
</script>

<button
	type="button"
	class="mi {extraClass ?? ''}"
	class:danger
	role={checked !== undefined ? 'menuitemradio' : 'menuitem'}
	aria-checked={checked}
	aria-describedby={describedBy}
	{disabled}
	{onclick}
>
	{#if icon}<span class="mi-icon" aria-hidden="true">{icon}</span>{/if}
	<span class="mi-label">{@render children()}</span>
	{#if hint}<span class="mi-hint">{hint}</span>{/if}
	{#if checked}<span class="mi-check" aria-hidden="true">✓</span>{/if}
</button>

<style>
	.mi {
		display: flex;
		align-items: center;
		gap: 9px;
		width: 100%;
		padding: 7px 9px;
		border: none;
		border-radius: var(--radius-sm);
		background: none;
		color: var(--text-primary);
		font: inherit;
		font-size: 13px;
		text-align: left;
		cursor: pointer;
	}

	.mi:hover:not(:disabled),
	.mi:focus-visible {
		background: var(--bg-hover);
	}

	.mi:focus-visible {
		outline: none;
	}

	.mi:disabled {
		opacity: 0.5;
		cursor: default;
	}

	.mi-icon {
		width: 18px;
		text-align: center;
		flex: 0 0 18px;
		opacity: 0.85;
	}

	.mi-label {
		flex: 1;
		min-width: 0;
		white-space: nowrap;
		overflow: hidden;
		text-overflow: ellipsis;
	}

	.mi-hint {
		color: var(--text-muted);
		font-size: 11.5px;
		flex: 0 0 auto;
	}

	.mi-check {
		color: var(--accent-primary-soft, var(--accent-blue));
		flex: 0 0 auto;
	}

	.mi.danger {
		color: var(--accent-red);
	}

	.mi.danger:hover:not(:disabled) {
		background: color-mix(in srgb, var(--accent-red) 12%, transparent);
	}

	@media (pointer: coarse) {
		.mi {
			padding: 11px 10px;
		}
	}
</style>
