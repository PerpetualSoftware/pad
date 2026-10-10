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
			<h2 id="pending-edits-title">{active.kind === 'stale' ? 'This item changed since you opened the raw editor' : active.reason === 'set_aside' ? 'Edits from an earlier editor version' : 'Unsaved edits in an open tab'}</h2>
			<button class="close-btn" type="button" onclick={() => pendingEditsDialog.keep()} aria-label={active.kind === 'stale' ? 'Close, keeping your text unsaved' : "Close, keeping the tab's edits"}>&#10005;</button>
		</div>
		<div class="modal-body">
			{#if active.kind === 'stale'}
				<!-- BUG-3540: the stored body changed under this raw editor. No
				     default: closing keeps editing and sends nothing. -->
				<p>
					<strong>{active.itemRef}</strong> was changed by someone else after your markdown was loaded.
					Saving your text would replace their changes. Reload to see the stored text (your unsaved
					text is discarded), or overwrite it with yours.
				</p>
			{:else if active.reason === 'set_aside'}
				<!-- BUG-3244: these edits are in no body and opening the item will
				     not store them, so no branch here offers that. -->
				{#if active.kind === 'excerpt'}
					<p>
						This prompt includes an excerpt of <strong>{active.itemRef}</strong>'s body, which is
						missing edits an editor upgrade set aside. Opening the item will not restore them.
					</p>
				{:else if active.kind === 'duplicate'}
					<p>
						<strong>{active.itemRef}</strong> has edits from an earlier editor version that are not in
						its stored body, and opening it will not restore them. A copy holds the stored body,
						without them. The original keeps them.
					</p>
				{:else}
					<p>
						<strong>{active.itemRef}</strong> has edits from an earlier editor version that are not in
						its stored body, and opening it will not restore them. Saving your body would discard them.
					</p>
				{/if}
			{:else if active.kind === 'excerpt'}
				<p>
					This prompt includes an excerpt of <strong>{active.itemRef}</strong>'s body, and the stored
					body is behind edits in an open tab, so the excerpt may be out of date. Open the item so
					its edits save for a current one.
				</p>
			{:else if active.kind === 'duplicate'}
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
			{#if active.kind === 'stale'}
				<Button variant="secondary" onclick={() => pendingEditsDialog.reload()}>Reload the stored text</Button>
				<Button variant="danger" onclick={() => pendingEditsDialog.overwrite()}>Overwrite with my text</Button>
			{:else}
			<Button variant="secondary" onclick={() => pendingEditsDialog.keep()}>
				{active.kind === 'save' ? (active.reason === 'set_aside' ? 'Keep those edits' : "Keep the tab's edits") : 'Cancel'}
			</Button>
			<Button variant="primary" onclick={() => pendingEditsDialog.overwrite()}>
				{active.kind === 'duplicate' ? 'Copy the stored body' : active.kind === 'excerpt' ? 'Use it anyway' : active.reason === 'set_aside' ? 'Discard them' : 'Overwrite them'}
			</Button>
			{/if}
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
