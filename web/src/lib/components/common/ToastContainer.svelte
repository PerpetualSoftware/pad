<script lang="ts">
	import { fly, fade } from 'svelte/transition';
	import { toastStore } from '$lib/stores/toast.svelte';
	import type { Toast } from '$lib/stores/toast.svelte';

	function iconForType(type: Toast['type']): string {
		switch (type) {
			case 'success': return '\u2713';
			case 'error': return '\u2717';
			case 'info': return '\u2139';
		}
	}
</script>

{#if toastStore.toasts.length > 0}
	<div class="toast-container" role="status" aria-live="polite">
		{#each toastStore.toasts as toast (toast.id)}
			<div
				class="toast toast-{toast.type}"
				in:fly={{ x: 80, duration: 250 }}
				out:fade={{ duration: 150 }}
			>
				<span class="toast-icon">{iconForType(toast.type)}</span>
				<span class="toast-message">{toast.message}</span>
				<button
					class="toast-dismiss"
					onclick={() => toastStore.dismiss(toast.id)}
					aria-label="Dismiss notification"
				>&times;</button>
			</div>
		{/each}
	</div>
{/if}

<style>
	.toast-container {
		position: fixed;
		bottom: 20px;
		right: 20px;
		z-index: 100;
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
		pointer-events: none;
		max-width: 360px;
	}

	.toast {
		display: flex;
		align-items: center;
		gap: var(--space-3);
		padding: var(--space-3) var(--space-4);
		border-radius: var(--radius);
		border: 1px solid var(--border);
		background: var(--bg-secondary);
		color: var(--text-primary);
		font-size: 0.88em;
		box-shadow: 0 4px 12px rgba(0, 0, 0, 0.25);
		pointer-events: auto;
	}

	.toast-success {
		border-left: 3px solid var(--accent-green);
	}
	.toast-success .toast-icon {
		color: var(--accent-green);
	}

	.toast-error {
		border-left: 3px solid var(--accent-red, #ef4444);
	}
	.toast-error .toast-icon {
		color: var(--accent-red, #ef4444);
	}

	.toast-info {
		border-left: 3px solid var(--accent-blue);
	}
	.toast-info .toast-icon {
		color: var(--accent-blue);
	}

	.toast-icon {
		font-size: 1.1em;
		flex-shrink: 0;
		width: 18px;
		text-align: center;
		font-weight: 700;
	}

	.toast-message {
		flex: 1;
		min-width: 0;
		line-height: 1.4;
	}

	.toast-dismiss {
		flex-shrink: 0;
		padding: 0;
		width: 20px;
		height: 20px;
		display: flex;
		align-items: center;
		justify-content: center;
		border-radius: var(--radius-sm);
		color: var(--text-muted);
		font-size: 1.1em;
		line-height: 1;
		cursor: pointer;
		background: none;
		border: none;
	}
	.toast-dismiss:hover {
		background: var(--bg-hover);
		color: var(--text-primary);
	}

	@media (max-width: 768px) {
		.toast-container {
			bottom: 12px;
			right: 12px;
			left: 12px;
			max-width: none;
		}
	}
</style>
