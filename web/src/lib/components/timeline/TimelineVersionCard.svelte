<script lang="ts">
	import type { Version, Item } from '$lib/types';
	import { api } from '$lib/api/client';
	import DiffView from '$lib/components/versions/DiffView.svelte';
	import { relativeTime } from '$lib/utils/markdown';

	interface Props {
		version: Version;
		wsSlug: string;
		itemSlug: string;
		currentContent: string;
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

	let { version, wsSlug, itemSlug, currentContent, onRestore, frozen = false, flushBeforeRestore }: Props = $props();

	let expanded = $state(false);
	let confirming = $state(false);
	let restoring = $state(false);

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
		// Capture the item identity before the await. This card lives in the
		// timeline panel that ItemDetail reuses across a no-{#key} item switch
		// (its itemSlug/wsSlug props change under it), so a lazy version-content
		// resolve landing after a switch must not write into a stale card
		// (TASK-2112). `resolving` is a local spinner flag, always cleared.
		const reqSlug = itemSlug;
		const reqWs = wsSlug;
		resolving = true;
		resolveError = false;
		try {
			const full = await api.versions.get(reqWs, reqSlug, version.id);
			if (reqSlug !== itemSlug || reqWs !== wsSlug) return;
			fetchedContent = full.content;
		} catch {
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
		} else {
			ensureResolved();
		}
	}

	function startRestore() {
		confirming = true;
	}

	function cancelRestore() {
		confirming = false;
	}

	async function confirmRestore() {
		// Master-freeze guard (TASK-2172): the restore UI is hidden while frozen,
		// but drop a straggler click so a peeking master never dispatches restore.
		if (frozen) return;
		// Capture the item identity before the await so a mid-flight item switch
		// (rapid j/k / row-click in the split pane) can't fire onRestore with
		// A's restored item into a parent now showing B — nor flip this card's
		// confirm state after it's been repurposed (TASK-2112). `restoring` is a
		// local button flag, always cleared.
		const reqSlug = itemSlug;
		const reqWs = wsSlug;
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
			const updatedItem = await api.versions.restore(reqWs, reqSlug, version.id);
			if (reqSlug !== itemSlug || reqWs !== wsSlug) return;
			confirming = false;
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
			<span
				class="badge"
				class:badge-agent={version.created_by === 'agent'}
				class:badge-user={version.created_by !== 'agent'}
			>
				{actorLabel(version.created_by)}
			</span>
			<span class="badge badge-source">
				{sourceLabel(version.source)}
			</span>
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
					<DiffView oldContent={displayContent} newContent={currentContent} />
				{/if}
			</div>

			<!-- Master-freeze (TASK-2172 / R12): a peeking master hides restore. -->
			{#if !frozen}
			<div class="restore-area">
				{#if confirming}
					<div class="confirm-prompt">
						<span class="confirm-text">Restore to this version?</span>
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
								onclick={confirmRestore}
								disabled={restoring}
							>
								{restoring ? 'Restoring...' : 'Confirm Restore'}
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

	.badge {
		display: inline-flex;
		align-items: center;
		padding: 1px var(--space-2);
		border-radius: 9999px;
		font-size: 0.7em;
		font-weight: 500;
		text-transform: uppercase;
		letter-spacing: 0.03em;
		line-height: 1.6;
	}

	.badge-agent {
		background: color-mix(in srgb, var(--accent-purple, #a855f7) 15%, transparent);
		color: var(--accent-purple, #a855f7);
	}

	.badge-user {
		background: color-mix(in srgb, var(--accent-blue) 15%, transparent);
		color: var(--accent-blue);
	}

	.badge-source {
		background: color-mix(in srgb, var(--accent-green) 15%, transparent);
		color: var(--accent-green);
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
