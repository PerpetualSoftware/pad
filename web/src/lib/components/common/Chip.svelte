<script lang="ts">
	import type { Snippet } from 'svelte';

	interface Props {
		/** CSS color (usually a `var(--...)` reference) driving text + tint. */
		color?: string;
		/** Render a leading dot in the chip color. */
		dot?: boolean;
		/** `sm` for dense rows (cards, table cells); `md` elsewhere. */
		size?: 'sm' | 'md';
		/** When provided the chip renders as a <button>. */
		onclick?: (e: MouseEvent) => void;
		title?: string;
		/** Brief scale pulse — used by status click-cycling. */
		pulse?: boolean;
		children: Snippet;
	}

	let {
		color = 'var(--accent-gray)',
		dot = false,
		size = 'md',
		onclick,
		title,
		pulse = false,
		children
	}: Props = $props();
</script>

{#if onclick}
	<button
		type="button"
		class="chip {size}"
		class:pulse
		style:--chip-c={color}
		{title}
		{onclick}
	>
		{#if dot}<span class="dot" aria-hidden="true"></span>{/if}
		{@render children()}
	</button>
{:else}
	<span class="chip {size}" class:pulse style:--chip-c={color} {title}>
		{#if dot}<span class="dot" aria-hidden="true"></span>{/if}
		{@render children()}
	</span>
{/if}

<style>
	.chip {
		display: inline-flex;
		align-items: center;
		gap: 5px;
		border: none;
		border-radius: 6px;
		font-weight: 500;
		line-height: 1.5;
		white-space: nowrap;
		background: color-mix(in srgb, var(--chip-c) var(--chip-alpha, 16%), transparent);
		color: var(--chip-c);
	}

	.chip.md {
		padding: 2px 8px;
		font-size: 0.85em;
	}

	.chip.sm {
		padding: 1px 6px;
		font-size: 0.8em;
	}

	button.chip {
		cursor: pointer;
		font-family: inherit;
	}

	button.chip:hover {
		filter: brightness(1.15);
	}

	.dot {
		width: 7px;
		height: 7px;
		border-radius: 50%;
		background: currentColor;
		flex: 0 0 7px;
	}

	.pulse {
		animation: chip-pulse 0.3s ease;
	}

	@keyframes chip-pulse {
		0% { transform: scale(1); }
		50% { transform: scale(1.12); }
		100% { transform: scale(1); }
	}

	@media (prefers-reduced-motion: reduce) {
		.pulse {
			animation: none;
		}
	}
</style>
