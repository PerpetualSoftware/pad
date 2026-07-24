<script lang="ts">
	import type { Snippet } from 'svelte';
	import type { HTMLButtonAttributes } from 'svelte/elements';

	interface Props extends HTMLButtonAttributes {
		/** primary = filled violet (AA text via --accent-primary-strong);
		 *  secondary = raised neutral; ghost = borderless; danger = red tint. */
		variant?: 'primary' | 'secondary' | 'ghost' | 'danger';
		size?: 'sm' | 'md';
		children: Snippet;
	}

	let {
		variant = 'secondary',
		size = 'md',
		type = 'button',
		children,
		...rest
	}: Props = $props();
</script>

<button class="btn {variant} {size}" {type} {...rest}>
	{@render children()}
</button>

<style>
	.btn {
		display: inline-flex;
		align-items: center;
		justify-content: center;
		gap: 6px;
		border-radius: var(--radius);
		font-weight: 600;
		font-family: inherit;
		line-height: 1.5;
		white-space: nowrap;
		cursor: pointer;
		border: 1px solid transparent;
		transition: background 0.12s, border-color 0.12s, filter 0.12s;
	}

	.btn.md {
		padding: 6px 14px;
		font-size: 13px;
	}

	.btn.sm {
		padding: 3px 10px;
		font-size: 12px;
	}

	.btn:disabled {
		opacity: 0.5;
		cursor: default;
		pointer-events: none;
	}

	.btn:focus-visible {
		outline: 2px solid color-mix(in srgb, var(--accent-primary) 65%, transparent);
		outline-offset: 2px;
	}

	.primary {
		background: var(--accent-primary-strong, var(--accent-primary));
		color: var(--text-on-accent);
	}

	.primary:hover:not(:disabled) {
		filter: brightness(1.08);
	}

	.secondary {
		background: var(--bg-tertiary);
		border-color: var(--border);
		color: var(--text-primary);
	}

	.secondary:hover:not(:disabled) {
		border-color: var(--border-strong);
		background: var(--bg-hover);
	}

	.ghost {
		background: none;
		color: var(--text-secondary);
	}

	.ghost:hover:not(:disabled) {
		background: var(--bg-hover);
		color: var(--text-primary);
	}

	.danger {
		background: color-mix(in srgb, var(--accent-red) 12%, transparent);
		border-color: color-mix(in srgb, var(--accent-red) 40%, transparent);
		color: var(--accent-red);
	}

	.danger:hover:not(:disabled) {
		background: color-mix(in srgb, var(--accent-red) 20%, transparent);
		border-color: var(--accent-red);
	}
</style>
