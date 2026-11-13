<script lang="ts">
	import type { Snippet } from 'svelte';
	import type { HTMLButtonAttributes } from 'svelte/elements';

	// An icon-only control (TASK-4515). The name is required: it is the aria-label and also
	// visually hidden text, so a lookup by role or by text finds the control. `href` makes it a
	// link. md is 32px (44px on a phone or a coarse pointer); sm is 28px and grows only with
	// `touch`. The glyph (children) is decoration.
	interface Props extends Omit<HTMLButtonAttributes, 'children'> {
		label: string;
		size?: 'sm' | 'md';
		touch?: boolean;
		href?: string;
		onclick?: (e: MouseEvent) => void;
		element?: HTMLElement;
		children: Snippet;
	}

	let {
		label,
		size = 'md',
		touch = size === 'md',
		href,
		element = $bindable(),
		class: className = '',
		type = 'button',
		onclick,
		children,
		...rest
	}: Props = $props();
</script>

{#if href}
	<a
		bind:this={element}
		{href}
		class="icon-btn {size} {className}"
		class:touch
		aria-label={label}
		{onclick}
	>
		<span class="glyph" aria-hidden="true">{@render children()}</span>
		<span class="sr-only">{label}</span>
	</a>
{:else}
	<button
		bind:this={element}
		{...rest}
		{type}
		class="icon-btn {size} {className}"
		class:touch
		aria-label={label}
		{onclick}
	>
		<span class="glyph" aria-hidden="true">{@render children()}</span>
		<span class="sr-only">{label}</span>
	</button>
{/if}

<style>
	.icon-btn {
		display: inline-flex;
		align-items: center;
		justify-content: center;
		flex-shrink: 0;
		padding: 0;
		border: 1px solid transparent;
		border-radius: var(--radius);
		background: none;
		color: var(--text-secondary);
		cursor: pointer;
		text-decoration: none;
		transition: background 0.12s, color 0.12s;
	}
	.icon-btn.md {
		width: 32px;
		height: 32px;
	}
	.icon-btn.sm {
		width: 28px;
		height: 28px;
	}
	.icon-btn:hover:not(:disabled) {
		background: var(--bg-hover);
		color: var(--text-primary);
		text-decoration: none;
	}
	.icon-btn:disabled {
		opacity: 0.5;
		cursor: default;
		pointer-events: none;
	}
	.icon-btn:focus-visible {
		outline: 2px solid color-mix(in srgb, var(--accent-primary) 65%, transparent);
		outline-offset: 2px;
	}
	.glyph {
		display: inline-flex;
	}
	.glyph :global(svg) {
		width: 18px;
		height: 18px;
	}
	.sm .glyph :global(svg) {
		width: 16px;
		height: 16px;
	}
	@media (max-width: 768px), (pointer: coarse) {
		.icon-btn.touch {
			width: 44px;
			height: 44px;
		}
		.icon-btn.touch .glyph :global(svg) {
			width: 20px;
			height: 20px;
		}
	}
</style>
