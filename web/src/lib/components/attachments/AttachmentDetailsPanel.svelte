<!--
	AttachmentDetailsPanel — what a file IS, and what you can do with it
	(PLAN-2392 DR-2 / DR-6 / DR-10 / DR-13 / DR-18, TASK-2423).

	Tapping a file used to do the most destructive-adjacent thing available:
	a strip tile was a bare `<a download>`, so one tap put the file in your
	Downloads folder with no way to see what it was first. This panel is what
	a tap opens instead — the metadata, then the actions.

	PRESENTATION is the existing `Menu` with `sheetOnMobile`: a popover on
	desktop, a BottomSheet at the mobile breakpoint (DR-6). No new overlay
	primitive, so ESC ordering, outside-click, portal placement and the sheet's
	focus handling are the app's existing ones rather than a second
	implementation of each.

	THE ACTIONS ARE NOT DEFINED HERE. They come from
	`$lib/attachments/actions` and are rendered from the descriptor list
	(DR-5) — this component chooses between the anchor and button branches of
	`MenuItem` on the descriptor's own `element` discriminant, and never calls
	`run()` on an anchor (the browser performs those; calling both would fire
	the action twice). Adding an action means adding a descriptor, not editing
	this file.

	IT OPENS IMMEDIATELY AND COMPLETES THE METADATA AFTER (DR-2, DR-10). The
	open event's `filename` / `mime_type` / `size_bytes` are nullable by
	contract: a chip NodeView knows only what its options give it and fills
	these from an asynchronous HEAD probe that may be incomplete or failed.
	Awaiting that before opening would make a tap feel broken on a slow
	connection, so the panel paints what it was handed and fetches the rest
	itself. The three states are distinguishable, deliberately:

	  - `ok`        — gaps filled in place.
	  - `missing`   — the row is gone (404). AUTHORITATIVE: the panel says so
	                  and every action goes inert, rather than offering a
	                  Download that will fail.
	  - `transient` — an inline, retryable error BESIDE the row it already
	                  knows. Never a blank sheet. Retry goes through
	                  `revalidateAttachmentMetadata`, which invalidates before
	                  refetching — a plain refetch would replay the cached
	                  failure and look broken (DR-10).

	THE DELETE CONFIRMATION IS AN IN-APP DRILL-DOWN (DR-18) — and it is the
	SAME one the strip's tile shows, `AttachmentDeleteConfirm`, rows and prompt
	text both. This panel supplies the sub-view slot; the confirmation owns the
	shape (prompt as `role="presentation"`, `aria-describedby` back-reference,
	Cancel first, destructive row last) and the two warning arms. It is wired as
	the descriptor's `confirmDelete` promise rather than as a bespoke delete
	path, so the descriptor's own identity-snapshot and permission re-check
	across the confirmation stay in force.

	SWITCH-SAFETY. The host swaps this component's props from one attachment
	to another without a `{#key}` remount (a second tap while the panel is
	open), so every await-then-write is fenced through
	`$lib/attachments/viewFence` against the (workspace, attachment) pair —
	the same bug class as the strip's. Read that module's header for why there
	are three fences.

	NOT HERE: `state_generation` and Undo. Delete behaves exactly like today's
	tile delete; the generation token and the Undo toast land across all three
	entry points at once in PLAN-2411 (DR-19).
-->
<script lang="ts">
	import { onDestroy } from 'svelte';
	import Menu from '$lib/components/common/Menu.svelte';
	import MenuItem from '$lib/components/common/MenuItem.svelte';
	import AttachmentIcon from '$lib/attachments/icons/AttachmentIcon.svelte';
	import AttachmentDeleteConfirm from './AttachmentDeleteConfirm.svelte';
	import {
		attachmentActionsFor,
		type AttachmentActionContext,
		type ButtonAttachmentAction,
	} from '$lib/attachments/actions';
	import {
		describeAttachmentType,
		displayFilename,
		formatBytes,
		iconForAttachment,
	} from '$lib/attachments/display';
	import { attachmentRefsIn } from '$lib/utils/commentAttachments';
	import { toastStore } from '$lib/stores/toast.svelte';
	// The metadata + delete-confirm machinery lives in shared modules now
	// (TASK-2473); this component is a consumer + the renderer.
	import { createSurfaceMetadata } from '$lib/attachments/surfaceMetadata.svelte';
	import { createDeleteConfirm } from '$lib/attachments/surfaceDeleteConfirm.svelte';

	interface Props {
		open: boolean;
		wsSlug: string;
		attachmentId: string;
		/**
		 * Seed metadata from the open event. All three are NULLABLE by
		 * contract (DR-2) — the strip populates them from its list row, a chip
		 * may have none of them yet.
		 */
		filename: string | null;
		mimeType: string | null;
		sizeBytes: number | null;
		/** Element the panel positions against and returns focus to. */
		anchor: HTMLElement | null;
		/**
		 * Supplied by the HOST from its own `computeMutationsEnabled(canEdit,
		 * peeking)` — never by the emitting surface, which has no mutation
		 * context (DR-8). Delete is absent-as-disabled without it.
		 */
		mutationsEnabled: boolean;
		/** Persisted item body, for the "still used here" delete warning. */
		itemContent?: string | null;
		/**
		 * The editor's LIVE markdown. `itemContent` lags by design (saved on
		 * flush, not per keystroke), so an image inserted seconds ago wouldn't
		 * trip the warning for exactly the attachment a user is most likely to
		 * delete by mistake. Consulted at confirm time only.
		 */
		liveContent?: (() => string | null) | null;
		/**
		 * Bumped by the host to force a fresh metadata read — parent-item
		 * restore revalidates rather than assuming the prior state holds
		 * (DR-14).
		 */
		revalidateToken?: number;
		/**
		 * The parent item is archived, so attachment reads will 404 — the server
		 * refuses them for an archived parent. The seed metadata the event
		 * carried is therefore not trustworthy as evidence the file is
		 * REACHABLE, and the panel must probe even when it looks complete, so it
		 * lands in the authoritative missing state rather than offering a
		 * Download that fails on click (DR-14).
		 */
		parentArchived?: boolean;
		onclose: () => void;
		onDeleted?: (attachmentId: string) => void;
	}

	let {
		open,
		wsSlug,
		attachmentId,
		filename,
		mimeType,
		sizeBytes,
		anchor,
		mutationsEnabled,
		itemContent = null,
		liveContent = null,
		revalidateToken = 0,
		parentArchived = false,
		onclose,
		onDeleted,
	}: Props = $props();

	const uid = $props.id();
	const promptId = `attachment-delete-note-${uid}`;

	// Action-run state. The metadata + delete-confirm machinery moved out to the
	// shared surface modules (TASK-2473); running the actions stays here.
	let busy = $state(false);
	let actionError = $state<string | null>(null);
	/**
	 * Teardown latch and the deferred-close timer. Plain `let`, not `$state`:
	 * nothing renders from them, and they are read by continuations that must
	 * see the CURRENT value rather than a reactive snapshot.
	 */
	let destroyed = false;
	let deferredClose: ReturnType<typeof setTimeout> | undefined;
	/**
	 * Counts confirmed deletes. `run()` resolves the same way whether the row
	 * was deleted or the user CANCELLED the confirmation, so closing on a
	 * resolved delete would dismiss the panel out from under a cancel — this is
	 * how the two are told apart.
	 */
	let deleteSignal = 0;

	const displayName = $derived(displayFilename(filename));

	// The delete-confirmation machine (DR-18). Owns the confirmation STATE — the
	// pending resolver, the warning wording, the permission-withdrawn abandon —
	// while the delete descriptor still owns the delete itself. `isReferenced`
	// reads `referencedHere()` at request time so it sees unflushed editor edits.
	const deleteConfirm = createDeleteConfirm({
		mutationsEnabled: () => mutationsEnabled,
		isReferenced: () => referencedHere(),
		displayName: () => displayName,
	});

	// The metadata machine (DR-2, DR-10, DR-14). Seeds from the open event, fills
	// the gaps with a HEAD, and owns the (workspace, attachment) fences the
	// panel's own actions/close fence against. A genuine subject change drops the
	// other per-subject state the machine does not own — the confirmation and any
	// in-flight action — exactly as the panel did inline.
	const surfaceMeta = createSurfaceMetadata(
		() => ({
			ws: wsSlug,
			attachmentId,
			seed: { filename, mime_type: mimeType, size_bytes: sizeBytes },
			open,
			parentArchived,
			revalidateToken,
		}),
		{
			onSubjectChange: () => {
				deleteConfirm.cancel();
				busy = false;
				actionError = null;
			},
		}
	);

	// Render-facing views of the metadata machine's settled phase.
	const missing = $derived(surfaceMeta.phase === 'missing');
	const loadFailed = $derived(surfaceMeta.phase === 'transient');
	const loading = $derived(surfaceMeta.slow);
	/**
	 * An archived parent's reachability probe is still in flight.
	 *
	 * Normally the panel offers its actions immediately — that is DR-2's whole
	 * point, and waiting on a probe would make a tap feel broken. But when the
	 * parent is archived we already know reads are refused; the probe is only
	 * confirming it. Offering Download and Open in that window hands the user
	 * an action whose sole outcome is a 404 (final review round 6).
	 */
	const unreachablePending = $derived(parentArchived && loading && !missing);
	// The event's value wins when it has one — it came from a list row, which
	// is at least as good as a HEAD and is available before any fetch. The merge
	// lives in the metadata machine now; the empty-string fallback stays here
	// because `mime` feeds the icon/type helpers, which want a string.
	const mime = $derived(surfaceMeta.fields.mime_type || '');
	const size = $derived(surfaceMeta.fields.size_bytes);
	const iconId = $derived(iconForAttachment(mime || null, filename));
	const typeLabel = $derived(describeAttachmentType(mime || null, filename));
	// Always says at least the type — a panel that opened on a chip with no
	// metadata at all still has to describe SOMETHING while the HEAD is in
	// flight, and "Reading details…" replaces only the part that is genuinely
	// unknown rather than the whole line (DR-10).
	const metaLine = $derived(
		missing
			? 'No longer available'
			: [
					typeLabel,
					size !== null && size !== undefined
						? formatBytes(size)
						: loading
							? 'Reading details…'
							: null,
				]
					.filter(Boolean)
					.join(' · ')
	);
	/**
	 * The panel's accessible name carries filename, type and size (DR-12) —
	 * and the FULL filename, unelided: truncation is a visual affordance, never
	 * an information loss (DR-13).
	 */
	const panelLabel = $derived(`Options for ${displayName}, ${metaLine}`);

	/**
	 * The action context. Built with GETTERS rather than as a snapshot object:
	 * the delete descriptor deliberately re-reads `mutationsEnabled` and the
	 * attachment identity on the far side of the confirmation, and a frozen
	 * object would make those re-checks read the values as they were when the
	 * confirmation opened — exactly the staleness they exist to catch.
	 */
	const ctx: AttachmentActionContext = {
		get workspaceSlug() {
			return wsSlug;
		},
		get attachment() {
			return { id: attachmentId, filename: filename ?? '', mime_type: mime };
		},
		get mutationsEnabled() {
			// PERMISSION only — "may this user delete here", which is the host's
			// answer and nothing else. Deliberately NOT `&& !missing`: that
			// conflates permission with reachability, and once the descriptor
			// started using this to decide whether Delete EXISTS, a gone row
			// lost the row entirely while Open and Download stayed present and
			// disabled beside it. Reachability is enforced where it belongs —
			// the render site disables every action while `missing`, and a
			// delete that races a deletion elsewhere 404s, which the descriptor
			// already treats as authoritative.
			return mutationsEnabled;
		},
		confirmDelete: () => deleteConfirm.request(),
		onDeleted: (id) => {
			deleteSignal += 1;
			onDeleted?.(id);
		},
		onCopied: () => toastStore.show('Link copied to clipboard', 'success'),
	};

	const actions = $derived(attachmentActionsFor(ctx));

	/**
	 * Ids referenced by THIS item's body. A hit means deleting leaves a
	 * missing-attachment placeholder in the content, which the user deserves
	 * to know before confirming. Read at confirm time (not derived) so it sees
	 * unflushed editor edits.
	 */
	function referencedHere(): boolean {
		let live: string | null = null;
		try {
			live = liveContent?.() ?? null;
		} catch {
			live = null;
		}
		return new Set(attachmentRefsIn(live ?? itemContent ?? '')).has(attachmentId);
	}

	async function runAction(action: ButtonAttachmentAction) {
		if (!surfaceMeta.paint.isCurrent()) return;
		if (!action.enabled(ctx)) return;
		// Fence 2: a subject change mid-action must not write this action's
		// outcome onto a DIFFERENT attachment's panel. The request itself still
		// lands — it targets an id, not a view.
		const token = surfaceMeta.viewFence.begin();
		const deletesBefore = deleteSignal;
		actionError = null;
		busy = true;
		try {
			await action.run(ctx);
			if (token.stale()) return;
			// Only when the row actually went: `run()` also resolves when the
			// user cancelled the confirmation, and closing then would dismiss
			// the panel out from under a Cancel.
			if (deleteSignal !== deletesBefore) onclose();
		} catch (err) {
			if (token.stale()) return;
			actionError = err instanceof Error ? err.message : `Couldn't ${action.label.toLowerCase()}`;
		} finally {
			if (!token.stale()) busy = false;
		}
	}

	/**
	 * Teardown (PLAN-2392, orchestrator review).
	 *
	 * The host destroys this component by nulling its request — on an item
	 * switch, on archive, on close. Without this, a delete that resolves after
	 * that point still runs its continuation and calls `onclose()`, which
	 * closes whatever panel the host has open BY THEN: attachment A's delete
	 * dismissing the panel the user just opened on B. And a confirmation left
	 * on screen at teardown would never resolve, stranding the descriptor's
	 * `await` forever.
	 *
	 * So teardown does what a subject change does: invalidate the fences so
	 * every in-flight continuation reads stale, and reject any pending
	 * confirmation.
	 */
	onDestroy(() => {
		destroyed = true;
		clearTimeout(deferredClose);
		deferredClose = undefined;
		// Invalidate the fences so every in-flight continuation reads stale, and
		// reject any pending confirmation so the descriptor's `await` settles.
		surfaceMeta.dispose();
		deleteConfirm.dispose();
	});

	function handleClose() {
		// A confirmation still on screen when the panel closes is a rejection:
		// leaving the promise unresolved would strand the descriptor's `await`
		// forever.
		deleteConfirm.cancel();
		onclose();
	}

	/**
	 * Anchor rows navigate/download by their DEFAULT ACTION, so the close is
	 * deferred to a macrotask. Closing synchronously detaches the `<a>` during
	 * its own click handler, and a detached anchor's navigation is cancelled in
	 * some browsers — the download would silently not happen.
	 */
	function closeAfterNavigation() {
		// Fenced like every other continuation: the timer outlives the click, so
		// by the time it fires the panel may have been reopened on a DIFFERENT
		// attachment (tap a chip, tap another). Closing then would dismiss a
		// panel the user just opened. Cleared on teardown so it cannot fire into
		// a destroyed component either.
		const token = surfaceMeta.viewFence.begin();
		clearTimeout(deferredClose);
		deferredClose = setTimeout(() => {
			deferredClose = undefined;
			if (destroyed) return;
			if (token.stale()) return;
			if (!surfaceMeta.paint.isCurrent()) return;
			handleClose();
		}, 0);
	}
</script>

<Menu
	{open}
	onclose={handleClose}
	trigger={anchor ?? undefined}
	mode="portal"
	width={272}
	sheetOnMobile
	sheetTitle={displayName}
	ariaLabel={panelLabel}
	focusKey={`${attachmentId}:${deleteConfirm.pending ? 'delete' : 'root'}`}
>
	{#if !deleteConfirm.pending}
		<!-- Header. `role="presentation"`, like the item menu's confirm note:
		     a role="menu" owns menuitem / separator / group children, and this
		     says explicitly that the header is none of them. -->
		<div class="ap-header" role="presentation">
			<span class="ap-icon" aria-hidden="true"><AttachmentIcon id={iconId} size={22} /></span>
			<span class="ap-head-text">
				<!-- The full name stays in `title` and in the panel's accessible
				     name; the ellipsis is visual only (DR-13). -->
				<span class="ap-name" title={displayName}>{displayName}</span>
				<span class="ap-meta" class:ap-meta-missing={missing} title={mime || undefined}>
					{metaLine}
				</span>
			</span>
		</div>

		{#if missing}
			<div class="ap-note ap-note-missing" role="presentation">
				This file is no longer available. It may have been deleted.
			</div>
		{:else if loadFailed}
			<!-- Beside what we already know, never instead of it (DR-10). -->
			<div class="ap-note ap-note-error" role="presentation">Couldn't load the file details.</div>
			<MenuItem icon="↻" onclick={() => surfaceMeta.retry()}>Retry</MenuItem>
		{/if}

		{#if actionError}
			<div class="ap-note ap-note-error" role="presentation">{actionError}</div>
		{/if}

		<div class="menu-divider" role="separator"></div>

		{#each actions as action (action.id)}
			<!-- The action's SVG icon, drawn from the shared registry through
			     MenuItem's snippet path — NOT a glyph string (TASK-2472). Declared
			     per-iteration so it closes over THIS action's icon id. -->
			{#snippet actionIcon()}
				<AttachmentIcon id={action.icon} />
			{/snippet}
			{#if action.element === 'anchor'}
				<MenuItem
					iconSnippet={actionIcon}
					href={action.href(ctx)}
					download={action.download?.(ctx)}
					target={action.target}
					rel={action.rel}
					title={action.description}
					disabled={!action.enabled(ctx) || missing || unreachablePending}
					onclick={closeAfterNavigation}
				>
					{action.label}
				</MenuItem>
			{:else}
				<MenuItem
					iconSnippet={actionIcon}
					danger={action.danger}
					title={action.description}
					disabled={!action.enabled(ctx) || missing || busy || unreachablePending}
					onclick={() => runAction(action)}
				>
					{busy && action.id === 'delete' ? 'Deleting…' : action.label}
				</MenuItem>
			{/if}
		{/each}
	{:else}
		<!--
			Delete confirmation as a drill-down sub-view (DR-18). The shape and
			the wording live in the shared component, which the strip's tile
			renders too — one confirmation for one object.
		-->
		<AttachmentDeleteConfirm
			prompt={deleteConfirm.warning ?? ''}
			{promptId}
			oncancel={() => deleteConfirm.cancel()}
			onconfirm={() => deleteConfirm.confirm()}
		/>
	{/if}
</Menu>

<style>
	/* Every rule uses LOGICAL properties (DR-13): the panel has to survive a
	   200-character filename and an RTL locale without pushing its actions
	   off-screen. */
	.ap-header {
		display: flex;
		align-items: flex-start;
		gap: 9px;
		padding-block: 7px 8px;
		padding-inline: 9px;
	}

	.ap-icon {
		flex: 0 0 auto;
		color: var(--text-secondary);
		margin-block-start: 1px;
	}

	/* min-width: 0 on every flex child holding the filename, or the ellipsis
	   below never engages and the row grows instead. */
	.ap-head-text {
		display: flex;
		flex-direction: column;
		gap: 2px;
		min-width: 0;
		flex: 1 1 auto;
	}

	.ap-name {
		min-width: 0;
		font-size: 13px;
		font-weight: 600;
		color: var(--text-primary);
		white-space: nowrap;
		overflow: hidden;
		text-overflow: ellipsis;
	}

	.ap-meta {
		min-width: 0;
		font-size: 11.5px;
		color: var(--text-muted);
		white-space: nowrap;
		overflow: hidden;
		text-overflow: ellipsis;
	}

	.ap-meta-missing {
		color: var(--accent-orange);
	}

	.ap-note {
		padding-block: 4px 6px;
		padding-inline: 9px;
		font-size: 12px;
		line-height: 1.35;
		overflow-wrap: anywhere;
	}

	.ap-note-error {
		color: var(--accent-red);
	}

	.ap-note-missing {
		color: var(--text-muted);
	}

	.menu-divider {
		border-block-start: 1px solid var(--border-subtle);
		margin-block: 5px;
		margin-inline: 4px;
	}
</style>
