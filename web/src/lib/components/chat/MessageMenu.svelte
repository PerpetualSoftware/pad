<script lang="ts">
	import { toastStore } from '$lib/stores/toast.svelte';
	import type { ChatMessage } from '$lib/types';

	// The actions on one confirmed message (TASK-4044 PR 3b): Reply, Copy, Delete.
	// The bubble decides when it is open (hover or focus on a pointer device, a
	// long-press on touch). Delete has no confirm step (ruling d87482de): the
	// tombstone that replaces the row is visible, and the server allows a human
	// to delete their agent's rows too.
	let {
		message,
		onReply,
		onDelete,
		onDone
	}: {
		message: ChatMessage;
		onReply: (m: ChatMessage) => void;
		onDelete: (m: ChatMessage) => void;
		onDone: () => void;
	} = $props();

	async function copy() {
		onDone();
		try {
			await navigator.clipboard.writeText(message.body);
			toastStore.show('Copied to clipboard', 'success');
		} catch {
			toastStore.show("Couldn't copy that message", 'error');
		}
	}
</script>

<div class="menu" role="group" aria-label="Message actions">
	<button
		type="button"
		onclick={() => {
			onDone();
			onReply(message);
		}}>Reply</button
	>
	<button type="button" onclick={() => void copy()}>Copy</button>
	<button
		type="button"
		class="danger"
		onclick={() => {
			onDone();
			onDelete(message);
		}}>Delete</button
	>
</div>

<style>
	.menu {
		display: flex;
		gap: var(--space-1);
		margin-top: var(--space-1);
	}
	button {
		padding: var(--space-1) var(--space-2);
		border: 1px solid var(--border);
		border-radius: var(--radius-md, 6px);
		background: var(--bg-primary);
		color: var(--text-primary);
		font: inherit;
		font-size: 0.8rem;
		cursor: pointer;
	}
	.danger {
		margin-left: var(--space-3);
		color: var(--accent-red, var(--text-primary));
	}
	/* A finger needs a larger target, and Delete has no confirm step. */
	@media (pointer: coarse) {
		.menu {
			gap: var(--space-2);
		}
		button {
			min-height: 44px;
			padding: var(--space-1) var(--space-3);
		}
		.danger {
			margin-left: var(--space-6);
		}
	}
</style>
