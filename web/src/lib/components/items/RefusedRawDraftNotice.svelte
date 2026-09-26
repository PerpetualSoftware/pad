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
	}

	let { savedAt, busy = false, onrestore, oncopy, ondismiss }: Props = $props();

	let when = $derived(new Date(savedAt).toLocaleString());
</script>

<div class="refused-raw-draft" role="status" data-testid="refused-raw-draft">
	<p>
		Markdown you typed here at <time datetime={new Date(savedAt).toISOString()}>{when}</time> was not saved,
		because another tab had edits to this item that were not stored yet. It is kept in this browser.
	</p>
	<div class="actions">
		<Button variant="primary" size="sm" disabled={busy} onclick={onrestore}>Restore it</Button>
		<Button variant="secondary" size="sm" disabled={busy} onclick={oncopy}>Copy it</Button>
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
	.actions {
		display: flex;
		flex-wrap: wrap;
		gap: var(--space-2);
	}
</style>
