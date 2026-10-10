<script lang="ts">
	// The general confirmation dialog (TASK-2221, audit C39), driven by the
	// confirmDialog store and mounted once in the root layout, beside
	// OpenChildrenDialog and PendingEditsDialog. It replaces native confirm():
	// the native dialog blocks the page, cannot be styled, and is a different
	// pattern from every other confirmation in the app.
	import { confirmDialog } from '$lib/stores/confirmDialog.svelte';
	import Modal from '$lib/components/common/Modal.svelte';
	import Button from '$lib/components/common/Button.svelte';

	let active = $derived(confirmDialog.active);
</script>

<Modal open={!!active} onclose={() => confirmDialog.cancel()} labelledby="confirm-dialog-title" maxWidth="440px">
	{#if active}
		<div class="modal-header">
			<h2 id="confirm-dialog-title">{active.title}</h2>
		</div>
		<div class="modal-body">
			<p class="message">{active.message}</p>
		</div>
		<div class="modal-footer">
			<Button variant="secondary" autofocus onclick={() => confirmDialog.cancel()}>{active.cancelLabel ?? 'Cancel'}</Button>
			<Button variant={active.danger ? 'danger-solid' : 'primary'} onclick={() => confirmDialog.confirm()}>
				{active.confirmLabel}
			</Button>
		</div>
	{/if}
</Modal>

<style>
	.modal-header {
		display: flex;
		align-items: center;
		padding: var(--space-4) var(--space-6);
		border-bottom: 1px solid var(--border);
	}
	.modal-header h2 {
		margin: 0;
		font-size: 1.05em;
		font-weight: 600;
	}
	.modal-body {
		padding: var(--space-5) var(--space-6);
	}
	.message {
		margin: 0;
		white-space: pre-line;
		color: var(--text-secondary);
	}
	.modal-footer {
		display: flex;
		justify-content: flex-end;
		gap: var(--space-2);
		padding: var(--space-4) var(--space-6);
		border-top: 1px solid var(--border);
	}
	@media (max-width: 600px) {
		.modal-header,
		.modal-body,
		.modal-footer {
			padding-left: var(--space-4);
			padding-right: var(--space-4);
		}
	}
</style>
