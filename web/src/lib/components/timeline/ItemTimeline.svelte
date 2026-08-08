<script lang="ts">
	import { onDestroy, tick, untrack } from 'svelte';
	import { api } from '$lib/api/client';
	import { sseService } from '$lib/services/sse.svelte';
	import { authStore } from '$lib/stores/auth.svelte';
	import { workspaceStore } from '$lib/stores/workspace.svelte';
	import type { TimelineEntry, TimelineResponse, Item } from '$lib/types';
	import TimelineCommentCard from './TimelineCommentCard.svelte';
	import TimelineActivityCard from './TimelineActivityCard.svelte';
	import TimelineVersionCard from './TimelineVersionCard.svelte';
	import { attachmentRefsIn } from '$lib/utils/commentAttachments';
	import { fetchAttachmentMetadata } from '$lib/components/editor/attachment-metadata';
	import { attachmentDownloadUrl, type AttachmentMeta } from '$lib/markdown/attachments';
	import { canOpenInViewer } from '$lib/attachments/display';
	import { isEditorOwnedImage } from '$lib/attachments/editorOwnedImage';
	// One declaration of the viewer's image shape, on the channel (TASK-2431).
	import type { LightboxImage } from '$lib/attachments/events';
	import Lightbox from '$lib/components/common/Lightbox.svelte';
	import CommentEditor from '$lib/components/CommentEditor.svelte';

	interface Props {
		wsSlug: string;
		/**
		 * Workspace owner's username — needed so comment wiki-links render
		 * with the full `/{username}/{workspace}/...` href. Without it the
		 * links resolve visually but navigate to a dead 3-segment route
		 * (BUG-1744 verification finding).
		 */
		username?: string;
		itemSlug: string;
		currentContent: string;
		items?: Item[];
		onRestore?: (item: Item) => void;
		/**
		 * itemId + collectionId let the timeline answer
		 * `workspaceStore.canEditItem(...)` for write-affordance gating.
		 * Optional — when missing, the composer / reply / reaction / delete
		 * controls fall back to "no edit" (PLAN-1100 / TASK-1107). The slug
		 * page already has the full item, so always passes both.
		 */
		itemId?: string;
		collectionId?: string;
		/**
		 * Which entry kinds render (undefined = all). Filter-only: the merged
		 * feed still fetches every kind, so switching kinds never refetches.
		 */
		visibleKinds?: Array<'comment' | 'activity' | 'version'>;
		/**
		 * `frozen` freezes the COMMENT/REACTION surfaces (composer, reply, edit,
		 * delete, reaction). These are per-item / per-user REST entities, so under
		 * the invisible-freeze model (BUG-2263) the full-page host leaves this
		 * `false` even while peeking — comments stay live on the passive side.
		 * Defaults false → byte-identical for every existing caller.
		 */
		frozen?: boolean;
		/**
		 * `restoreFrozen` freezes ONLY version restore. Unlike comments, a restore
		 * REST-writes this item's `items.content` directly (not via the Y.Doc
		 * applier), so on a peeking side whose Y.Doc is retained-alive it can be
		 * overwritten by a later collab flush — a SAME-ITEM collision the
		 * different-master/pane premise does NOT cover (BUG-2263 / Codex P1). The
		 * host passes `restoreFrozen={peeking}` so restore stays confined to the
		 * active editor. Defaults false → byte-identical for every existing caller.
		 */
		restoreFrozen?: boolean;
		/**
		 * BUG-2271: forwarded verbatim to TimelineVersionCard so the initiating
		 * client can flush its LIVE collab editor markdown into items.content
		 * BEFORE a restore captures the server-side undo-point (otherwise in-flight
		 * edits still in the ~5s flush-debounce window are lost). Threaded from
		 * ItemDetail, which owns the collab flusher. Unset → no-op for non-collab
		 * callers, so existing usage is unaffected.
		 */
		flushBeforeRestore?: () => Promise<void>;
		/**
		 * Identity of the `ItemDetail` mount that owns this timeline
		 * (PLAN-2392 DR-8 / TASK-2421). Forwarded verbatim to every
		 * CommentEditor this timeline mounts — the composer here, and the
		 * edit/reply composers inside TimelineCommentCard — so an attachment
		 * chip in a comment body can address the ONE host that owns it. A
		 * master and a peeked pane are both mounted on the same module-global
		 * bus, so `itemId` alone is not an address. Empty (the default, for
		 * callers outside an ItemDetail) disables addressing.
		 */
		hostToken?: string;
		/**
		 * Viewer-toolbar context (TASK-2474), forwarded verbatim to the `Lightbox`
		 * this timeline mounts. `mutationsEnabled` is the HOST's answer
		 * (`canEdit && !peeking`) — deliberately NOT this component's own `canEdit`
		 * below, which ignores peeking, so a peeked master's timeline viewer would
		 * wrongly offer Delete. DEFAULT false → read-only toolbar. The two content
		 * getters back the delete-warning check (DR-5).
		 */
		mutationsEnabled?: boolean;
		getItemContent?: () => string | null;
		getLiveContent?: () => string | null;
	}

	let { wsSlug, username = '', itemSlug, currentContent, items = [], onRestore, itemId, collectionId, frozen = false, restoreFrozen = false, flushBeforeRestore, visibleKinds, hostToken = '', mutationsEnabled = false, getItemContent, getLiveContent }: Props = $props();

	// Resolve canEditItem reactively; falls to false if itemId/collectionId
	// aren't supplied (e.g. an older caller). Folds in the master-freeze gate
	// (TASK-2172): while `frozen`, the composer / reply / reaction / delete
	// affordances all disable through this single derived.
	let canEdit = $derived(
		!frozen &&
		(itemId && collectionId
			? workspaceStore.canEditItem({ id: itemId, collection_id: collectionId })
			: false)
	);

	let entries: TimelineEntry[] = $state([]);

	// Render-side filter over the one merged feed. The instance stays
	// mounted across filter changes (SSE subscriptions live on) — the pane's
	// Activity/Versions tabs drive this (PLAN-2290 Phase 4). undefined = all.
	let visibleEntries = $derived(
		visibleKinds ? entries.filter((e) => visibleKinds.includes(e.kind)) : entries
	);
	let showComposer = $derived(!visibleKinds || visibleKinds.includes('comment'));
	let hasMore: boolean = $state(false);
	let loading: boolean = $state(false);
	let loadingMore: boolean = $state(false);
	let error: string = $state('');

	// Resolver for `pad-attachment:UUID` references in comment bodies.
	// Metadata (MIME + size) is fetched lazily per UUID via a HEAD probe and
	// cached here; renderMarkdown reads it through the derived resolver so a
	// newly-resolved attachment re-renders its comment inline.
	let attMeta = $state<Map<string, AttachmentMeta>>(new Map());
	let attachmentResolver = $derived((uuid: string) => attMeta.get(uuid) ?? null);

	// In-flight / settled UUIDs — a non-reactive guard so the probe effect
	// fires one HEAD per attachment without re-triggering on attMeta writes.
	const probed = new Set<string>();

	function probeAttachment(uuid: string) {
		if (probed.has(uuid)) return;
		probed.add(uuid);
		// Capture the workspace identity before the HEAD probe. `attMeta` is a
		// workspace-scoped attachment cache; ItemDetail reuses this panel across
		// a no-{#key} item switch (its wsSlug/itemSlug props just change), so a
		// probe resolving after a workspace switch must NOT write the old
		// workspace's attachment metadata into the new item's cache (TASK-2112).
		const reqWs = wsSlug;
		fetchAttachmentMetadata(wsSlug, uuid, (id, variant) =>
			attachmentDownloadUrl(wsSlug, id, variant)
		).then((m) => {
			// A transient failure (5xx / network) is not evidence about the
			// row, and the helper deliberately doesn't cache it — so drop the
			// probed mark too, or this panel could never ask again for the
			// rest of the mount (PLAN-2392 DR-17). Note what this does and
			// does not buy: clearing the mark makes the attachment eligible
			// again on the NEXT run of the probe effect (a new comment, an
			// edit, a remount), it does not schedule a retry of its own. A
			// proactive retry belongs with the timeline's deletion
			// subscription in phase 3c, not here. A `missing` result IS
			// authoritative: leave the mark set and leave `attMeta` without an
			// entry, which is what the renderer already degrades to a missing
			// placeholder on.
			if (m.status === 'transient') {
				probed.delete(uuid);
				return;
			}
			if (m.status !== 'ok') return;
			if (reqWs !== wsSlug) return;
			const next = new Map(attMeta);
			// filename is left empty — the markdown alt text is the chip/img
			// label, and renderAttachmentImage only falls back to filename
			// when alt is blank.
			next.set(uuid, { id: uuid, mime_type: m.mime, filename: '', size_bytes: m.size });
			attMeta = next;
		});
	}

	// Probe every attachment referenced by a comment or reply as the
	// timeline loads / changes. Depends only on `entries`.
	$effect(() => {
		for (const entry of entries) {
			if (entry.kind !== 'comment' || !entry.comment) continue;
			for (const id of attachmentRefsIn(entry.comment.body)) probeAttachment(id);
			for (const reply of entry.comment.replies ?? []) {
				for (const id of attachmentRefsIn(reply.body)) probeAttachment(id);
			}
		}
	});

	// Lightbox state (IDEA-1660). Set when a thumbnail is activated; cleared
	// on close. Null = closed, so the host remounts fresh on each open.
	let lightbox: { images: LightboxImage[]; index: number; invoker: HTMLElement | null } | null =
		$state(null);
	let entryListEl: HTMLElement | undefined = $state();

	/**
	 * The DR-16 open gate, applied to ONE rendered thumbnail (TASK-2431).
	 *
	 * Returns the viewer's own record for an `<img data-attachment-id>`, or
	 * null when that image may not be opened. Null is the answer in two
	 * different situations and deliberately does not distinguish them:
	 *
	 *  - the MIME is known and is not on the allowlist. `image/svg+xml` is the
	 *    one that matters — SVG carries active content, and the markdown
	 *    renderer emits an `<img>` for ANY `image/*` (`markdown/attachments.ts`
	 *    `isImageMime`), which is the correct decision for RENDERING and the
	 *    wrong one for OPENING. That difference is the whole reason this
	 *    function exists rather than a `startsWith('image/')` test.
	 *  - the MIME is not known yet. Fail SAFE: an unresolved probe is not
	 *    evidence that a file is a PNG. It costs nothing, because an image
	 *    whose metadata has not resolved is not rendered as an `<img>` at all
	 *    (the renderer shows a "missing" placeholder until `attMeta` has the
	 *    row), so there is no cold-start case where this refuses something the
	 *    user can see and could previously open. A later probe re-runs the
	 *    semantics effect below and the thumbnail becomes clickable then.
	 *
	 * Read straight off the CACHED metadata — never a fresh HEAD per click.
	 * `attMeta` is the same map the renderer resolved the image through, so the
	 * gate and the picture on screen are answering from one source.
	 */
	function viewerImageFor(el: HTMLElement): LightboxImage | null {
		const id = el.getAttribute('data-attachment-id') ?? '';
		if (!id) return null;
		const meta = attMeta.get(id);
		if (!meta || !canOpenInViewer(meta.mime_type)) return null;
		return {
			id,
			alt: el.getAttribute('alt') ?? '',
			// The probe leaves filename empty (see probeAttachment) and HEAD
			// carries no intrinsic dimensions, so most of these are null here.
			// That is what nullable means on this type; the strip fills them.
			filename: meta.filename || null,
			mime_type: meta.mime_type,
			size_bytes: meta.size_bytes ?? null,
			width: meta.width ?? null,
			height: meta.height ?? null,
		};
	}

	/**
	 * Open the viewer on an activated thumbnail, with its siblings in the same
	 * comment/reply body so ←/→ can page them.
	 *
	 * THE WHOLE LIST IS GATED, not just the clicked image (TASK-2431). Before
	 * this, the set was built from every `img[data-attachment-id]` in scope with
	 * no MIME consulted at all, so opening a safe PNG and pressing → could land
	 * on an SVG: gating the click alone is not a gate.
	 *
	 * And the index is derived from the clicked attachment's ID, not from its
	 * position among the DOM elements — filtering reindexes everything after the
	 * first refusal, so a DOM index would silently open the wrong image. When the
	 * clicked image is itself refused it is not in the list, `findIndex` returns
	 * -1, and nothing opens.
	 */
	function openLightboxFromImg(imgEl: HTMLElement): boolean {
		const clickedId = imgEl.getAttribute('data-attachment-id') ?? '';
		if (!clickedId) return false;
		const scope = imgEl.closest('.comment-body, .reply-body') ?? imgEl.parentElement;
		const els = scope
			? Array.from(scope.querySelectorAll<HTMLElement>('img[data-attachment-id]'))
			: [imgEl];
		const list = els
			.map((el) => viewerImageFor(el))
			.filter((x): x is LightboxImage => x !== null);
		// A body can legitimately embed the same attachment twice; the first
		// occurrence wins, which shows the image the user asked for.
		const index = list.findIndex((x) => x.id === clickedId);
		if (index < 0) return false;
		lightbox = { images: list, index, invoker: imgEl };
		return true;
	}

	// BOTH activation routes go through the one gated opener — a filter applied
	// to the mouse path only would be no filter at all.
	//
	// `preventDefault` is now conditional on actually opening: swallowing the
	// event for a thumbnail the viewer refuses would leave the click doing
	// nothing at all, including whatever the surrounding markup would have done
	// with it.
	/**
	 * The image this delegated event is about, or null when the event is not
	 * ours to act on.
	 *
	 * The entry list contains LIVE CommentEditor instances, whose inline images
	 * are AttachmentImage NodeViews that own their own activation, semantics and
	 * propagation (TASK-2432). They render the same `img[data-attachment-id]`
	 * this selector matches, so without the ownership check this delegation acts
	 * on elements belonging to another component — see `isEditorOwnedImage` for
	 * the two failures that produced.
	 */
	function delegatedThumb(e: Event): HTMLElement | null {
		const imgEl = (e.target as HTMLElement | null)?.closest(
			'img[data-attachment-id]'
		) as HTMLElement | null;
		if (!imgEl || isEditorOwnedImage(imgEl)) return null;
		return imgEl;
	}

	function onThumbClick(e: MouseEvent) {
		const imgEl = delegatedThumb(e);
		if (!imgEl) return;
		if (openLightboxFromImg(imgEl)) e.preventDefault();
	}

	function onThumbKeydown(e: KeyboardEvent) {
		if (e.key !== 'Enter' && e.key !== ' ') return;
		// A modified key is a shortcut, not an activation — the same guard the
		// NodeView carries. Cmd/Ctrl+Enter is CommentEditor's submit binding.
		if (e.ctrlKey || e.metaKey || e.altKey || e.shiftKey) return;
		const imgEl = delegatedThumb(e);
		if (!imgEl) return;
		// Space would otherwise scroll the page — but only suppress it when the
		// key actually did something.
		if (openLightboxFromImg(imgEl)) e.preventDefault();
	}

	// Delegated click + keydown on the entry list (rather than declarative
	// handlers on the static container, which the a11y lint would flag).
	$effect(() => {
		const el = entryListEl;
		if (!el) return;
		el.addEventListener('click', onThumbClick);
		el.addEventListener('keydown', onThumbKeydown);
		return () => {
			el.removeEventListener('click', onThumbClick);
			el.removeEventListener('keydown', onThumbKeydown);
		};
	});

	// Inline attachment images come from sanitized {@html}, so we can't wrap
	// them in a <button> at render time. Instead make each one a focusable,
	// announced control imperatively so keyboard users can open the lightbox.
	// Depends on BOTH `entries` (new comments) AND `attMeta` (an image only
	// renders as an <img> once its metadata resolves — before that it's a
	// "missing" placeholder span — so the pass must re-run on resolution).
	//
	// THE SEMANTICS ARE CONDITIONAL ON THE SAME GATE THE OPENER USES
	// (TASK-2431). Previously every `image/*` thumbnail got `role="button"`, a
	// tabindex and a "View image" name; with the opener now refusing the ones
	// outside the allowlist, that would leave an SVG as a focus stop announced
	// as a button whose activation does nothing — a worse outcome than the hole
	// it replaces, and the reason this pass has to be able to take semantics
	// BACK OFF an element (a probe can resolve a MIME that turns a thumbnail
	// from viewable-by-assumption into refused).
	$effect(() => {
		// `visibleEntries`, NOT `entries`: the pane's Activity / Versions tabs
		// filter the rendered set without refetching, so flipping away from
		// comments and back DESTROYS and rebuilds every comment card while
		// `entries` never changes. Tracking the raw list left those rebuilt
		// images mouse-openable (the delegated listeners live on the container,
		// which survives) but with no role, no tabindex and no name — openable
		// by mouse only, which is the dead-control failure in its other
		// direction (Codex round 4). Reading the derived also reads `entries`,
		// so nothing is lost by not naming it too.
		void visibleEntries;
		void attMeta;
		const el = entryListEl;
		if (!el) return;
		// The DOM pass is deferred a tick, so it can land after an item /
		// workspace switch has already replaced what it was written for. Nothing
		// it does is destructive, but a cancelled continuation is the local house
		// rule for every await-then-write here (TASK-2112).
		let cancelled = false;
		tick().then(() => {
			if (cancelled) return;
			for (const img of el.querySelectorAll<HTMLElement>('img[data-attachment-id]')) {
				// A live editor's NodeView owns its own semantics (TASK-2432) and
				// `attMeta` is probed from SAVED bodies only — so every image in a
				// DRAFT comment looks unresolved here, and this pass would strip
				// the role/tabindex the NodeView just set.
				if (isEditorOwnedImage(img)) continue;
				if (viewerImageFor(img) === null) {
					img.removeAttribute('role');
					img.removeAttribute('tabindex');
					img.removeAttribute('aria-label');
					continue;
				}
				if (img.getAttribute('role') === 'button') continue;
				img.setAttribute('role', 'button');
				img.setAttribute('tabindex', '0');
				const alt = img.getAttribute('alt');
				img.setAttribute('aria-label', alt ? `View image: ${alt}` : 'View attachment image');
			}
		});
		return () => {
			cancelled = true;
		};
	});

	// Current user ID for reaction toggle — read from the global auth store.
	let currentUserId = $derived(authStore.userId);
	let isAdmin = $derived(authStore.user?.role === 'admin');

	// Track IDs from the most recent first-page fetch, used by SSE merge
	// to detect deletions without incorrectly removing older-page entries.
	let firstPageIds = $state<Set<string>>(new Set());

	async function loadTimeline() {
		// Capture the request identity (item + workspace) BEFORE the await.
		// ItemDetail reuses this panel across a no-{#key} item switch (its
		// itemSlug/wsSlug props just change), so a slower A load must NOT
		// overwrite B's entries / error / spinner (TASK-2112).
		const reqSlug = itemSlug;
		const reqWs = wsSlug;
		loading = true;
		error = '';
		try {
			const resp: TimelineResponse = await api.timeline.list(reqWs, reqSlug);
			if (reqSlug !== itemSlug || reqWs !== wsSlug) return;
			entries = resp.entries;
			hasMore = resp.has_more;
			firstPageIds = new Set(resp.entries.map((e) => e.id));
		} catch (err: any) {
			if (reqSlug !== itemSlug || reqWs !== wsSlug) return;
			error = err?.message ?? 'Failed to load timeline';
		} finally {
			if (reqSlug === itemSlug && reqWs === wsSlug) loading = false;
		}
	}

	async function loadMore() {
		if (loadingMore || entries.length === 0) return;
		// Capture identity before the await so a switch mid-flight can't append
		// A's older page onto B's entries (TASK-2112).
		const reqSlug = itemSlug;
		const reqWs = wsSlug;
		const oldest = entries[entries.length - 1];
		loadingMore = true;
		try {
			const resp: TimelineResponse = await api.timeline.list(reqWs, reqSlug, {
				before: oldest.created_at,
				before_id: oldest.id
			});
			if (reqSlug !== itemSlug || reqWs !== wsSlug) return;
			// Deduplicate by ID to handle boundary overlap from <= queries.
			const existingIds = new Set(entries.map((e) => e.id));
			const newEntries = resp.entries.filter((e) => !existingIds.has(e.id));
			entries = [...entries, ...newEntries];
			hasMore = resp.has_more;
		} catch (err: any) {
			if (reqSlug !== itemSlug || reqWs !== wsSlug) return;
			error = err?.message ?? 'Failed to load more';
		} finally {
			if (reqSlug === itemSlug && reqWs === wsSlug) loadingMore = false;
		}
	}

	/**
	 * The view the currently painted timeline belongs to. Plain `let` + untrack,
	 * NOT `$state`: the effect below both reads and writes it, and a `$state`
	 * read inside its own writing effect self-depends, aborts the flush and
	 * silently strands unrelated reactivity (CONVE-1688). Seeded from the
	 * initial props so a fresh mount is not treated as a switch — there is
	 * nothing to tear down on the first run.
	 */
	// The two parts are joined with a separator that cannot occur in a slug,
	// so no pair of (workspace, item) values can collide into one key.
	let lastView = untrack(() => `${wsSlug}/${itemSlug}`);

	$effect(() => {
		const ws = wsSlug;
		const slug = itemSlug;
		// A→B LIFECYCLE (TASK-2431). This component is reused across an item /
		// workspace switch (no `{#key}`), and it had NO viewer reset at all —
		// `lightbox` was cleared on close and nowhere else. So a switch left a
		// full-screen viewer up over the incoming item, still holding the
		// previous view's attachment ids while `Lightbox` rebuilt their URLs
		// from the workspace it captured at open. The strip's reset is the
		// pattern; this is the same rule stated for this component.
		//
		// `attMeta` is deliberately NOT cleared alongside it. It is keyed by a
		// bare uuid where the shared HEAD cache is keyed `ws:uuid`, which looks
		// like a workspace-scoped cache leaking across the switch — but an
		// attachment id is a UUID belonging to exactly one workspace, so an
		// entry can only ever answer for the id it describes. Clearing it would
		// buy nothing and cost something real: the probe effect refills it only
		// when `entries` next changes, so a FAILED load after the switch would
		// leave every already-rendered image permanently un-openable for the
		// rest of the mount (Codex round 3).
		untrack(() => {
			const view = `${ws}/${slug}`;
			if (view === lastView) return;
			lastView = view;
			lightbox = null;
		});
		loadTimeline();
	});

	// Only refresh the timeline for comment/reaction events — NOT item_updated.
	// Content saves create version-diff entries that appear on next natural
	// refresh (new comment, page load). Refreshing on every content save caused
	// visible shakiness and rate-limit errors from rapid SSE replay.
	const relevantEvents = new Set([
		'comment_created',
		'comment_updated',
		'comment_deleted',
		'reaction_added',
		'reaction_removed'
	]);

	// Debounce SSE-driven refreshes so rapid-fire event replays (e.g. on
	// page reconnect) don't hammer the timeline endpoint.
	let sseRefreshTimer: ReturnType<typeof setTimeout> | undefined;

	const unsubscribe = sseService.onItemEvent((event) => {
		if (relevantEvents.has(event.type)) {
			clearTimeout(sseRefreshTimer);
			sseRefreshTimer = setTimeout(async () => {
				// Capture identity before the await — this same panel instance
				// serves the next item after a no-{#key} switch, so a debounced
				// refresh resolving late must not merge A's entries into B (TASK-2112).
				const reqSlug = itemSlug;
				const reqWs = wsSlug;
				try {
					const resp: TimelineResponse = await api.timeline.list(reqWs, reqSlug);
					if (reqSlug !== itemSlug || reqWs !== wsSlug) return;
					const freshIds = new Set(resp.entries.map((e) => e.id));
					const existingIds = new Set(entries.map((e) => e.id));

					// Prepend genuinely new entries.
					const newEntries = resp.entries.filter((e) => !existingIds.has(e.id));

					// Update existing entries from the fresh response (e.g., reaction changes).
					// Remove entries that were previously on the first page but are now gone (deleted).
					// Keep all entries from older pages (loaded via "Load more") untouched.
					const freshById = new Map(resp.entries.map((e) => [e.id, e]));
					const updatedExisting = entries
						.filter((e) => {
							if (firstPageIds.has(e.id) && !freshIds.has(e.id)) return false;
							return true;
						})
						.map((e) => freshById.get(e.id) ?? e);

					entries = [...newEntries, ...updatedExisting];
					firstPageIds = freshIds;
				} catch {
					// Silently ignore SSE refresh failures.
				}
			}, 500);
		}
	});

	onDestroy(() => {
		unsubscribe();
	});

	let submitting: boolean = $state(false);

	// Posts a new comment. Throws on failure so CommentEditor preserves the
	// draft; clears itself on success.
	async function submitComment(body: string) {
		// Capture identity before the await so a mid-flight item switch can't
		// leak A's error into B's view or refresh B off A's mutation (TASK-2112).
		// `submitting` is a composer busy flag (not item-scoped load state), so
		// it's always cleared in finally — the switched-to composer must not
		// stay stuck spinning.
		const reqSlug = itemSlug;
		const reqWs = wsSlug;
		submitting = true;
		error = '';
		try {
			await api.comments.create(reqWs, reqSlug, {
				body,
				created_by: 'user',
				source: 'web'
			});
			if (reqSlug !== itemSlug || reqWs !== wsSlug) return;
			await loadTimeline();
		} catch (err: any) {
			if (reqSlug !== itemSlug || reqWs !== wsSlug) return;
			error = err?.message ?? 'Failed to post comment';
			throw err;
		} finally {
			submitting = false;
		}
	}

	async function handleReply(commentId: string, body: string) {
		const reqSlug = itemSlug;
		const reqWs = wsSlug;
		try {
			await api.comments.reply(reqWs, commentId, {
				body,
				created_by: 'user',
				source: 'web'
			});
			if (reqSlug !== itemSlug || reqWs !== wsSlug) return;
			await loadTimeline();
		} catch (err: any) {
			if (reqSlug !== itemSlug || reqWs !== wsSlug) return;
			error = err?.message ?? 'Failed to post reply';
			throw err; // let CommentEditor keep the draft
		}
	}

	// Edits a comment or reply (author/admin enforced server-side). Throws on
	// failure so the inline CommentEditor preserves the draft.
	async function handleEdit(commentId: string, body: string) {
		const reqSlug = itemSlug;
		const reqWs = wsSlug;
		try {
			await api.comments.update(reqWs, commentId, { body });
			if (reqSlug !== itemSlug || reqWs !== wsSlug) return;
			await loadTimeline();
		} catch (err: any) {
			if (reqSlug !== itemSlug || reqWs !== wsSlug) return;
			error = err?.message ?? 'Failed to edit comment';
			throw err;
		}
	}

	async function handleDelete(commentId: string) {
		if (!confirm('Delete this comment?')) return;
		const reqSlug = itemSlug;
		const reqWs = wsSlug;
		try {
			await api.comments.delete(reqWs, commentId);
			if (reqSlug !== itemSlug || reqWs !== wsSlug) return;
			await loadTimeline();
		} catch (err: any) {
			if (reqSlug !== itemSlug || reqWs !== wsSlug) return;
			error = err?.message ?? 'Failed to delete comment';
		}
	}

	async function handleReaction(commentId: string, emoji: string) {
		const reqSlug = itemSlug;
		const reqWs = wsSlug;
		try {
			await api.comments.addReaction(reqWs, commentId, emoji);
			if (reqSlug !== itemSlug || reqWs !== wsSlug) return;
			await loadTimeline();
		} catch (err: any) {
			if (reqSlug !== itemSlug || reqWs !== wsSlug) return;
			error = err?.message ?? 'Failed to add reaction';
		}
	}

	async function handleRemoveReaction(commentId: string, emoji: string) {
		const reqSlug = itemSlug;
		const reqWs = wsSlug;
		try {
			await api.comments.removeReaction(reqWs, commentId, emoji);
			if (reqSlug !== itemSlug || reqWs !== wsSlug) return;
			await loadTimeline();
		} catch (err: any) {
			if (reqSlug !== itemSlug || reqWs !== wsSlug) return;
			error = err?.message ?? 'Failed to remove reaction';
		}
	}

	function dotClass(kind: TimelineEntry['kind']): string {
		if (kind === 'comment') return 'dot-comment';
		if (kind === 'version') return 'dot-version';
		return 'dot-activity';
	}
</script>

<section class="timeline">
	<header class="timeline-header">
		<h3 class="timeline-title">Timeline</h3>
		{#if entries.length > 0}
			<span class="entry-count">{entries.length}{hasMore ? '+' : ''}</span>
		{/if}
	</header>

	<!-- Comment compose — gated on canEditItem (PLAN-1100 / TASK-1107) folded
	     with the master-freeze (`frozen` → canEdit=false, TASK-2172).
	     Read-only viewers / guests with view-only grants see the timeline
	     thread but cannot post; the composer is hidden entirely. Note: an
	     attachment upload the user STARTED in this composer before the pane
	     opened is orphaned when the composer unmounts on freeze — the ACCEPTED,
	     tracked BUG-2177 tradeoff (its upload bails via attachment-upload.ts's
	     view.isDestroyed check; no crash, no committed-content loss). -->
	{#if canEdit && showComposer}
		<div class="compose">
			<CommentEditor
				{wsSlug}
				{itemId}
				{hostToken}
				placeholder="Write a comment… (paste or drop an image to attach)"
				submitLabel="Comment"
				{submitting}
				onSubmit={submitComment}
			/>
		</div>
	{/if}

	{#if loading && entries.length === 0}
		<div class="loading">
			<span class="spinner"></span>
			<span class="loading-text">Loading timeline...</span>
		</div>
	{/if}

	{#if error}
		<div class="error">{error}</div>
	{/if}

	{#if !loading || entries.length > 0}
		<div class="entry-list" bind:this={entryListEl}>
			{#each visibleEntries as entry (entry.id)}
				<div class="entry">
					<div class="entry-rail">
						<span class="dot {dotClass(entry.kind)}"></span>
						<span class="line"></span>
					</div>
					<div class="entry-content">
						{#if entry.kind === 'comment' && entry.comment}
							<TimelineCommentCard
								comment={entry.comment}
								{wsSlug}
								{username}
								{items}
								{currentUserId}
								{canEdit}
								{frozen}
								{isAdmin}
								{hostToken}
								{attachmentResolver}
								onDelete={handleDelete}
								onReply={handleReply}
								onEdit={handleEdit}
								onReaction={handleReaction}
								onRemoveReaction={handleRemoveReaction}
							/>
						{:else if entry.kind === 'activity' && entry.activity}
							<TimelineActivityCard activity={entry.activity} />
						{:else if entry.kind === 'version' && entry.version}
							<TimelineVersionCard
								version={entry.version}
								{wsSlug}
								{itemSlug}
								{currentContent}
								{onRestore}
								{flushBeforeRestore}
								frozen={frozen || restoreFrozen}
							/>
						{/if}
					</div>
				</div>
			{/each}

			{#if entries.length === 0 && !loading}
				<div class="empty">No timeline entries yet.</div>
			{/if}
		</div>

		{#if hasMore}
			<button class="load-more-btn" type="button" disabled={loadingMore} onclick={loadMore}>
				{loadingMore ? 'Loading...' : 'Load more'}
			</button>
		{/if}
	{/if}
</section>

<!--
	Keyed per open (TASK-2431), the shape `AttachmentViewerHost` uses. `Lightbox`
	seeds its INDEX once through `untrack`, so replacing `lightbox` while a
	viewer is already up would reuse the instance and open the new set at the old
	position. (Its MIME filter is `$derived` and would re-answer on its own —
	the index is what cannot.) Nothing reaches that state today, the open viewer
	being inert over everything that could cause it, which is why this belongs in
	the structure rather than resting on a fact about the current UI.

	Accepted cost, recorded: a keyed block DESTROYS the old instance before
	creating the new one, so a viewer→viewer swap briefly releases the last
	backdrop lease (un-inerting the app) and runs the old viewer's focus restore
	before the new one takes focus. That is the same transient
	`AttachmentViewerHost` has carried since TASK-2428, it is unreachable from
	the UI (the open viewer inerts every control that could trigger it), and the
	alternative — a reused instance showing a stale set — is the worse of the
	two. Only a viewer→NULL→viewer sequence happens in practice, where the
	release is meant to happen anyway.
-->
{#key lightbox}
	{#if lightbox}
		<Lightbox
			images={lightbox.images}
			index={lightbox.index}
			{wsSlug}
			invoker={lightbox.invoker}
			{mutationsEnabled}
			{getItemContent}
			{getLiveContent}
			onClose={() => (lightbox = null)}
		/>
	{/if}
{/key}

<style>
	.timeline {
		display: flex;
		flex-direction: column;
		gap: var(--space-4);
	}

	.timeline-header {
		display: flex;
		align-items: center;
		gap: var(--space-2);
	}

	.timeline-title {
		margin: 0;
		font-size: 1em;
		font-weight: 600;
		color: var(--text-primary);
	}

	.entry-count {
		display: inline-flex;
		align-items: center;
		justify-content: center;
		min-width: 1.5em;
		padding: 0 var(--space-1);
		background: var(--bg-tertiary);
		border-radius: 9999px;
		font-size: 0.75em;
		font-weight: 600;
		color: var(--text-muted);
		line-height: 1.6;
	}

	/* ── Compose ──────────────────────────────────────────────────────────── */

	.compose {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
	}

	/* ── Loading / Error ──────────────────────────────────────────────────── */

	.loading {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		padding: var(--space-4);
		justify-content: center;
		color: var(--text-muted);
	}

	.spinner {
		display: inline-block;
		width: 16px;
		height: 16px;
		border: 2px solid var(--border);
		border-top-color: var(--accent-blue);
		border-radius: 50%;
		animation: spin 0.6s linear infinite;
	}

	@keyframes spin {
		to {
			transform: rotate(360deg);
		}
	}

	.loading-text {
		font-size: 0.85em;
	}

	.error {
		padding: var(--space-2) var(--space-3);
		background: color-mix(in srgb, var(--accent-red) 10%, transparent);
		border: 1px solid color-mix(in srgb, var(--accent-red) 30%, transparent);
		border-radius: var(--radius);
		color: var(--accent-red);
		font-size: 0.85em;
	}

	/* ── Timeline entries ─────────────────────────────────────────────────── */

	.entry-list {
		display: flex;
		flex-direction: column;
	}

	.entry {
		display: flex;
		gap: var(--space-3);
	}

	.entry-rail {
		display: flex;
		flex-direction: column;
		align-items: center;
		flex-shrink: 0;
		width: 16px;
		padding-top: var(--space-2);
	}

	.dot {
		width: 10px;
		height: 10px;
		border-radius: 50%;
		flex-shrink: 0;
		z-index: 1;
	}

	.dot-comment {
		background: var(--accent-blue);
	}

	.dot-activity {
		background: var(--text-muted);
	}

	.dot-version {
		background: var(--accent-green);
	}

	.line {
		width: 1px;
		flex: 1;
		background: var(--border);
	}

	.entry:last-child .line {
		display: none;
	}

	.entry-content {
		flex: 1;
		min-width: 0;
		padding-bottom: var(--space-3);
	}

	.empty {
		text-align: center;
		padding: var(--space-6);
		color: var(--text-muted);
		font-size: 0.9em;
	}

	.load-more-btn {
		display: block;
		width: 100%;
		padding: var(--space-2) var(--space-4);
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		color: var(--text-muted);
		font-size: 0.85em;
		font-weight: 500;
		cursor: pointer;
		text-align: center;
	}

	.load-more-btn:hover:not(:disabled) {
		color: var(--text-primary);
		border-color: var(--accent-blue);
	}

	.load-more-btn:disabled {
		opacity: 0.5;
		cursor: not-allowed;
	}
</style>
