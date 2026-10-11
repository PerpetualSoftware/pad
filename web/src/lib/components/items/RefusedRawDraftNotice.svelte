<script lang="ts">
	// Markdown a save could not store is offered back on the next open of the
	// item (BUG-3230 U0; the text is kept by $lib/items/refusedRawDraft).
	import Button from '$lib/components/common/Button.svelte';

	interface Props {
		savedAt: number;
		busy?: boolean;
		onrestore: () => void;
		oncopy: () => void;
		ondismiss: () => void;
		/**
		 * Someone saved this item after the kept text was written (TASK-3606):
		 * Restore would replace their newer version, so Copy leads.
		 */
		newer?: boolean;
	}

	let { savedAt, busy = false, onrestore, oncopy, ondismiss, newer = false }: Props = $props();

	let when = $derived(new Date(savedAt).toLocaleString());
</script>

<div class="refused-raw-draft" role="status" data-testid="refused-raw-draft">
	<!-- Said once for every producer: an unload flush another tab's edits
	     refused, a session that ended under the item, a tab that died before
	     the server confirmed (TASK-3606). -->
	<p>
		Changes you made here at <time datetime={new Date(savedAt).toISOString()}>{when}</time> did not reach the
		server. They are kept in this browser.
	</p>
	{#if newer}
		<p class="newer">This item changed after these edits were made; restoring replaces the newer version.</p>
	{/if}
	<div class="actions">
		{#if newer}
			<Button variant="primary" size="sm" disabled={busy} onclick={oncopy}>Copy it</Button>
			<Button variant="secondary" size="sm" disabled={busy} onclick={onrestore}>Restore it</Button>
		{:else}
			<Button variant="primary" size="sm" disabled={busy} onclick={onrestore}>Restore it</Button>
			<Button variant="secondary" size="sm" disabled={busy} onclick={oncopy}>Copy it</Button>
		{/if}
		<Button variant="ghost" size="sm" disabled={busy} onclick={ondismiss}>Discard</Button>
	</div>
</div>

<style>
	.refused-raw-draft {
		margin: 0 0 var(--space-3);
		padding: var(--space-3);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		background: var(--bg-secondary);
		font-size: 0.88em;
		color: var(--text-secondary);
	}
	.refused-raw-draft p {
		margin: 0 0 var(--space-2);
	}
	.newer {
		color: var(--text-primary);
		font-weight: 600;
	}
	.actions {
		display: flex;
		flex-wrap: wrap;
		gap: var(--space-2);
	}
</style>
