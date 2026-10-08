<script lang="ts">
	// TASK-3462 U3b: the item pane's half of the built-in update nudge. For an
	// item made from one of the conventions or playbooks Pad ships, it reads the
	// item's state, and when Pad's library has newer text it shows a badge that
	// opens a review dialog. Nothing updates on its own (Dave's ruling): the
	// update happens only when a person presses Accept.
	import { onDestroy, untrack } from 'svelte';
	import { api } from '$lib/api/client';
	import { authStore } from '$lib/stores/auth.svelte';
	import Modal from '$lib/components/common/Modal.svelte';
	import DiffView from '$lib/components/versions/DiffView.svelte';
	import type { BuiltinStateResponse } from '$lib/types';
	import { builtinFieldChanges } from '$lib/library/builtinOffers';

	interface Props {
		wsSlug: string;
		/** The item's ref (or slug), as the item routes take it. */
		itemRef: string;
		/** The item's seq: a new value means the item changed, so the state is read again. */
		seq: number;
		/** The item's current body and fields blob, for the comparison. */
		currentContent: string;
		currentFields: string;
		canEdit: boolean;
		/**
		 * Whether the pane holds edits the server cannot see yet: a pending raw
		 * draft, a save in flight, or a collab editor that is not synced (its
		 * typing is buffered or dropped, not in the op-log). Accept refuses
		 * while it does (codex r1, r2). A synced collab editor's typing IS in
		 * the op-log, where the server's content_pending_flush refusal covers
		 * it. Asked at the press, synchronously.
		 */
		hasUnsavedEdits?: () => boolean;
		/** The item's id: what a press is about, which a rename does not change. */
		itemId: string;
		/**
		 * Called after an accepted update. The pane decides what to reload: the
		 * collab editor receives the new body through the room and its own
		 * refresh, so it must NOT reload there (a reload cancels the editor's
		 * flush); the raw editor keeps its body on an SSE refresh, so it must.
		 */
		onAccepted?: () => void;
		/**
		 * Save the open collab editor's text before the update and say what
		 * happened (codex r4). A synced socket proves nothing about typing the
		 * server has stored: a frame in flight is not in the op-log yet, so the
		 * server's content_pending_flush refusal cannot see it. 'flushed' means
		 * the editor held text the preview did not show: the update is not
		 * sent, and the preview is read again. Absent outside collab mode.
		 */
		flushEdits?: () => Promise<'flushed' | 'deduped' | 'failed' | 'skipped'>;
	}

	let {
		wsSlug,
		itemId,
		itemRef,
		seq,
		currentContent,
		currentFields,
		canEdit,
		hasUnsavedEdits,
		onAccepted,
		flushEdits
	}: Props = $props();

	let offer = $state<BuiltinStateResponse | null>(null);
	let open = $state(false);
	let busy = $state(false);
	let errorMessage = $state<string | null>(null);
	// Set when the server refused because an open editor holds unflushed
	// edits: the next Accept may then discard them, and says so.
	let pendingEdits = $state(false);

	// Bumped by every read, so only the newest one commits.
	let loadGen = 0;

	async function load(ws: string, ref: string) {
		const isSameIdentity = authStore.identityFence();
		const myLoad = ++loadGen;
		try {
			const st = await api.builtins.get(ws, ref);
			if (!isSameIdentity() || myLoad !== loadGen) return;
			offer = st.state === 'update_available' || st.state === 'diverged' || st.state === 'unknown_origin' ? st : null;
			// A preview with nothing behind it would be an empty dialog.
			if (!offer) open = false;
		} catch {
			// 404 not_builtin (an item made from no built-in), a server before
			// TASK-3462, or a failed read: no offer.
			if (!isSameIdentity() || myLoad !== loadGen) return;
			offer = null;
			open = false;
		}
	}

	$effect(() => {
		const ws = wsSlug;
		const ref = itemRef;
		void seq; // re-read when the item changes
		untrack(() => load(ws, ref));
	});

	// A new identity sees nothing of the previous one's dialog.
	const stopIdentityWatch = authStore.onIdentityChange(() => {
		offer = null;
		open = false;
		busy = false;
		errorMessage = null;
		pendingEdits = false;
		void load(wsSlug, itemRef);
	});
	onDestroy(stopIdentityWatch);

	function parseFields(blob: string): Record<string, unknown> {
		try {
			const v = JSON.parse(blob || '{}');
			return v && typeof v === 'object' && !Array.isArray(v) ? (v as Record<string, unknown>) : {};
		} catch {
			return {};
		}
	}

	let fieldChanges = $derived(
		offer?.library ? builtinFieldChanges(parseFields(currentFields), offer.library.fields, offer.seed?.fields) : []
	);

	let badgeLabel = $derived(
		offer?.state === 'update_available'
			? 'Update available'
			: offer?.state === 'diverged'
				? 'Library changed'
				: offer?.state === 'unknown_origin'
					? 'Library version differs'
					: ''
	);

	function showValue(v: unknown): string {
		if (v === undefined) return '(none)';
		return typeof v === 'string' ? v : JSON.stringify(v);
	}

	function openDialog() {
		errorMessage = null;
		pendingEdits = false;
		open = true;
	}

	async function accept() {
		if (!offer || busy || !canEdit) return;
		if (hasUnsavedEdits?.()) {
			errorMessage =
				'This item has edits that have not reached the server yet. Wait until they are saved (and the editor is connected), then accept.';
			return;
		}
		const isSameIdentity = authStore.identityFence();
		// The item this press is about: a response that settles after the pane
		// moved to another item commits nothing here (codex r1).
		const ws = wsSlug;
		const ref = itemRef;
		const id = itemId;
		// By id, not ref: a rename during the request is the same item (codex r2).
		const stillThisItem = () => isSameIdentity() && itemId === id;
		const discard = pendingEdits;
		busy = true;
		errorMessage = null;
		try {
			const flushed = flushEdits ? await flushEdits() : 'deduped';
			if (!stillThisItem()) return;
			if (flushed === 'failed' || flushed === 'skipped') {
				errorMessage = 'Your latest edits could not be saved, so nothing was replaced. Try again in a moment.';
				return;
			}
			if (flushed === 'flushed') {
				errorMessage =
					'Your latest edits were saved first, so the comparison has changed. Review it again before accepting.';
				void load(wsSlug, itemRef);
				return;
			}
			await api.builtins.update(ws, ref, {
				expected_seq: offer.seq,
				...(discard ? { overwrite_pending_edits: true } : {})
			});
			if (!stillThisItem()) return;
			open = false;
			offer = null;
			pendingEdits = false;
			onAccepted?.();
		} catch (err) {
			if (!stillThisItem()) return;
			const code = (err as { code?: string }).code;
			if (code === 'content_pending_flush') {
				pendingEdits = true;
				errorMessage =
					'This item has edits an open editor has not saved yet. Accepting again replaces them as well.';
			} else if (code === 'update_conflict') {
				errorMessage = 'The item changed since this preview opened. The preview has been refreshed; review it again.';
				// The LIVE ref: a rename, which is what usually moves seq here,
				// leaves the captured one pointing at nothing (codex r3).
				void load(wsSlug, itemRef);
			} else if (code === 'builtin_up_to_date') {
				open = false;
				offer = null;
			} else {
				errorMessage = (err as Error)?.message || 'The update failed.';
			}
		} finally {
			if (stillThisItem()) busy = false;
		}
	}
</script>

{#if offer}
	<button class="builtin-badge" type="button" onclick={openDialog} title="Review the newer text in Pad's library">
		{badgeLabel}
	</button>
{/if}

<Modal {open} onclose={() => (open = false)} labelledby="builtin-update-title" maxWidth="960px">
	{#if offer?.library}
		<div class="modal-header">
			<h2 id="builtin-update-title">{badgeLabel}</h2>
			<button class="close-btn" type="button" onclick={() => (open = false)} aria-label="Close">&#10005;</button>
		</div>
		<div class="modal-body">
			{#if offer.state === 'update_available'}
				<p class="lead">
					Pad's library has a newer version of this {offer.kind ?? 'item'}. Your copy is unedited, so accepting
					only brings in the library's changes.
				</p>
				<DiffView oldContent={currentContent} newContent={offer.library.content} oldLabel="Your copy" newLabel="Pad's library" />
			{:else if offer.state === 'diverged' && offer.seed}
				<p class="lead">
					Pad's library has a newer version of this {offer.kind ?? 'item'}, and your copy was edited too.
					Accepting replaces your copy with the library's text, your edits included.
				</p>
				<h3 class="section">What Pad's library changed</h3>
				<DiffView oldContent={offer.seed.content} newContent={offer.library.content} oldLabel="When added" newLabel="Pad's library" />
				<h3 class="section">What you changed</h3>
				<DiffView oldContent={offer.seed.content} newContent={currentContent} oldLabel="When added" newLabel="Your copy" />
			{:else}
				<p class="lead">
					This copy was added before Pad recorded which version it came from, so it may have been edited.
					Compare it with the library's text before deciding.
				</p>
				<DiffView oldContent={currentContent} newContent={offer.library.content} oldLabel="Your copy" newLabel="Pad's library" />
			{/if}

			{#if fieldChanges.length > 0}
				<h3 class="section">Settings accepting would replace</h3>
				<ul class="field-changes">
					{#each fieldChanges as change (change.key)}
						<li>
							<code>{change.key}</code>:
							<span class="old">{showValue(change.current)}</span>
							&rarr;
							<span class="new">{change.library === undefined ? '(removed)' : showValue(change.library)}</span>
						</li>
					{/each}
				</ul>
			{/if}

			<p class="note">
				{#if pendingEdits}
					The unsaved edits from the open editor are discarded and are not kept anywhere. The last saved text stays
					in this item's version history.
				{:else}
					Your current text stays in this item's version history.
				{/if}
				The settings above are not kept there; the update's history entry records the values it replaced. Status and
				title are never changed.
			</p>
			{#if errorMessage}
				<p class="error" role="alert">{errorMessage}</p>
			{/if}
		</div>
		<div class="modal-footer">
			<button type="button" class="btn" onclick={() => (open = false)}>Not now</button>
			{#if canEdit}
				<button type="button" class="btn primary" disabled={busy} onclick={accept}>
					{pendingEdits ? 'Discard unsaved edits and accept' : "Accept the library's version"}
				</button>
			{:else}
				<span class="readonly">Only someone who can edit this item can accept it.</span>
			{/if}
		</div>
	{/if}
</Modal>

<style>
	.builtin-badge {
		font-size: 0.75rem;
		font-weight: 600;
		color: var(--status-blue);
		background: transparent;
		border: 1px solid currentColor;
		border-radius: 999px;
		padding: 2px 8px;
		cursor: pointer;
	}
	.builtin-badge:hover {
		text-decoration: underline;
	}
	.modal-header {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: 12px;
		padding: 16px 20px 8px;
	}
	.modal-header h2 {
		margin: 0;
		font-size: 1.1rem;
	}
	.close-btn {
		background: none;
		border: none;
		cursor: pointer;
		color: var(--text-secondary);
		font-size: 1rem;
	}
	.modal-body {
		padding: 0 20px 12px;
		max-height: 70vh;
		overflow: auto;
	}
	.lead {
		margin: 4px 0 12px;
	}
	.section {
		font-size: 0.9rem;
		margin: 16px 0 6px;
	}
	.field-changes {
		margin: 0;
		padding-left: 18px;
		font-size: 0.85rem;
	}
	.field-changes .old {
		color: var(--text-secondary);
		text-decoration: line-through;
		overflow-wrap: anywhere;
	}
	.field-changes .new {
		overflow-wrap: anywhere;
	}
	.note {
		font-size: 0.8rem;
		color: var(--text-secondary);
		margin-top: 16px;
	}
	.error {
		color: var(--status-red, #c0392b);
		font-size: 0.85rem;
	}
	.modal-footer {
		display: flex;
		justify-content: flex-end;
		align-items: center;
		gap: 8px;
		padding: 12px 20px 16px;
		flex-wrap: wrap;
	}
	.btn {
		padding: 6px 12px;
		border-radius: 6px;
		border: 1px solid var(--border);
		background: var(--bg-secondary);
		color: var(--text-primary);
		cursor: pointer;
	}
	.btn.primary {
		background: var(--accent-blue, #2563eb);
		border-color: transparent;
		color: #fff;
	}
	.btn:disabled {
		opacity: 0.6;
		cursor: default;
	}
	.readonly {
		font-size: 0.8rem;
		color: var(--text-secondary);
	}
</style>
