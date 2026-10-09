<script lang="ts">
	import type { Version, Item, AutosaveRun } from '$lib/types';
	import { api, isContentPendingFlushError } from '$lib/api/client';
	import { pendingEditsReason } from '$lib/items/contentWrite';
	import { authStore } from '$lib/stores/auth.svelte';
	import DiffView from '$lib/components/versions/DiffView.svelte';
	import StaleBodyNotice from '$lib/components/common/StaleBodyNotice.svelte';

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
		/**
		 * PLAN-2348 U3: the entry stands for a collapsed run of autosaves, and
		 * `version` is its NEWEST row. The diff then spans the whole run —
		 * before the oldest row's edit, after the newest's — and restore goes
		 * to the oldest row, the body the run started from.
		 */
		run?: AutosaveRun;
		/**
		 * PLAN-2348 U3: show the body's edits since `version` — the newest
		 * saved version — up to the current body. For an edit the version
		 * throttle wrote no row for (`body_edited`), this is the only diff that
		 * holds it. The current body is on one side, so the stale-body notice
		 * applies; there is no restore, because no row holds that state.
		 */
		sinceNow?: boolean;
	}

	let { version, wsSlug, itemSlug, currentContent, currentContentStale = false, onRestore, frozen = false, flushBeforeRestore, run, sinceNow = false }: Props = $props();

	/** The row a restore writes back: before the run, or before this edit. */
	const restoreId = $derived(run ? run.oldest_version_id : version.id);
	const linesAdded = $derived(run ? run.lines_added : version.lines_added);
	const linesRemoved = $derived(run ? run.lines_removed : version.lines_removed);
	const countsKnown = $derived(!sinceNow && linesAdded !== undefined && linesRemoved !== undefined);

	let expanded = $state(false);
	let confirming = $state(false);
	let restoring = $state(false);
	// BUG-3031: the server refused the restore because the item holds edits
	// another editor session has not saved. The confirm then asks again, naming
	// what the restore would discard, and a second yes sends the override.
	let pendingEdits = $state(false);
	// BUG-3244: the pending edits were set aside by an editor upgrade, not held by a tab.
	let pendingSetAside = $state(false);
	// TASK-2205: the server's message when a restore failed; cleared on retry or cancel.
	let restoreError = $state<string | null>(null);
	// Whose error, about which item: a card reused for another item, or seen by
	// the next signed-in account, shows nothing (codex r1, r2).
	let restoreErrorFor = $state('');

	// PLAN-2348 U3: a card shows ITS OWN edit — the body before the write that
	// made the row against the body after it — not the row against today's
	// body, which mixed every later edit into this one (checkpoint 2, defect
	// 2). The pair comes from the diff endpoint (U2), resolved lazily the
	// first time the card is expanded.
	let diffPair = $state<{ before: string; after: string } | null>(null);
	let resolveError = $state(false);
	let resolving = $state(false);

	async function ensureResolved() {
		if (diffPair !== null || resolving) return;
		// NAVIGATION fence (TASK-2112): capture the REQUEST identity — the item
		// and workspace this resolve is about — before the await. This card lives
		// in the timeline panel that ItemDetail reuses across a no-{#key} item
		// switch (its itemSlug/wsSlug props change under it), so a lazy
		// resolve landing after a switch must not write into a stale card.
		// `resolving` is a local spinner flag, always cleared.
		const reqSlug = itemSlug;
		const reqWs = wsSlug;
		// IDENTITY fence (BUG-3095), a separate question from the one above and
		// not covered by it: the navigation fence compares slugs, which do not
		// move when the SIGNED-IN USER changes on the same workspace and item.
		// The GETs are issued before any await here, so they cannot be
		// mis-issued — what this guards is the COMMIT below, which would
		// otherwise paint one identity's text into a card the next identity is
		// reading.
		const isSameIdentity = authStore.identityFence();
		const oldestId = run?.oldest_version_id;
		resolving = true;
		resolveError = false;
		try {
			const [newest, oldest] = await Promise.all([
				api.versions.diff(reqWs, reqSlug, version.id),
				oldestId && oldestId !== version.id
					? api.versions.diff(reqWs, reqSlug, oldestId)
					: Promise.resolve(null)
			]);
			if (!isSameIdentity()) return;
			if (reqSlug !== itemSlug || reqWs !== wsSlug) return;
			diffPair = sinceNow
				? { before: newest.after, after: currentContent }
				: { before: (oldest ?? newest).before, after: newest.after };
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
			pendingSetAside = false;
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
		pendingSetAside = false;
		restoreError = null;
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
		// The version asked for, captured with the item: the card can be reused
		// for another version while the flush below is awaited (codex r3).
		const reqVersion = restoreId;
		// IDENTITY fence (BUG-3095). This is the surface-8 member: the restore
		// POST below is issued AFTER `flushBeforeRestore`, a parent-provided
		// await of unbounded duration, so a sign-out or account swap during that
		// flush sends the restore on the NEXT user's cookie. The navigation
		// fence above cannot see that — an identity change leaves both slugs
		// alone — and the pre-existing slug check ran only AFTER the POST had
		// already gone out, which is too late for a write.
		const isSameIdentity = authStore.identityFence();
		restoring = true;
		restoreError = null;
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
					? await api.versions.restore(reqWs, reqSlug, reqVersion, { overwritePendingEdits: true })
					: await api.versions.restore(reqWs, reqSlug, reqVersion);
			} catch (err) {
				// BUG-3031: nothing was written. The flush above drained THIS tab's
				// editor, so the pending edits are another session's, and only the
				// user can decide to discard them. Same fences as the success path,
				// first: the question is about this item, asked of this user.
				if (!isSameIdentity()) return;
				if (reqSlug !== itemSlug || reqWs !== wsSlug) return;
				if (isContentPendingFlushError(err)) {
					pendingEdits = true;
					pendingSetAside = pendingEditsReason(err) === 'set_aside';
					return;
				}
				// TASK-2205 (audit C37): a failed restore says so, here, with the
				// server's message, as the timeline's other actions do. It used to
				// be rethrown from the click handler: no toast, no inline line, and
				// the card snapped back as if nothing had been asked.
				restoreError = err instanceof Error && err.message ? err.message : 'Restore failed';
				restoreErrorFor = authStore.identityEpoch + ':' + reqWs + '/' + reqSlug + '#' + reqVersion;
				return;
			}
			if (!isSameIdentity()) return;
			if (reqSlug !== itemSlug || reqWs !== wsSlug) return;
			confirming = false;
			pendingEdits = false;
			pendingSetAside = false;
			restoreError = null;
			onRestore?.(updatedItem);
		} finally {
			restoring = false;
		}
	}

	const restoreLabel = $derived(
		version.is_create ? 'Restore to as created' : run ? 'Restore to before these edits' : 'Restore to before this edit'
	);
	const oldLabel = $derived(
		sinceNow ? 'Last saved version' : version.is_create ? 'Empty' : run ? `Before these ${run.count} autosaves` : 'Before this edit'
	);
	const newLabel = $derived(sinceNow ? 'Now' : version.is_create ? 'As created' : 'After');
</script>

<div class="version-card" class:expanded data-diff={sinceNow ? 'current' : 'edit'}>
	<div class="summary-row">
		<span class="row-label">{sinceNow ? 'Since then' : 'Description'}</span>
		{#if countsKnown}
			<span class="lines"
				><span class="added">+{linesAdded}</span> <span class="removed">−{linesRemoved}</span> lines</span
			>
		{:else if sinceNow}
			<span class="lines unknown">every change since the last saved version, up to now</span>
		{:else}
			<span class="lines unknown">changed</span>
		{/if}
		<button class="show-changes" type="button" aria-expanded={expanded} onclick={toggle}>
			{expanded ? 'Hide changes' : 'Show changes'}<span class="chevron" class:open={expanded} aria-hidden="true">▾</span>
		</button>
	</div>

	{#if expanded}
		<div class="card-body">
			<div class="diff-container">
				{#if resolving}
					<p class="diff-status">Loading changes…</p>
				{:else if resolveError}
					<p class="diff-status">Couldn't load this edit's changes.</p>
				{:else if diffPair !== null}
					{#if currentContentStale && diffPair.after === currentContent}<StaleBodyNotice />{/if}
					<p class="pair-head">
						{sinceNow
							? 'Cumulative: everything that changed since the last saved version, not only this edit.'
							: version.is_create
								? 'The body as this item was created.'
								: 'This edit only, not a comparison with the current body.'}
					</p>
					<DiffView oldContent={diffPair.before} newContent={diffPair.after} {oldLabel} {newLabel} />
				{/if}
			</div>

			<!-- Master-freeze (TASK-2172 / R12): a peeking master hides restore.
			     A since-now diff has no restore: no version row holds its state. -->
			{#if !frozen && !sinceNow}
			<div class="restore-area">
				{#if confirming}
					<div class="confirm-prompt">
						{#if pendingEdits}
							<span class="confirm-text confirm-warning" role="alert">
								{pendingSetAside
									? 'This item has edits from an earlier editor version that are not in its saved body. Restoring will discard them, and no version will keep them.'
									: 'This item has unsaved edits from another tab or session. Restoring will discard them, and no version will keep them.'}
							</span>
						{:else}
							<span class="confirm-text">{restoreLabel}?</span>
						{/if}
						{#if restoreError && restoreErrorFor === authStore.identityEpoch + ':' + wsSlug + '/' + itemSlug + '#' + restoreId}
							<span class="confirm-text confirm-warning restore-error" role="alert">Restore failed: {restoreError}</span>
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
						{restoreLabel}
					</button>
				{/if}
			</div>
			{/if}
		</div>
	{/if}
</div>

<style>
	/* A section inside a History event card (PLAN-2348 U3), not a card of its
	   own: the event card owns the border. */
	.version-card {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
		min-width: 0;
	}

	.summary-row {
		display: flex;
		align-items: baseline;
		gap: var(--space-3);
		flex-wrap: wrap;
		font-size: 0.85em;
	}

	.row-label {
		color: var(--text-muted);
		min-width: 6.5em;
	}

	.lines {
		color: var(--text-muted);
	}

	.added {
		color: var(--accent-green);
		font-weight: 600;
	}

	.removed {
		color: var(--accent-red);
		font-weight: 600;
	}

	.show-changes {
		background: none;
		border: none;
		padding: 0;
		font: inherit;
		font-weight: 600;
		color: var(--accent-purple);
		cursor: pointer;
		display: inline-flex;
		align-items: center;
		gap: 0.25em;
	}

	.show-changes:hover {
		text-decoration: underline;
	}

	.pair-head {
		margin: 0 0 var(--space-1);
		font-size: 0.8em;
		color: var(--text-muted);
	}


	.diff-status {
		margin: 0;
		padding: var(--space-2);
		font-size: 0.85em;
		color: var(--text-muted);
	}



	.chevron {
		font-size: 0.85em;
		transition: transform 0.15s ease;
	}

	.chevron.open {
		transform: rotate(180deg);
	}

	.card-body {
		display: flex;
		flex-direction: column;
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
