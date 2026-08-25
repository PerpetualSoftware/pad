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
	import TimelineStructuredCard from './TimelineStructuredCard.svelte';
	import { attachmentRefsIn } from '$lib/utils/commentAttachments';
	import {
		fetchAttachmentMetadata,
		revalidateAttachmentMetadata,
		invalidateAttachmentMetadata
	} from '$lib/components/editor/attachment-metadata';
	import { attachmentDownloadUrl, type AttachmentMeta } from '$lib/markdown/attachments';
	import { canOpenInViewer } from '$lib/attachments/display';
	import { isEditorOwnedImage } from '$lib/attachments/editorOwnedImage';
	// One declaration of the surface set's image shape, on the channel (TASK-2431).
	import {
		notifyAttachmentSurfaceOpen,
		registerAttachmentDeletionListener,
		type LightboxImage
	} from '$lib/attachments/events';
	import { viewIdentity, createPaintFence } from '$lib/attachments/viewFence';
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
		visibleKinds?: Array<'comment' | 'activity' | 'version' | 'note' | 'decision'>;
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
		 * PLAN-2392 3c-iii U1 (TASK-2510). The archive LEVEL of the item whose
		 * `ItemDetail` owns this timeline, threaded from `ItemDetail` exactly as the
		 * attachment surface host takes it (DR-14). Two jobs:
		 *  - LEVEL: while true, every attachment probe goes through the no-store
		 *    revalidation primitive, so an archived parent's genuine 404 lands as
		 *    "missing" and a stale cached `ok` cannot repaint a broken `<img>`.
		 *  - EDGES: false→true drops this item's cached attachment metadata + probe
		 *    marks (so a re-render degrades to the missing path, not a broken image)
		 *    and bumps the lifecycle epoch; true→false re-probes the unresolved set
		 *    no-store so a restore escapes a `missing` cached while archived.
		 * Item scoping is inherent to the prop — no SSE filter changes here, which is
		 * what lets it cover BULK archive/restore (whose `items_bulk_updated` carries
		 * no item_id). Defaults false → byte-identical for every existing caller.
		 */
		parentArchived?: boolean;
	}

	let { wsSlug, username = '', itemSlug, currentContent, items = [], onRestore, itemId, collectionId, frozen = false, restoreFrozen = false, flushBeforeRestore, visibleKinds, hostToken = '', parentArchived = false }: Props = $props();

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
	// Server-supplied position of the next page; see cursorFrom (BUG-2765).
	let nextCursor: { before: string; before_id: string } | null = $state(null);
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

	// UUIDs whose last probe did NOT yield metadata — a genuine `missing` (404)
	// or a `transient` failure. Tracked EXPLICITLY because `probed` is
	// non-reactive and a `missing` writes nothing to `attMeta`, so "clear a Set"
	// alone would re-probe nothing on restore (PLAN-2392 3c-iii U1). The restore
	// edge re-probes exactly this set.
	const unresolved = new Set<string>();

	// Deleted UUIDs — the deletion bus's authoritative, latched fact. A HEAD
	// that resolves `ok` AFTER the delete must not repopulate `attMeta`, and the
	// tombstone covers EVERY deleted id, not only ones already cached: a delayed
	// probe for a not-yet-populated id must refuse to write too. Per-id, NOT the
	// epoch — a single deletion must not fence the OTHER attachments' in-flight
	// probes (PLAN-2392 3c-iii U1).
	const tombstoned = new Set<string>();

	// The per-timeline LIFECYCLE epoch — a plain `let` (non-reactive: read at
	// probe dispatch, checked before write). Bumped by BOTH archive/restore prop
	// edges. `revalidateAttachmentMetadata` invalidates the shared cache map but
	// cannot cancel an already-dispatched promise, so a pre-archive `ok` or a
	// pre-restore `missing` could otherwise overwrite the post-edge state — the
	// epoch fence refuses any authoritative result whose dispatch epoch is stale.
	let lifecycleEpoch = 0;

	// Reactive re-probe trigger. The probe effect tracks this; the restore edge
	// bumps it so the effect re-runs for exactly the unresolved set (clearing the
	// non-reactive `probed` Set alone re-probes nothing — the effect tracks only
	// `entries`).
	let probeNonce = $state(0);

	// Set true by the restore edge just before it bumps `probeNonce`, consumed
	// (and reset) by the probe effect's next run: the restore re-probe must be
	// no-store to ESCAPE a `missing` cached while the parent was archived, even
	// though `parentArchived` is already false again by then. Plain `let`,
	// read/written only under `untrack` — never a tracked-scope self-write.
	let pendingRestoreNoStore = false;

	// The attachment UUIDs referenced by the current comment/reply bodies. One
	// definition, shared by the probe effect and the archive/restore edges so
	// each targets exactly this item's attachments.
	function referencedAttachmentIds(): Set<string> {
		const ids = new Set<string>();
		for (const entry of entries) {
			if (entry.kind !== 'comment' || !entry.comment) continue;
			for (const id of attachmentRefsIn(entry.comment.body)) ids.add(id);
			for (const reply of entry.comment.replies ?? []) {
				for (const id of attachmentRefsIn(reply.body)) ids.add(id);
			}
		}
		return ids;
	}

	function probeAttachment(uuid: string, noStore: boolean) {
		if (probed.has(uuid)) return;
		probed.add(uuid);
		// Capture the workspace identity AND the lifecycle epoch before the HEAD.
		// `attMeta` is a workspace-scoped attachment cache; ItemDetail reuses this
		// panel across a no-{#key} item switch (its wsSlug/itemSlug props just
		// change), so a probe resolving after a workspace switch must NOT write the
		// old workspace's metadata into the new item's cache (TASK-2112). The epoch
		// is the same discipline for archive/restore (PLAN-2392 3c-iii U1).
		const reqWs = wsSlug;
		const reqEpoch = lifecycleEpoch;
		// LEVEL rule: while the parent is archived (or forced by a restore edge),
		// bypass the shared module cache with a no-store revalidation so a stale
		// cached `ok` cannot repaint a broken `<img>` and a genuine 404 lands as
		// missing honestly. Otherwise the plain, cache-sharing HEAD.
		const urlFor = (id: string, variant?: 'thumb-sm' | 'thumb-md' | 'original') =>
			attachmentDownloadUrl(wsSlug, id, variant);
		const probe = noStore
			? revalidateAttachmentMetadata(wsSlug, uuid, urlFor, { cache: 'no-store' })
			: fetchAttachmentMetadata(wsSlug, uuid, urlFor);
		probe.then((m) => {
			// A delete (tombstone) or a workspace switch makes this answer
			// irrelevant — ignore it entirely.
			if (tombstoned.has(uuid)) return;
			if (reqWs !== wsSlug) return;
			// A transient failure (5xx / network) is not evidence about the row,
			// and the helper deliberately doesn't cache it — so drop the probed
			// mark, leaving the attachment eligible again on the NEXT run of the
			// probe effect (a new comment, an edit, a remount, a restore bump). It
			// does not schedule a retry of its own (PLAN-2392 DR-17).
			if (m.status === 'transient') {
				probed.delete(uuid);
				unresolved.add(uuid);
				return;
			}
			// An authoritative ok/missing that lands AFTER a lifecycle edge must
			// not overwrite the post-edge state (the epoch fence).
			if (reqEpoch !== lifecycleEpoch) return;
			if (m.status !== 'ok') {
				// `missing` is authoritative: latch it unresolved and clear any
				// stale entry so the renderer degrades to the missing placeholder.
				unresolved.add(uuid);
				if (attMeta.has(uuid)) {
					const next = new Map(attMeta);
					next.delete(uuid);
					attMeta = next;
				}
				return;
			}
			unresolved.delete(uuid);
			const next = new Map(attMeta);
			// filename is left empty — the markdown alt text is the chip/img
			// label, and renderAttachmentImage only falls back to filename
			// when alt is blank.
			next.set(uuid, { id: uuid, mime_type: m.mime, filename: '', size_bytes: m.size });
			attMeta = next;
		});
	}

	// Probe every attachment referenced by a comment or reply as the timeline
	// loads / changes. Tracks `entries` (new/edited comments) and `probeNonce`
	// (the restore re-probe trigger). `parentArchived` is read NON-reactively:
	// the LEVEL is a dispatch-time decision, and the archive/restore EDGES are
	// owned by the dedicated edge effect below — reading it reactively here would
	// re-run (and needlessly re-probe) on every lifecycle flip.
	$effect(() => {
		void probeNonce;
		const ids = referencedAttachmentIds();
		untrack(() => {
			const noStore = parentArchived === true || pendingRestoreNoStore;
			pendingRestoreNoStore = false;
			for (const id of ids) probeAttachment(id, noStore);
		});
	});

	// External attachment deletion (the app-wide bus, PLAN-2382). Drop the cached
	// entry AND its probe mark so the comment-HTML renderer re-renders the
	// reference through the missing path, and TOMBSTONE the id so an in-flight
	// HEAD that resolves `ok` after the delete can't repopulate it. Self-broadcast
	// is idempotent (the deleting surface already reconciled).
	$effect(() => {
		return registerAttachmentDeletionListener((uuid) => {
			if (!uuid) return;
			tombstoned.add(uuid);
			probed.delete(uuid);
			unresolved.delete(uuid);
			if (attMeta.has(uuid)) {
				const next = new Map(attMeta);
				next.delete(uuid);
				attMeta = next;
			}
		});
	});

	// Archive/restore LIFECYCLE edges (PLAN-2392 3c-iii U1 / DR-14). Threaded as a
	// PROP from ItemDetail rather than an SSE subscription: the bulk
	// archive/restore endpoints emit `items_bulk_updated` with NO item_id (routed
	// only to sync_required), which a timeline-side item_archived/item_restored
	// filter would miss — but ItemDetail reconciles `deleted_at` through BOTH the
	// per-item events and the bulk sync path, and passes the settled level down.
	// Edge-triggered off a LATCHED previous value (plain `let` + untrack — a
	// $state read+written in one effect aborts its flush, CONVE-1688). Seeded from
	// the initial prop so a mount on an already-archived item is a LEVEL, not an
	// edge (the LEVEL rule in the probe effect covers it).
	let prevArchived = untrack(() => parentArchived === true);
	$effect(() => {
		const archived = parentArchived === true;
		untrack(() => {
			if (archived === prevArchived) return;
			prevArchived = archived;
			lifecycleEpoch += 1;
			// Every id this timeline holds ANY state for — referenced by the current
			// comments, cached in `attMeta`, probed, or latched unresolved. BOTH edges
			// reconcile over this whole set, not just the currently-referenced ids: an
			// attachment resolved-then-unreferenced (or a probe still in flight) keeps
			// `attMeta`/`probed` state that a later re-reference would otherwise replay
			// — a stale `ok` painting a broken `<img>` against an archived parent, or a
			// stale probe mark skipping the restore re-probe (Codex rounds 1-2, the
			// symmetric P2). All of `attMeta`/`probed`/`unresolved` describe THIS
			// timeline instance's own probes (UUIDs are globally unique), so the union
			// never misattributes an entry to another item. After a no-{#key} workspace
			// switch the union may still carry a PRIOR workspace's id (attMeta persists
			// across the switch by design); discarding it from THIS instance's caches is
			// benign (it re-probes on switch-back), and the shared-cache invalidate below
			// keys on the CURRENT `wsSlug`, so a foreign id hits a `wsSlug:id` key that
			// never exists — a no-op that correctly leaves the other workspace's (still
			// valid) cache entry untouched.
			const referenced = referencedAttachmentIds();
			const tracked = new Set<string>(referenced);
			for (const id of attMeta.keys()) tracked.add(id);
			for (const id of probed) tracked.add(id);
			for (const id of unresolved) tracked.add(id);
			if (archived) {
				// false→true: a cached `ok` renders a plain `<img>` with no error bridge
				// to the missing presentation, so ANY tracked entry against the archived
				// parent would show a BROKEN image on (re-)render. Drop every cached
				// entry + probe mark and invalidate the shared cache so a re-render /
				// re-reference goes through the no-store probe → missing path honestly.
				// Already-painted thumbs keep their decoded bytes until re-render (the
				// DR-14 posture); no eager re-probe.
				for (const id of tracked) {
					probed.delete(id);
					unresolved.add(id);
					invalidateAttachmentMetadata(wsSlug, id);
				}
				// `tracked` ⊇ every `attMeta` key, so this drops exactly the tracked
				// cached entries — i.e. all of them.
				if (attMeta.size) attMeta = new Map();
			} else {
				// true→false: re-probe every NOT-yet-resolved tracked id NO-STORE so a
				// `missing` cached while archived is escaped; leave resolved `ok` entries
				// alone (they survive archive, and re-probing them would be a redundant
				// HEAD). Clearing the probe mark + invalidating for a not-currently-
				// referenced id too is what lets a re-reference AFTER the restore
				// re-probe instead of skipping on a stale probe mark / stale cached
				// `missing` (Codex rounds 1-2). The probe effect iterates only
				// CURRENTLY-referenced ids, so bump the reactive nonce (which forces this
				// run no-store) when a referenced id needs re-probing; the rest re-probe
				// when they are next referenced.
				let bump = false;
				for (const id of tracked) {
					if (attMeta.has(id)) continue;
					probed.delete(id);
					unresolved.add(id);
					invalidateAttachmentMetadata(wsSlug, id);
					if (referenced.has(id)) bump = true;
				}
				if (bump) {
					pendingRestoreNoStore = true;
					probeNonce += 1;
				}
			}
		});
	});

	let entryListEl: HTMLElement | undefined = $state();

	// STALE-ACTIVATION FENCE (T4a, TASK-2489). The timeline is reused across a
	// no-`{#key}` item / workspace switch, so a thumbnail activated after a switch
	// must not emit against the new view. `paint` records the (ws, item) whose
	// thumbnails are on screen — re-recorded only when `entries` actually change
	// (a bare view change without a reload keeps the old paint), so `isCurrent()`
	// at emit refuses a stale click. The captured `workspaceSlug` on the event is
	// the same DR-16 protection carried onto the wire.
	const timelineView = viewIdentity(() => ({ ws: wsSlug, item: itemSlug }));
	const paint = createPaintFence(timelineView);
	$effect(() => {
		void entries;
		// Capture the view NON-reactively: this must fire only when the rendered
		// entries change (a current load set them), never on a bare `wsSlug` change
		// that has not yet reloaded — that is the window a stale click lives in.
		untrack(() => paint.record(timelineView.capture()));
	});

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
		// ENTRY fence (T4a): a thumbnail activated after a view switch must not emit.
		if (!paint.isCurrent()) return false;
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
		// Emit on the surface channel (T4a) — the host owns the mount. The set + its
		// index ARE the capture (the notify snapshots them); `workspaceSlug` is
		// captured here so the host serves this view's images from this workspace.
		const target = list[index];
		notifyAttachmentSurfaceOpen({
			attachmentId: clickedId,
			workspaceSlug: wsSlug,
			itemId: itemId ?? '',
			hostToken,
			images: list,
			index,
			invoker: imgEl,
			filename: target.filename,
			mime_type: target.mime_type,
			size_bytes: target.size_bytes,
		});
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
			nextCursor = cursorFrom(resp, resp.entries);
			firstPageIds = new Set(resp.entries.map((e) => e.id));
		} catch (err: any) {
			if (reqSlug !== itemSlug || reqWs !== wsSlug) return;
			error = err?.message ?? 'Failed to load timeline';
		} finally {
			if (reqSlug === itemSlug && reqWs === wsSlug) loading = false;
		}
	}

	/**
	 * Where the next page starts. The SERVER decides this (BUG-2765): it
	 * over-fetches per source and drops rows that cannot render, so a page can
	 * carry fewer entries than the rows it consumed — or none, with more
	 * history behind them. Deriving the cursor from the last rendered entry
	 * then either cannot be done (an empty page) or does not advance (a page
	 * whose rows all dropped), and the same window is requested forever.
	 *
	 * The fallback to the last entry is for a server that predates the field;
	 * it is exactly what this component did before, including its wedge.
	 */
	function cursorFrom(
		resp: TimelineResponse,
		known: TimelineEntry[]
	): { before: string; before_id: string } | null {
		if (!resp.has_more) return null;
		if (resp.next_before && resp.next_before_id) {
			return { before: resp.next_before, before_id: resp.next_before_id };
		}
		const last = known[known.length - 1];
		return last ? { before: last.created_at, before_id: last.id } : null;
	}

	/**
	 * How many pages one press of Load More may walk through while every row
	 * comes back droppable. This is a UX bound, not the fix: the cursor above
	 * is what makes paging complete, and a single hop is already correct. It
	 * exists so a user crossing a long run of `read` activity sees entries
	 * appear rather than a spinner and nothing, and it is small so a
	 * pathological item cannot turn one click into an unbounded request fan.
	 */
	const MAX_EMPTY_HOPS = 5;

	async function loadMore() {
		if (loadingMore || !nextCursor) return;
		// Capture identity before the await so a switch mid-flight can't append
		// A's older page onto B's entries (TASK-2112).
		const reqSlug = itemSlug;
		const reqWs = wsSlug;
		loadingMore = true;
		try {
			for (let hop = 0; hop < MAX_EMPTY_HOPS && nextCursor; hop++) {
				const cursor = nextCursor;
				const resp: TimelineResponse = await api.timeline.list(reqWs, reqSlug, {
					before: cursor.before,
					before_id: cursor.before_id
				});
				if (reqSlug !== itemSlug || reqWs !== wsSlug) return;
				// Deduplicate by ID to handle boundary overlap from <= queries,
				// and because the server's cursor can deliberately re-cover rows
				// an earlier page already rendered (see cursorFrom).
				const existingIds = new Set(entries.map((e) => e.id));
				const newEntries = resp.entries.filter((e) => !existingIds.has(e.id));
				// Merge, do not concatenate. The server's cursor deliberately
				// re-covers ground when one source's window ran out before
				// another's, so a later page can carry entries NEWER than the
				// oldest one already shown — appending those would print them
				// below it (codex round 1). Same order the server sorts by:
				// created_at descending, id descending as the tie-break.
				//
				// Compared as INSTANTS, not as strings: these timestamps do
				// not all carry the same precision — the store writes whole
				// seconds, but a structured note or decision can carry a
				// hand-written sub-second one — and lexicographically
				// "…:05.123Z" sorts BEFORE "…:05Z", which would place the more
				// precise entry an hour's worth of rows away from where it
				// belongs.
				entries = [...entries, ...newEntries].sort((a, b) => {
					const at = new Date(a.created_at).getTime();
					const bt = new Date(b.created_at).getTime();
					if (at !== bt) return bt - at;
					return a.id < b.id ? 1 : a.id > b.id ? -1 : 0;
				});
				hasMore = resp.has_more;
				nextCursor = cursorFrom(resp, entries);
				// Stop as soon as the press produced something to look at.
				if (newEntries.length > 0) break;
			}
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
	$effect(() => {
		void wsSlug;
		void itemSlug;
		// A→B LIFECYCLE (TASK-2431 / T4a). This component is reused across an item /
		// workspace switch (no `{#key}`). The open surface is the HOST's now, and it
		// closes on the resource switch (T4a), so the timeline no longer clears a
		// `lightbox` of its own — a stale click is refused by the `paint` fence at
		// emit, and a stale OPEN is closed by the host.
		//
		// `attMeta` is deliberately NOT cleared on the switch. It is keyed by a bare
		// uuid where the shared HEAD cache is keyed `ws:uuid`, which looks like a
		// workspace-scoped cache leaking across the switch — but an attachment id is
		// a UUID belonging to exactly one workspace, so an entry can only ever answer
		// for the id it describes. Clearing it would buy nothing and cost something
		// real: the probe effect refills it only when `entries` next changes, so a
		// FAILED load after the switch would leave every already-rendered image
		// permanently un-openable for the rest of the mount (Codex round 3).
		loadTimeline();
	});

	// Only refresh the timeline for comment/reaction events — NOT item_updated.
	// Content saves create version-diff entries that appear on next natural
	// refresh (new comment, page load). Refreshing on every content save caused
	// visible shakiness and rate-limit errors from rapid SSE replay.
	//
	// The `note` / `decision` kinds (BUG-2301) inherit this deliberately. They
	// are written by `pad item note` / `pad item decide`, which PATCH the item
	// and so emit `item_updated` — an open Activity tab will not show a new one
	// until the next natural refresh. That is the same staleness version
	// entries have always had, and these kinds have no web writer at all, so no
	// user performs the action and then waits on this view. Flagged twice in
	// review and declined twice: admitting `item_updated` here would trade a
	// bounded, documented staleness for the shakiness and rate-limit errors
	// this exclusion exists to prevent.
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
	/** Backoff before the single retry a failed SSE refresh gets (BUG-2508). */
	const SSE_REFRESH_RETRY_MS = 2000;
	/** Set by onDestroy; checked by every continuation that outlives the mount. */
	let destroyed = false;

	// Named rather than inline so the failure path can re-invoke it once. The
	// `isRetry` flag is what bounds that to ONE extra attempt.
	async function refreshFromSSE(isRetry = false) {
		if (destroyed) return;
		// Capture identity before the await — this same panel instance
		// serves the next item after a no-{#key} switch, so a debounced
		// refresh resolving late must not merge A's entries into B (TASK-2112).
		const reqSlug = itemSlug;
		const reqWs = wsSlug;
		try {
			const resp: TimelineResponse = await api.timeline.list(reqWs, reqSlug);
			if (destroyed) return;
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
		} catch (err) {
			// A failed SSE-driven refresh is not fatal — the timeline keeps
			// showing what it already has — but silently dropping it leaves
			// the panel quietly missing a comment somebody else just posted,
			// with nothing indicating it and NO RETRY: the next refresh only
			// comes with the next relevant SSE event, which may never arrive
			// (BUG-2508).
			//
			// So log it, and retry ONCE on a longer delay. Deliberately not an
			// `error` banner: this is a background refresh, and a
			// modal-weight failure surface for it would be worse than the bug
			// it reports. Deliberately not unbounded retries either — the
			// debounce exists because SSE replay can hammer this endpoint, and
			// a retry loop would reintroduce exactly that.
			if (destroyed) return;
			if (reqSlug !== itemSlug || reqWs !== wsSlug) return;
			console.error('[timeline] SSE-driven refresh failed', err);
			if (isRetry) return;
			clearTimeout(sseRefreshTimer);
			sseRefreshTimer = setTimeout(() => void refreshFromSSE(true), SSE_REFRESH_RETRY_MS);
		}
	}

	const unsubscribe = sseService.onItemEvent((event) => {
		if (relevantEvents.has(event.type)) {
			clearTimeout(sseRefreshTimer);
			sseRefreshTimer = setTimeout(() => void refreshFromSSE(), 500);
		}
	});

	onDestroy(() => {
		unsubscribe();
		// The debounce timer AND the retry timer both live in `sseRefreshTimer`,
		// and a rejected request can schedule a retry from its own catch AFTER
		// teardown — the identity fence (reqSlug/reqWs) is not a teardown fence,
		// since a remounted panel can legitimately hold the same identity. Clear
		// the timer and latch `destroyed` so neither a pending debounce nor a late
		// failure can fire into a dead component (found in review of BUG-2508).
		destroyed = true;
		clearTimeout(sseRefreshTimer);
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
		if (kind === 'note') return 'dot-note';
		if (kind === 'decision') return 'dot-decision';
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
						<!-- No `&& entry.note` guard, unlike the kinds above: the card is
						     null-safe, and a payload-less entry still occupies a rail. Requiring
						     the payload turns a partial entry into a blank rail with no card at
						     all, which reads as a rendering fault rather than as a thin entry. -->
						{:else if entry.kind === 'note'}
							<TimelineStructuredCard
								kind="note"
								note={entry.note}
								actor={entry.actor}
								createdAt={entry.created_at}
							/>
						{:else if entry.kind === 'decision'}
							<TimelineStructuredCard
								kind="decision"
								decision={entry.decision}
								actor={entry.actor}
								createdAt={entry.created_at}
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
	The timeline no longer mounts `Lightbox` directly (T4a, TASK-2489). An activated
	thumbnail emits on the surface channel (`openLightboxFromImg`) and the ONE
	`AttachmentSurfaceHost` owns the mount, the keyed-per-open remount and the
	lifecycle.
-->

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

	.dot-note {
		background: var(--accent-cyan);
	}

	.dot-decision {
		background: var(--accent-orange);
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
