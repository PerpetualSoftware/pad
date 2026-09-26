<script lang="ts">
	// BUG-3050 U1: a body write met edits a tab has not stored yet. The user
	// chooses; nothing is overwritten by default (Escape and the close button
	// KEEP the tab's edits).
	import { pendingEditsDialog } from '$lib/stores/pendingEditsDialog.svelte';
	import Modal from '$lib/components/common/Modal.svelte';
	import Button from '$lib/components/common/Button.svelte';

	let active = $derived(pendingEditsDialog.active);
</script>

<Modal open={!!active} onclose={() => pendingEditsDialog.keep()} labelledby="pending-edits-title" maxWidth="500px">
	{#if active}
		<div class="modal-header">
			<h2 id="pending-edits-title">Unsaved edits in an open tab</h2>
			<button class="close-btn" type="button" onclick={() => pendingEditsDialog.keep()} aria-label="Close, keeping the tab's edits">&#10005;</button>
		</div>
		<div class="modal-body">
			{#if active.kind === 'duplicate'}
				<p>
					<strong>{active.itemRef}</strong> has edits in an open tab that are not stored yet. A copy
					made now holds the stored body, without them. Open the item so its edits save, then
					duplicate it.
				</p>
			{:else}
				<p>
					<strong>{active.itemRef}</strong> has edits in an open tab that are not stored yet, so the
					body you loaded is behind them. Saving your body would replace those edits.
				</p>
			{/if}
		</div>
		<div class="modal-footer">
			<Button variant="secondary" onclick={() => pendingEditsDialog.keep()}>
				{active.kind === 'duplicate' ? 'Cancel' : "Keep the tab's edits"}
			</Button>
			<Button variant="primary" onclick={() => pendingEditsDialog.overwrite()}>
				{active.kind === 'duplicate' ? 'Copy the stored body' : 'Overwrite them'}
			</Button>
		</div>
	{/if}
</Modal>

<style>
	.modal-header {
		display: flex;
		align-items: center;
		justify-content: space-between;
		padding: var(--space-4) var(--space-4) var(--space-2);
	}
	.modal-header h2 {
		margin: 0;
		font-size: 1.05em;
	}
	.close-btn {
		background: none;
		border: none;
		color: var(--text-muted);
		cursor: pointer;
	}
	.modal-body {
		padding: 0 var(--space-4);
		font-size: 0.92em;
		color: var(--text-secondary);
	}
	.modal-footer {
		display: flex;
		justify-content: flex-end;
		gap: var(--space-2);
		padding: var(--space-4);
	}
</style>
