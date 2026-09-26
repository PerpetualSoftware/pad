<script lang="ts">
	import type { Version, Item } from '$lib/types';
	import { api, isContentPendingFlushError } from '$lib/api/client';
	import { authStore } from '$lib/stores/auth.svelte';
	import DiffView from '$lib/components/versions/DiffView.svelte';
	import StaleBodyNotice from '$lib/components/common/StaleBodyNotice.svelte';
	import Chip from '$lib/components/common/Chip.svelte';
	import { relativeTime } from '$lib/utils/markdown';

	interface Props {
		version: Version;
		wsSlug: string;
		itemSlug: string;
		currentContent: string;
		/** The server marked `currentContent` as behind the live document
		 *  (BUG-3000 `content_state`); the diff says so (BUG-3050 U3). */
		currentContentStale?: boolean;
		onRestore?: (item: Item) => void;
		/**
		 * PLAN-2154 Phase 2 / D2 / R12 (TASK-2172): master-freeze. The restore
		 * button is otherwise ungated (server-authorized), so a peeking master
		 * must hide it to keep the freeze complete. Defaults false →
		 * byte-identical for existing callers.
		 */
		frozen?: boolean;
		/**
		 * BUG-2271: flush the initiating client's LIVE collab editor markdown into
		 * items.content BEFORE the restore POST runs. The restore's server-side
		 * undo-point ("Restored from…") is captured from the CURRENT persisted
		 * items.content inside the restore tx; a live collab editor may hold edits
		 * still sitting in the Y.Doc/op-log within the ~5s flush-debounce window
		 * (not yet PATCHed). Without this flush those edits aren't captured in the
		 * undo-point and are lost after the restore wipes the op-log + reseeds peers
		 * (BUG-2264). The collab server is a dumb relay with no Yjs decoder, so the
		 * ONLY place that can render the live doc to markdown is the initiating
		 * client — hence a client-side flush here. ItemDetail owns the flusher and
		 * threads this down through ItemTimeline; leave it unset (no-op) for
		 * non-collab / other callers, who are then byte-identical to before.
		 */
		flushBeforeRestore?: () => Promise<void>;
	}

	let { version, wsSlug, itemSlug, currentContent, currentContentStale = false, onRestore, frozen = false, flushBeforeRestore }: Props = $props();

	let expanded = $state(false);
	let confirming = $state(false);
	let restoring = $state(false);
	// BUG-3031: the server refused the restore because the item holds edits
	// another editor session has not saved. The confirm then asks again, naming
	// what the restore would discard, and a second yes sends the override.
	let pendingEdits = $state(false);

	// The timeline endpoint serves raw reverse-patch text for diff versions
	// (is_diff), so version.content is unreadable patch data, not real content.
	// Resolve it lazily the first time the card is expanded (BUG-1612). Non-diff
	// versions already carry full content, so displayContent falls straight through.
	let fetchedContent = $state<string | null>(null);
	let resolveError = $state(false);
	let resolving = $state(false);
	let displayContent = $derived(version.is_diff ? fetchedContent : version.content);

	async function ensureResolved() {
		if (!version.is_diff || fetchedContent !== null || resolving) return;
		// NAVIGATION fence (TASK-2112): capture the REQUEST identity — the item
		// and workspace this resolve is about — before the await. This card lives
		// in the timeline panel that ItemDetail reuses across a no-{#key} item
		// switch (its itemSlug/wsSlug props change under it), so a lazy
		// version-content resolve landing after a switch must not write into a
		// stale card. `resolving` is a local spinner flag, always cleared.
		const reqSlug = itemSlug;
		const reqWs = wsSlug;
		// IDENTITY fence (BUG-3095), a separate question from the one above and
		// not covered by it: the navigation fence compares slugs, which do not
		// move when the SIGNED-IN USER changes on the same workspace and item.
		// The GET is issued before any await here, so it cannot be mis-issued —
		// what this guards is the COMMIT below, which would otherwise paint one
		// identity's version text into a card the next identity is reading.
		const isSameIdentity = authStore.identityFence();
		resolving = true;
		resolveError = false;
		try {
			const full = await api.versions.get(reqWs, reqSlug, version.id);
			if (!isSameIdentity()) return;
			if (reqSlug !== itemSlug || reqWs !== wsSlug) return;
			fetchedContent = full.content;
		} catch {
			if (!isSameIdentity()) return;
			if (reqSlug !== itemSlug || reqWs !== wsSlug) return;
			resolveError = true;
		} finally {
			resolving = false;
		}
	}

	function toggle() {
		expanded = !expanded;
		if (!expanded) {
			confirming = false;
			pendingEdits = false;
		} else {
			ensureResolved();
		}
	}

	function startRestore() {
		confirming = true;
	}

	function cancelRestore() {
		confirming = false;
		pendingEdits = false;
	}

	async function confirmRestore(overwritePendingEdits: boolean) {
		// Master-freeze guard (TASK-2172): the restore UI is hidden while frozen,
		// but drop a straggler click so a peeking master never dispatches restore.
		if (frozen) return;
		// NAVIGATION fence (TASK-2112): capture the REQUEST identity — the item
		// and workspace — before the await, so a mid-flight item switch (rapid
		// j/k / row-click in the split pane) can't fire onRestore with A's
		// restored item into a parent now showing B — nor flip this card's
		// confirm state after it's been repurposed. `restoring` is a local
		// button flag, always cleared.
		const reqSlug = itemSlug;
		const reqWs = wsSlug;
		// IDENTITY fence (BUG-3095). This is the surface-8 member: the restore
		// POST below is issued AFTER `flushBeforeRestore`, a parent-provided
		// await of unbounded duration, so a sign-out or account swap during that
		// flush sends the restore on the NEXT user's cookie. The navigation
		// fence above cannot see that — an identity change leaves both slugs
		// alone — and the pre-existing slug check ran only AFTER the POST had
		// already gone out, which is too late for a write.
		const isSameIdentity = authStore.identityFence();
		restoring = true;
		try {
			// BUG-2271: flush the initiating client's live collab editor into
			// items.content FIRST, so the restore's undo-point (captured from
			// items.content server-side, inside the restore tx) reflects in-flight
			// edits not yet PATCHed. Best-effort: a flush failure must NOT block a
			// user-confirmed restore, so swallow and proceed. The flush is
			// self-routing (it PATCHes the item it was minted against), so no
			// cross-write is possible even if the pane switched mid-await; the
			// restore below re-fences on reqSlug/reqWs as before.
			try {
				await flushBeforeRestore?.();
			} catch {
				// Swallow — proceed with the restore regardless.
			}
			// BUG-3095: BETWEEN the await and the send. A failed flush is
			// swallowed above, so this is the only thing standing between an
			// identity change during the flush and a restore issued as the new
			// user. It must stay on this side of the POST: the slug check below
			// runs after it, which is the right place to refuse a stale UI
			// update and the wrong place to refuse a write.
			if (!isSameIdentity()) return;
			let updatedItem;
			try {
				updatedItem = overwritePendingEdits
					? await api.versions.restore(reqWs, reqSlug, version.id, { overwritePendingEdits: true })
					: await api.versions.restore(reqWs, reqSlug, version.id);
			} catch (err) {
				// BUG-3031: nothing was written. The flush above drained THIS tab's
				// editor, so the pending edits are another session's, and only the
				// user can decide to discard them. Same fences as the success path,
				// first: the question is about this item, asked of this user.
				if (!isSameIdentity()) return;
				if (isContentPendingFlushError(err)) {
					if (reqSlug !== itemSlug || reqWs !== wsSlug) return;
					pendingEdits = true;
					return;
				}
				throw err;
			}
			if (!isSameIdentity()) return;
			if (reqSlug !== itemSlug || reqWs !== wsSlug) return;
			confirming = false;
			pendingEdits = false;
			onRestore?.(updatedItem);
		} finally {
			restoring = false;
		}
	}

	function actorLabel(actor: string): string {
		return actor === 'agent' ? 'Agent' : 'User';
	}

	function sourceLabel(source: string): string {
		const labels: Record<string, string> = {
			cli: 'CLI',
			web: 'Web',
			skill: 'Skill',
			'collab-snapshot': 'Autosave'
		};
		return labels[source] ?? source;
	}
</script>

<div class="version-card" class:expanded>
	<button class="card-header" type="button" onclick={toggle}>
		<span class="icon">&#x1F4C4;</span>
		<div class="header-content">
			<span class="label">Content updated</span>
			{#if version.change_summary}
				<span class="change-summary">{version.change_summary}</span>
			{/if}
		</div>
		<div class="badges">
			<Chip
				size="sm"
				color={version.created_by === 'agent' ? 'var(--accent-purple)' : 'var(--status-blue)'}
			>
				{actorLabel(version.created_by)}
			</Chip>
			<Chip size="sm" color="var(--accent-green)">
				{sourceLabel(version.source)}
			</Chip>
		</div>
		<span class="timestamp" title={new Date(version.created_at).toLocaleString()}>
			{relativeTime(version.created_at)}
		</span>
		<span class="chevron" class:open={expanded}>&#x25B8;</span>
	</button>

	{#if expanded}
		<div class="card-body">
			<div class="diff-container">
				{#if resolving}
					<p class="diff-status">Loading version…</p>
				{:else if resolveError}
					<p class="diff-status">Couldn't load this version's content.</p>
				{:else if displayContent !== null}
					{#if currentContentStale}<StaleBodyNotice />{/if}
					<DiffView oldContent={displayContent} newContent={currentContent} />
				{/if}
			</div>

			<!-- Master-freeze (TASK-2172 / R12): a peeking master hides restore. -->
			{#if !frozen}
			<div class="restore-area">
				{#if confirming}
					<div class="confirm-prompt">
						{#if pendingEdits}
							<span class="confirm-text confirm-warning" role="alert">
								This item has unsaved edits from another tab or session. Restoring will discard them, and no version will keep them.
							</span>
						{:else}
							<span class="confirm-text">Restore to this version?</span>
						{/if}
						<div class="confirm-actions">
							<button
								class="btn-cancel"
								type="button"
								onclick={cancelRestore}
								disabled={restoring}
							>
								Cancel
							</button>
							<button
								class="btn-restore-confirm"
								type="button"
								onclick={() => confirmRestore(pendingEdits)}
								disabled={restoring}
							>
								{restoring ? 'Restoring...' : pendingEdits ? 'Discard edits and restore' : 'Confirm Restore'}
							</button>
						</div>
					</div>
				{:else}
					<button
						class="btn-restore"
						type="button"
						onclick={startRestore}
					>
						Restore this version
					</button>
				{/if}
			</div>
			{/if}
		</div>
	{/if}
</div>

<style>
	.version-card {
		border: 1px solid var(--border);
		border-radius: var(--radius);
		background: var(--bg-secondary);
		overflow: hidden;
	}

	.version-card.expanded {
		border-color: var(--accent-blue);
	}

	.card-header {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		width: 100%;
		padding: var(--space-2) var(--space-3);
		background: none;
		border: none;
		cursor: pointer;
		text-align: left;
		color: var(--text-primary);
		font: inherit;
	}

	.card-header:hover {
		background: var(--bg-tertiary);
	}

	.icon {
		flex-shrink: 0;
		font-size: 1em;
		line-height: 1;
	}

	.header-content {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		flex: 1;
		min-width: 0;
	}

	.label {
		font-size: 0.85em;
		font-weight: 500;
		white-space: nowrap;
	}

	.change-summary {
		font-size: 0.8em;
		color: var(--text-muted);
		white-space: nowrap;
		overflow: hidden;
		text-overflow: ellipsis;
	}

	.diff-status {
		margin: 0;
		padding: var(--space-2);
		font-size: 0.85em;
		color: var(--text-muted);
	}

	.badges {
		display: flex;
		gap: var(--space-1);
		flex-shrink: 0;
	}

	.timestamp {
		font-size: 0.8em;
		color: var(--text-muted);
		white-space: nowrap;
		flex-shrink: 0;
	}

	.chevron {
		font-size: 0.75em;
		color: var(--text-muted);
		transition: transform 0.15s ease;
		flex-shrink: 0;
	}

	.chevron.open {
		transform: rotate(90deg);
	}

	.card-body {
		border-top: 1px solid var(--border);
		padding: var(--space-3);
		background: var(--bg-tertiary);
	}

	.diff-container {
		margin-bottom: var(--space-3);
	}

	.restore-area {
		display: flex;
		justify-content: flex-end;
	}

	.btn-restore {
		padding: var(--space-1) var(--space-3);
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		color: var(--text-secondary);
		font-size: 0.8em;
		cursor: pointer;
	}

	.btn-restore:hover {
		background: color-mix(in srgb, var(--accent-blue) 10%, transparent);
		border-color: var(--accent-blue);
		color: var(--accent-blue);
	}

	.confirm-prompt {
		display: flex;
		align-items: center;
		gap: var(--space-3);
		padding: var(--space-2) var(--space-3);
		background: color-mix(in srgb, var(--accent-yellow, #eab308) 8%, transparent);
		border: 1px solid color-mix(in srgb, var(--accent-yellow, #eab308) 30%, transparent);
		border-radius: var(--radius);
		flex-wrap: wrap;
		width: 100%;
	}

	.confirm-text {
		font-size: 0.8em;
		color: var(--text-secondary);
		font-weight: 500;
	}

	.confirm-warning {
		color: var(--text-primary);
		flex-basis: 100%;
	}

	.confirm-actions {
		display: flex;
		gap: var(--space-2);
		margin-left: auto;
	}

	.btn-cancel {
		padding: var(--space-1) var(--space-3);
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		color: var(--text-secondary);
		font-size: 0.8em;
		cursor: pointer;
	}

	.btn-cancel:hover:not(:disabled) {
		background: var(--bg-primary);
		color: var(--text-primary);
	}

	.btn-restore-confirm {
		padding: var(--space-1) var(--space-3);
		background: var(--accent-blue);
		border: none;
		border-radius: var(--radius);
		color: #fff;
		font-size: 0.8em;
		font-weight: 500;
		cursor: pointer;
	}

	.btn-restore-confirm:hover:not(:disabled) {
		filter: brightness(1.1);
	}

	.btn-restore-confirm:disabled,
	.btn-cancel:disabled {
		opacity: 0.5;
		cursor: not-allowed;
	}
</style>
