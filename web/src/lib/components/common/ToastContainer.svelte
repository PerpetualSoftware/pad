<script lang="ts">
	import { fly, fade } from 'svelte/transition';
	import { goto } from '$app/navigation';
	import { toastStore } from '$lib/stores/toast.svelte';
	import type { Toast } from '$lib/stores/toast.svelte';

	function iconForType(type: Toast['type']): string {
		switch (type) {
			case 'success': return '\u2713';
			case 'error': return '\u2717';
			case 'info': return '\u2139';
		}
	}

	function handleClick(toast: Toast) {
		if (toast.link) {
			toastStore.dismiss(toast.id);
			goto(toast.link);
		}
	}

	// ANNOUNCEMENT (TASK-2202). The stacks below ARE the live regions, and
	// both are mounted for the page's whole life, empty until a toast arrives:
	// screen readers announce a node added to a region that already exists,
	// and miss one that arrives together with its region. The single stack
	// used to be wrapped in {#if toasts.length}, so a toast after an idle
	// spell mounted with its region and was usually never read.
	//
	// Errors go in an assertive region (they interrupt), the rest in a polite
	// one. aria-atomic is left false, so only the added toast is read, not the
	// whole stack. Each toast is its own keyed node, so the same text twice is
	// still a new node and is read again. The text lives in the toast itself,
	// once: no hidden copy for a test or a screen reader to find twice.
	const errors = $derived(toastStore.toasts.filter((t) => t.type === 'error'));
	const others = $derived(toastStore.toasts.filter((t) => t.type !== 'error'));

	// HOLD (TASK-2202): a toast stays while the pointer is over it or focus is
	// inside it. The two are separate holds, so leaving with the mouse while a
	// button inside still has focus keeps it up.
	function holdOnFocusOut(e: FocusEvent, id: string) {
		const next = e.relatedTarget as Node | null;
		if (next && (e.currentTarget as HTMLElement).contains(next)) return;
		toastStore.resume(id);
	}
	function holdOnFocusIn(e: FocusEvent, id: string) {
		const prev = e.relatedTarget as Node | null;
		if (prev && (e.currentTarget as HTMLElement).contains(prev)) return;
		toastStore.pause(id);
	}
</script>

{#snippet toastItem(toast: Toast)}
	<!-- svelte-ignore a11y_no_noninteractive_tabindex, a11y_no_static_element_interactions -->
	<div
		class="toast toast-{toast.type}"
		class:clickable={!!toast.link}
		data-toast-id={toast.id}
		in:fly={{ x: 80, duration: 250 }}
		out:fade={{ duration: 150 }}
		onclick={() => handleClick(toast)}
		onmouseenter={() => toastStore.pause(toast.id)}
		onmouseleave={() => toastStore.resume(toast.id)}
		onfocusin={(e) => holdOnFocusIn(e, toast.id)}
		onfocusout={(e) => holdOnFocusOut(e, toast.id)}
		role={toast.link ? 'button' : undefined}
		tabindex={toast.link ? 0 : -1}
		onkeydown={(e) => {
			if (!toast.link || e.target !== e.currentTarget) return;
			if (e.key === 'Enter' || e.key === ' ') {
				e.preventDefault();
				handleClick(toast);
			}
		}}
	>
		<span class="toast-icon">{iconForType(toast.type)}</span>
		<span class="toast-message">{toast.message}{#if toast.link}<span class="toast-link-hint"> →</span>{/if}</span>
		{#if toast.action}
			<button
				class="toast-action"
				onclick={(e) => { e.stopPropagation(); toast.action?.onAction(); toastStore.dismiss(toast.id); }}
			>{toast.action.label}</button>
		{/if}
		<button
			class="toast-dismiss"
			onclick={(e) => { e.stopPropagation(); toastStore.dismiss(toast.id); }}
			aria-label="Dismiss notification"
		>&times;</button>
	</div>
{/snippet}

<div class="toast-container">
	<div class="toast-stack" aria-live="assertive" data-toast-region="assertive">
		{#each errors as toast (toast.id)}{@render toastItem(toast)}{/each}
	</div>
	<div class="toast-stack" aria-live="polite" data-toast-region="polite">
		{#each others as toast (toast.id)}{@render toastItem(toast)}{/each}
	</div>
</div>

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

	.toast-stack {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
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

	.toast.clickable {
		cursor: pointer;
		transition: background 0.15s;
	}
	.toast.clickable:hover {
		background: var(--bg-hover);
	}
	.toast-link-hint {
		color: var(--accent-blue);
		font-weight: 600;
		margin-left: 2px;
	}

	.toast-success {
		border-left: 3px solid var(--accent-green);
	}
	.toast-success .toast-icon {
		color: var(--accent-green);
	}

	.toast-error {
		border-left: 3px solid var(--accent-red);
	}
	.toast-error .toast-icon {
		color: var(--accent-red);
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

	.toast-action {
		flex-shrink: 0;
		padding: 4px 10px;
		border-radius: var(--radius-sm);
		border: 1px solid var(--accent-blue);
		background: none;
		color: var(--accent-blue);
		font-size: 0.9em;
		font-weight: 600;
		cursor: pointer;
	}
	.toast-action:hover {
		background: color-mix(in srgb, var(--accent-blue) 12%, transparent);
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
