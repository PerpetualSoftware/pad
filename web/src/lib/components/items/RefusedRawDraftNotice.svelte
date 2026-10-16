<script lang="ts">
	// Markdown a save could not store is offered back on the next open of the
	// item (BUG-3230 U0; the text is kept by $lib/items/refusedRawDraft).
	import Button from '$lib/components/common/Button.svelte';
	import DiffView from '$lib/components/versions/DiffView.svelte';

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
		/**
		 * Restore sends only the user's own changes as a patch (TASK-3607), so
		 * a newer version is kept, and Restore leads again.
		 */
		appliesChanges?: boolean;
		/**
		 * Those changes overlap edits made since and could not be applied: the
		 * diff from where the user started to their kept text, to re-apply by
		 * hand. Restore is not offered; Copy leads.
		 */
		conflict?: { base: string; draft: string } | null;
		/**
		 * Why Restore is not offered yet (BUG-3850): shown, with Restore
		 * disabled and Copy leading. Null when it can run.
		 */
		restoreBlocked?: string | null;
		/**
		 * The draft's base is not known to be where the user started (BUG-3850
		 * rulings baa63dc3, a6380212): a patch could not be told apart from the
		 * newer edits, so Restore is not offered and Copy leads (TASK-3856).
		 */
		baseStale?: boolean;
	}

	let {
		savedAt,
		busy = false,
		onrestore,
		oncopy,
		ondismiss,
		newer = false,
		appliesChanges = false,
		conflict = null,
		restoreBlocked = null,
		baseStale = false
	}: Props = $props();

	let cannotApply = $derived(conflict === null && newer && appliesChanges && baseStale);
	let copyLeads = $derived(conflict !== null || restoreBlocked !== null || (newer && !appliesChanges));

	let when = $derived(new Date(savedAt).toLocaleString());
</script>

<div class="refused-raw-draft" role="status" data-testid="refused-raw-draft">
	<!-- Said once for every producer: an unload flush another tab's edits
	     refused, a session that ended under the item, a tab that died before
	     the server confirmed (TASK-3606). -->
	<p>
		Changes you made here at <time datetime={new Date(savedAt).toISOString()}>{when}</time>
		{cannotApply ? 'may not all have reached' : 'did not reach'} the server. They are kept in this browser.
	</p>
	{#if conflict}
		<p class="newer" data-testid="refused-raw-draft-conflict">
			Your changes overlap edits made here since, so they could not be applied. Copy your text and re-apply the
			changes below by hand.
		</p>
		<div class="diff">
			<DiffView oldContent={conflict.base} newContent={conflict.draft} oldLabel="When you started" newLabel="Your kept text" />
		</div>
	{:else if cannotApply}
		<p class="newer" data-testid="refused-raw-draft-stale">
			This item changed after these edits were made, and your changes can't be told apart from the newer ones, so
			they can't be applied by themselves. Copy your text and re-apply them by hand.
		</p>
	{:else if newer && appliesChanges}
		<p class="newer">This item changed after these edits were made; restoring applies only your changes and keeps the newer version.</p>
	{:else if newer}
		<p class="newer">This item changed after these edits were made; restoring replaces the newer version.</p>
	{/if}
	{#if restoreBlocked && !conflict && !cannotApply}
		<p data-testid="refused-raw-draft-waiting">{restoreBlocked}</p>
	{/if}
	<div class="actions">
		{#if conflict || cannotApply}
			<Button variant="primary" size="sm" disabled={busy} onclick={oncopy}>Copy it</Button>
		{:else if copyLeads}
			<Button variant="primary" size="sm" disabled={busy} onclick={oncopy}>Copy it</Button>
			<Button variant="secondary" size="sm" disabled={busy || restoreBlocked !== null} onclick={onrestore}>Restore it</Button>
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
	.diff {
		max-height: 20rem;
		overflow: auto;
		margin: 0 0 var(--space-2);
	}
	.actions {
		display: flex;
		flex-wrap: wrap;
		gap: var(--space-2);
	}
</style>
