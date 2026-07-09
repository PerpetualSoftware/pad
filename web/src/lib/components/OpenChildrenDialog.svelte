<script lang="ts">
	// BUG-1538 / TASK-1539 — Confirm dialog that surfaces the server's
	// `open_children` 409 guard (IDEA-1494) in the web UI. Mounted once
	// from +layout.svelte; driven by the openChildrenDialog singleton
	// store so call sites just `await openChildrenDialog.request(...)`
	// and don't render any dialog markup themselves.
	//
	// The modal mirrors EditCollectionModal's overlay+modal pattern
	// (.overlay backdrop / .modal box / Escape closes) so it looks like
	// the rest of the app's modals without pulling in a heavy dialog
	// abstraction. Child items render as inline links to their detail
	// pages — the whole point is helping the user see WHY the move was
	// blocked and either resolve the children or force-override.

	import { page } from '$app/state';
	import { openChildrenDialog } from '$lib/stores/openChildrenDialog.svelte';
	import Modal from '$lib/components/common/Modal.svelte';

	let active = $derived(openChildrenDialog.active);

	function onCancel() {
		openChildrenDialog.cancel();
	}

	function onConfirm() {
		openChildrenDialog.confirm();
	}

	// The native <dialog> (via <Modal>) owns Escape, the focus trap, and
	// focus save/restore. We keep a window listener only for the Cmd/Ctrl+Enter
	// shortcut that confirms the override — gated on `active` so it's inert
	// while the dialog is closed.
	function onKeydown(e: KeyboardEvent) {
		if (!active) return;
		if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) {
			e.preventDefault();
			onConfirm();
		}
	}

	// Item URLs are /[username]/[workspace]/[collection]/[slug]. The
	// dialog is global so we pull both segments from page.params rather
	// than threading them through. Matches the TableView / roles page
	// pattern (the canonical href shape elsewhere in the app).
	let username = $derived(page.params.username || '');
	let workspaceSlug = $derived(page.params.workspace || '');
	let canLink = $derived(!!username && !!workspaceSlug);

	function childHref(refOrSlug: string, collection: string): string {
		return `/${username}/${workspaceSlug}/${collection}/${refOrSlug}`;
	}
</script>

<svelte:window onkeydown={onKeydown} />

<Modal
	open={!!active}
	onclose={onCancel}
	labelledby="open-children-title"
	maxWidth="540px"
>
	{#if active}
		<div class="modal-header">
			<h2 id="open-children-title">Children still open</h2>
			<button class="close-btn" type="button" onclick={onCancel} aria-label="Cancel"
				>&#10005;</button
			>
		</div>

			<div class="modal-body">
				<p class="lead">
					Cannot mark <strong>{active.parentRef}</strong>
					<code>{active.details.attempted_value}</code> while child items are still in a
					non-terminal state.
				</p>

				{#if active.details.open_children.length > 0}
					<div class="child-section">
						<div class="section-label">
							{active.details.open_children.length} blocking
							{active.details.open_children.length === 1 ? 'child' : 'children'}:
						</div>
						<ul class="child-list">
							{#each active.details.open_children as child (child.ref)}
								<li class="child-row">
									{#if canLink}
										<a
											class="child-ref"
											href={childHref(child.ref, child.collection_slug)}
											target="_blank"
											rel="noopener"
										>
											{child.ref}
										</a>
									{:else}
										<span class="child-ref">{child.ref}</span>
									{/if}
									<span class="child-title">{child.title}</span>
									<span class="child-status">{child.status}</span>
								</li>
							{/each}
						</ul>
					</div>
				{/if}

				{#if active.details.hidden_blocker_count > 0}
					<div class="hidden-note">
						Plus <strong>{active.details.hidden_blocker_count}</strong> additional
						{active.details.hidden_blocker_count === 1 ? 'child' : 'children'} you don't have access
						to.
					</div>
				{/if}

				<div class="explainer">
					Close those items first, or override the guard to mark
					<strong>{active.parentRef}</strong> terminal anyway.
				</div>
			</div>

			<div class="modal-footer">
				<!-- svelte-ignore a11y_autofocus -->
				<button
					type="button"
					class="btn btn-secondary"
					autofocus
					onclick={onCancel}>Cancel</button
				>
				<button type="button" class="btn btn-danger" onclick={onConfirm}>
					Override and mark {active.details.attempted_value}
				</button>
			</div>
	{/if}
</Modal>

<style>
	.modal-header {
		display: flex;
		align-items: center;
		justify-content: space-between;
		padding: var(--space-4) var(--space-6);
		flex-shrink: 0;
		border-bottom: 1px solid var(--border);
	}

	.modal-header h2 {
		margin: 0;
		font-size: 1.05em;
		font-weight: 600;
	}

	.close-btn {
		background: none;
		border: 0;
		font-size: 1em;
		color: var(--text-secondary);
		cursor: pointer;
		padding: var(--space-1) var(--space-2);
		border-radius: var(--radius-sm);
	}
	.close-btn:hover {
		background: var(--bg-tertiary);
		color: var(--text-primary);
	}

	.modal-body {
		flex: 1;
		overflow-y: auto;
		padding: var(--space-5) var(--space-6);
		display: flex;
		flex-direction: column;
		gap: var(--space-4);
		min-height: 0;
	}

	.lead {
		margin: 0;
		color: var(--text-primary);
		line-height: 1.5;
	}

	.lead code {
		background: var(--bg-tertiary);
		padding: 0 var(--space-1);
		border-radius: var(--radius-sm);
		font-size: 0.9em;
	}

	.section-label {
		font-size: 0.85em;
		color: var(--text-secondary);
		margin-bottom: var(--space-2);
	}

	.child-list {
		list-style: none;
		margin: 0;
		padding: 0;
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
		border: 1px solid var(--border);
		border-radius: var(--radius-md);
		background: var(--bg-tertiary);
	}

	.child-row {
		display: flex;
		align-items: baseline;
		gap: var(--space-3);
		padding: var(--space-2) var(--space-3);
		border-bottom: 1px solid var(--border);
	}
	.child-row:last-child {
		border-bottom: 0;
	}

	.child-ref {
		font-family: var(--font-mono, ui-monospace, monospace);
		font-size: 0.85em;
		color: var(--accent, var(--text-primary));
		text-decoration: none;
		white-space: nowrap;
		font-weight: 600;
	}
	.child-ref:hover {
		text-decoration: underline;
	}

	.child-title {
		flex: 1;
		color: var(--text-primary);
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}

	.child-status {
		font-size: 0.8em;
		color: var(--text-secondary);
		background: var(--bg-secondary);
		padding: 2px var(--space-2);
		border-radius: var(--radius-sm);
		white-space: nowrap;
	}

	.hidden-note {
		font-size: 0.9em;
		color: var(--text-secondary);
		background: var(--bg-tertiary);
		padding: var(--space-2) var(--space-3);
		border-radius: var(--radius-md);
	}

	.explainer {
		font-size: 0.9em;
		color: var(--text-secondary);
		line-height: 1.5;
	}

	.modal-footer {
		display: flex;
		justify-content: flex-end;
		gap: var(--space-2);
		padding: var(--space-4) var(--space-6);
		border-top: 1px solid var(--border);
		flex-shrink: 0;
	}

	.btn {
		padding: var(--space-2) var(--space-4);
		border-radius: var(--radius-md);
		font-size: 0.9em;
		font-weight: 500;
		cursor: pointer;
		border: 1px solid transparent;
	}

	.btn-secondary {
		background: var(--bg-tertiary);
		color: var(--text-primary);
		border-color: var(--border);
	}
	.btn-secondary:hover {
		background: var(--bg-secondary);
	}

	.btn-danger {
		background: var(--color-danger, #dc2626);
		color: white;
	}
	.btn-danger:hover {
		filter: brightness(1.08);
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
