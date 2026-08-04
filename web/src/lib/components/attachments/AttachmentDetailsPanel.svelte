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
	import { onDestroy, untrack } from 'svelte';
	import Menu from '$lib/components/common/Menu.svelte';
	import MenuItem from '$lib/components/common/MenuItem.svelte';
	import AttachmentIcon from '$lib/attachments/icons/AttachmentIcon.svelte';
	import AttachmentDeleteConfirm, {
		attachmentDeletePrompt,
	} from './AttachmentDeleteConfirm.svelte';
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
	import { api } from '$lib/api/client';
	import {
		fetchAttachmentMetadata,
		revalidateAttachmentMetadata,
	} from '$lib/components/editor/attachment-metadata';
	import { attachmentRefsIn } from '$lib/utils/commentAttachments';
	import { toastStore } from '$lib/stores/toast.svelte';
	import { createFence, createPaintFence, viewIdentity } from '$lib/attachments/viewFence';

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

	/**
	 * How long a metadata read may hang before the panel calls it a failure.
	 * Generous: this is the "something is wrong" threshold, not a latency
	 * budget — a slow answer that arrives still wins.
	 */
	const METADATA_SLOW_MS = 10_000;

	const uid = $props.id();
	const promptId = `attachment-delete-note-${uid}`;

	// What the server told us, filling the gaps in what the event carried.
	let fetchedMime = $state<string | null>(null);
	let fetchedSize = $state<number | null>(null);
	let loading = $state(false);
	/** 404 — authoritative. Actions go inert. */
	let missing = $state(false);
	/** Non-404 failure — inline, retryable, alongside what we already know. */
	let loadFailed = $state(false);
	let view = $state<'root' | 'delete'>('root');
	let busy = $state(false);
	let actionError = $state<string | null>(null);
	let deletePrompt = $state('');
	/** Bumped by Retry; drives the loader effect's forced-revalidate path. */
	let forceReload = $state(0);

	// --- fences (see $lib/attachments/viewFence) ------------------------------
	// The identity of what this panel is showing. The PAIR, not the id alone:
	// the workspace half is what a hand-rolled fence keeps forgetting.
	const identity = viewIdentity(() => ({ ws: wsSlug, att: attachmentId }));
	// 1. Request fence — restarted per metadata read, so a Retry supersedes
	//    its own predecessor and only the newest response may write.
	const loadFence = createFence(identity);
	// 2. View fence — invalidated only when the panel really changes subject,
	//    so an in-flight delete of the attachment still on screen can still
	//    reconcile even after a Retry reloaded its metadata.
	const viewFence = createFence(identity);
	// 3. Paint fence — "does the control the user clicked belong to what is on
	//    screen?" Checked at ENTRY by every control, because the other two run
	//    after an await and no fence can unsend a request.
	const paint = createPaintFence(identity);

	// Plain `let`, never $state: read and written only inside effects, and a
	// $state here would make the effect below depend on what it writes
	// (CONVE-1688 — the self-write loop that silently aborts the flush).
	let paintedKey: string | null = null;
	/**
	 * The reload stamp this component has already acted on — the host's
	 * revalidate signal and the local Retry counter together. Seeded from the
	 * incoming prop so a host that has already bumped its counter (an earlier
	 * restore, before this panel existed) doesn't read as a pending reload on
	 * the first render.
	 */
	let seenReload = untrack(() => `${revalidateToken}:0`);
	/** Resolver for the in-app confirmation currently on screen, if any. */
	let pendingConfirm: ((confirmed: boolean) => void) | null = null;
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
	// is at least as good as a HEAD and is available before any fetch.
	const mime = $derived(mimeType || fetchedMime || '');
	const size = $derived(sizeBytes ?? fetchedSize);
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
		confirmDelete: () => confirmDelete(),
		onDeleted: (id) => {
			deleteSignal += 1;
			onDeleted?.(id);
		},
		onCopied: () => toastStore.show('Link copied to clipboard', 'success'),
	};

	const actions = $derived(attachmentActionsFor(ctx));

	function downloadUrl(uuid: string, variant?: 'thumb-sm' | 'thumb-md' | 'original'): string {
		return api.attachments.downloadUrl(wsSlug, uuid, variant);
	}

	/**
	 * Load whatever the event didn't carry, and re-read on demand.
	 *
	 * Reads only props + the fence identity in tracked scope; every piece of
	 * state it writes (`loading`, `missing`, `fetched*`) is read in the markup
	 * and in `untrack`ed blocks only, so the effect cannot self-invalidate.
	 */
	$effect(() => {
		const req = loadFence.restart();
		const isOpen = open;
		const seedMime = mimeType;
		const seedSize = sizeBytes;
		const reloadStamp = `${revalidateToken}:${forceReload}`;
		const archivedParent = parentArchived;

		let forced = false;
		untrack(() => {
			// A genuine subject change: drop everything the previous attachment
			// left behind, stop any in-flight continuation from reconciling, and
			// abandon a confirmation that was up for a file the user is no longer
			// looking at.
			if (req.key !== paintedKey) {
				paintedKey = req.key;
				viewFence.invalidate();
				settleConfirm(false);
				fetchedMime = null;
				fetchedSize = null;
				missing = false;
				loadFailed = false;
				busy = false;
				actionError = null;
			}
			// Whatever this run paints belongs to this (workspace, attachment).
			// An un-addressable token records nothing, which correctly stops the
			// panel's controls claiming the previous subject.
			paint.record(req);
			// An archived parent makes this a REACHABILITY question, and the
			// metadata cache can hold an `ok` observed before the archive — the
			// same reason existence probes elsewhere revalidate rather than
			// read. So force it, which also routes through the invalidate-then-
			// fetch path instead of replaying a stale success.
			if (archivedParent) forced = true;
			if (reloadStamp !== seenReload) {
				seenReload = reloadStamp;
				forced = true;
				// A forced revalidation exists because the previous answer may no
				// longer hold — the parent was just restored (DR-14). So drop the
				// latched `missing` NOW rather than only on an `ok`: if this
				// revalidation comes back transient, the render must fall through
				// to the retryable error. Leaving it latched shows "no longer
				// available" with no way to ask again, which is the exact
				// empty-vs-broken confusion DR-10 exists to prevent.
				missing = false;
			}
		});

		if (!isOpen || req.key === null) return;
		// Nothing to complete: the strip's entry point always has all three.
		// Unless the parent is archived — then "complete" and "reachable" are
		// different claims, and only a probe settles the second one.
		if (!forced && !archivedParent && seedMime && seedSize !== null && seedSize !== undefined) {
			return;
		}

		loading = true;
		loadFailed = false;
		// A HEAD that never settles is not a failure the fetch layer can report:
		// no rejection arrives, so without this the panel sits on "Reading
		// details…" forever with no Retry — indistinguishable to the user from
		// a hang, and the exact loading-vs-failed confusion DR-10 exists to
		// prevent. The request is NOT aborted: if it does eventually answer, it
		// is still the truth and still allowed to correct the error state.
		const slowTimer = setTimeout(() => {
			if (req.stale()) return;
			loading = false;
			loadFailed = true;
		}, METADATA_SLOW_MS);
		void (async () => {
			// The workspace comes off the TOKEN, not the live prop: the request
			// must name the workspace it was issued for even if the panel has
			// since moved on.
			const result = forced
				? await revalidateAttachmentMetadata(req.value.ws, req.value.att, downloadUrl)
				: await fetchAttachmentMetadata(req.value.ws, req.value.att, downloadUrl);
			clearTimeout(slowTimer);
			if (req.stale()) return;
			loading = false;
			if (result.status === 'ok') {
				fetchedMime = result.mime;
				fetchedSize = result.size;
				missing = false;
				loadFailed = false;
			} else if (result.status === 'missing') {
				// Authoritative. Latch it — the actions go inert below.
				missing = true;
				loadFailed = false;
			} else {
				// Says nothing about whether the row exists: keep showing what we
				// have and stay retryable.
				loadFailed = true;
			}
		})();
	});

	/**
	 * Permission withdrawn while the confirmation is open.
	 *
	 * The pane can go peeked (or a role can change) between opening the
	 * confirmation and answering it. Blocking the eventual request is not
	 * enough: the user is left looking at a live "Delete file" button for an
	 * action that can no longer happen, on a surface that is supposed to offer
	 * no delete at all in that state. So the confirmation is abandoned as a
	 * rejection, exactly as a subject change abandons it.
	 *
	 * Plain latch + `untrack` for the writes: as `$state` this effect would
	 * depend on what it writes, which aborts the flush and strands unrelated
	 * reactivity (CONVE-1688).
	 */
	$effect(() => {
		const mayMutate = mutationsEnabled;
		untrack(() => {
			if (mayMutate || view !== 'delete') return;
			settleConfirm(false);
		});
	});

	function retry() {
		// ENTRY fence: the clicked row was painted for `paint`'s identity, and
		// the live props may already name a different attachment.
		if (!paint.isCurrent()) return;
		loadFailed = false;
		// Goes through the loader effect's revalidate path rather than fetching
		// here, so a user Retry and the host's restore signal (DR-14) are ONE
		// code path — and both therefore invalidate before refetching, which is
		// the whole point of Retry (DR-10).
		forceReload += 1;
	}

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

	/**
	 * The confirmation, as a promise the delete descriptor awaits. The
	 * descriptor snapshots identity BEFORE this and re-checks permission after
	 * it resolves, which is the whole reason it is wired this way rather than
	 * as a bespoke "confirm, then call the API" path here.
	 *
	 * The wording comes from the shared `attachmentDeletePrompt` — the same
	 * two arms the strip's tile shows (DR-18). The hedged arm matters here for
	 * one EXTRA reason beyond the shared one: the body this checks is the
	 * HOST's, which is not necessarily the attachment's parent item. The
	 * open-panel event's `itemId` is ROUTING, not ownership: a chip in a reused
	 * comment composer's unsubmitted draft correctly routes to the host in
	 * front of the user even after an item switch.
	 */
	function confirmDelete(): Promise<boolean> {
		deletePrompt = attachmentDeletePrompt(displayName, referencedHere());
		return new Promise<boolean>((resolve) => {
			// Supersede any confirmation already up — two open at once would
			// leave one resolver dangling forever.
			pendingConfirm?.(false);
			pendingConfirm = resolve;
			view = 'delete';
		});
	}

	function settleConfirm(confirmed: boolean) {
		const resolve = pendingConfirm;
		pendingConfirm = null;
		view = 'root';
		resolve?.(confirmed);
	}

	async function runAction(action: ButtonAttachmentAction) {
		if (!paint.isCurrent()) return;
		if (!action.enabled(ctx)) return;
		// Fence 2: a subject change mid-action must not write this action's
		// outcome onto a DIFFERENT attachment's panel. The request itself still
		// lands — it targets an id, not a view.
		const token = viewFence.begin();
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
		viewFence.invalidate();
		loadFence.invalidate();
		paint.record(null);
		const resolve = pendingConfirm;
		pendingConfirm = null;
		resolve?.(false);
	});

	function handleClose() {
		// A confirmation still on screen when the panel closes is a rejection:
		// leaving the promise unresolved would strand the descriptor's `await`
		// forever.
		settleConfirm(false);
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
		const token = viewFence.begin();
		clearTimeout(deferredClose);
		deferredClose = setTimeout(() => {
			deferredClose = undefined;
			if (destroyed) return;
			if (token.stale()) return;
			if (!paint.isCurrent()) return;
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
	focusKey={`${attachmentId}:${view}`}
>
	{#if view === 'root'}
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
			<MenuItem icon="↻" onclick={retry}>Retry</MenuItem>
		{/if}

		{#if actionError}
			<div class="ap-note ap-note-error" role="presentation">{actionError}</div>
		{/if}

		<div class="menu-divider" role="separator"></div>

		{#each actions as action (action.id)}
			{#if action.element === 'anchor'}
				<MenuItem
					icon={action.icon}
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
					icon={action.icon}
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
			prompt={deletePrompt}
			{promptId}
			oncancel={() => settleConfirm(false)}
			onconfirm={() => settleConfirm(true)}
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
