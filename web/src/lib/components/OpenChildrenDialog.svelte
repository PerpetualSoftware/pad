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

	let active = $derived(openChildrenDialog.active);
	let cancelBtn: HTMLButtonElement | undefined = $state();
	let modalEl: HTMLDivElement | undefined = $state();
	let previouslyFocused: HTMLElement | null = null;

	// On open: remember whatever had focus so we can restore it on
	// close, then move focus to the safe action (Cancel) — keyboard
	// users land on a defined target and the destructive "Override"
	// action stays one Tab away. On close: restore the original focus
	// so the user lands back where they were (drag handle, status
	// dropdown, etc.).
	$effect(() => {
		if (active) {
			previouslyFocused = (document.activeElement as HTMLElement) ?? null;
			cancelBtn?.focus();
		} else if (previouslyFocused) {
			previouslyFocused.focus();
			previouslyFocused = null;
		}
	});

	function onCancel() {
		openChildrenDialog.cancel();
	}

	function onConfirm() {
		openChildrenDialog.confirm();
	}

	// Focus trap. While the dialog is open, Tab / Shift-Tab cycle
	// between the focusable elements WITHIN the modal — anything
	// outside is off-limits until the user cancels or confirms.
	// Cheap implementation: collect focusable descendants on each
	// Tab press (modal contents are small + static while open) and
	// wrap selection at the ends.
	function trapTab(e: KeyboardEvent) {
		if (!active || e.key !== 'Tab' || !modalEl) return;
		const nodes = modalEl.querySelectorAll<HTMLElement>(
			'a[href], button:not([disabled]), [tabindex]:not([tabindex="-1"]), input, select, textarea'
		);
		if (nodes.length === 0) return;
		const first = nodes[0];
		const last = nodes[nodes.length - 1];
		const current = document.activeElement as HTMLElement | null;
		if (e.shiftKey && current === first) {
			e.preventDefault();
			last.focus();
		} else if (!e.shiftKey && current === last) {
			e.preventDefault();
			first.focus();
		} else if (current && !modalEl.contains(current)) {
			// Focus escaped (e.g. via programmatic blur) — re-anchor.
			e.preventDefault();
			first.focus();
		}
	}

	function onKeydown(e: KeyboardEvent) {
		if (!active) return;
		if (e.key === 'Escape') {
			e.preventDefault();
			onCancel();
			return;
		}
		if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) {
			e.preventDefault();
			onConfirm();
			return;
		}
		trapTab(e);
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

{#if active}
	<!-- svelte-ignore a11y_click_events_have_key_events -->
	<!-- svelte-ignore a11y_no_static_element_interactions -->
	<div
		class="overlay"
		onclick={onCancel}
		role="dialog"
		aria-modal="true"
		aria-labelledby="open-children-title"
		tabindex="-1"
	>
		<div class="modal" bind:this={modalEl} onclick={(e) => e.stopPropagation()}>
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
				<button
					bind:this={cancelBtn}
					type="button"
					class="btn btn-secondary"
					onclick={onCancel}>Cancel</button
				>
				<button type="button" class="btn btn-danger" onclick={onConfirm}>
					Override and mark {active.details.attempted_value}
				</button>
			</div>
		</div>
	</div>
{/if}

<style>
	.overlay {
		position: fixed;
		inset: 0;
		background: rgba(0, 0, 0, 0.5);
		z-index: 60;
		display: flex;
		justify-content: center;
		align-items: flex-start;
		padding: 12vh var(--space-4) var(--space-4);
		animation: overlay-in 140ms ease-out;
	}

	.modal {
		width: 100%;
		max-width: 540px;
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius-lg);
		box-shadow: 0 20px 60px rgba(0, 0, 0, 0.5);
		display: flex;
		flex-direction: column;
		max-height: 75vh;
		animation: modal-in 160ms ease-out;
	}

	@keyframes overlay-in {
		from { opacity: 0; }
		to { opacity: 1; }
	}
	@keyframes modal-in {
		from { transform: translateY(8px); opacity: 0; }
		to { transform: translateY(0); opacity: 1; }
	}
	@media (prefers-reduced-motion: reduce) {
		.overlay, .modal { animation: none; }
	}

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
		.overlay {
			padding: var(--space-3);
			align-items: stretch;
		}
		.modal {
			max-width: 100%;
			max-height: calc(100vh - var(--space-6));
		}
		.modal-header,
		.modal-body,
		.modal-footer {
			padding-left: var(--space-4);
			padding-right: var(--space-4);
		}
	}
</style>
