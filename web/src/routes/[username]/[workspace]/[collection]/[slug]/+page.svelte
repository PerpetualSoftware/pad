<script lang="ts">
	import { page } from '$app/state';
	import { tick, onMount, onDestroy } from 'svelte';
	import { api, PadApiError, type ImportURLResponse } from '$lib/api/client';
	import { confirmOpenChildrenOrThrow, isOpenChildrenError } from '$lib/items/openChildrenError';
	import { marked } from 'marked';
	import { collectionStore } from '$lib/stores/collections.svelte';
	import { syncService } from '$lib/services/sync.svelte';
	import { sseService } from '$lib/services/sse.svelte';
	import Editor from '$lib/components/editor/Editor.svelte';
	import EditorBubbleMenu from '$lib/components/editor/EditorBubbleMenu.svelte';
	import EditorLinkPopover from '$lib/components/editor/EditorLinkPopover.svelte';
	import RawMarkdownEditor from '$lib/components/editor/RawMarkdownEditor.svelte';
	import type { Editor as EditorType } from '@tiptap/core';
	import * as Y from 'yjs';
	import { CollabProvider, type CollabConnectionState } from '$lib/collab/wsProvider.svelte';
	import { userColor } from '$lib/collab/cursorColor';
	import { authStore } from '$lib/stores/auth.svelte';
	import FieldEditor from '$lib/components/fields/FieldEditor.svelte';
	import TagInput from '$lib/components/fields/TagInput.svelte';
	import ItemTimeline from '$lib/components/timeline/ItemTimeline.svelte';
	import ChildItems from '$lib/components/ChildItems.svelte';
	import BacklinksPanel from '$lib/components/BacklinksPanel.svelte';
	import { goto } from '$app/navigation';
	import { relativeTime, wikiLinksToMarkdown, markdownToWikiLinks, cleanBrokenLinks, unescapeDocLinks } from '$lib/utils/markdown';
	import { toastStore } from '$lib/stores/toast.svelte';
	import { editorStore } from '$lib/stores/editor.svelte';
	import type { Item, Collection, CollectionSettings, QuickAction, ItemLink, AgentRole } from '$lib/types';
	import { parseFields, parseSchema, parseSettings, parseTags, formatItemRef, getTerminalOptions } from '$lib/types';
	import QuickActionsMenu from '$lib/components/common/QuickActionsMenu.svelte';
	import BottomSheet from '$lib/components/common/BottomSheet.svelte';
	import ContentSkeleton from '$lib/components/common/ContentSkeleton.svelte';
	import ContentError from '$lib/components/common/ContentError.svelte';
	import EditCollectionModal from '$lib/components/collections/EditCollectionModal.svelte';
	import ShareDialog from '$lib/components/ShareDialog.svelte';
	import { copyToClipboard } from '$lib/utils/clipboard';
	import { starredStore } from '$lib/stores/starred.svelte';
	import { titleStore } from '$lib/stores/title.svelte';
	import { workspaceStore } from '$lib/stores/workspace.svelte';
	import { createScrollRestoration } from '$lib/scroll/restore.svelte';

	type RelationshipEntry = {
		key: string;
		label: string;
		href: string | null;
		status?: string;
		linkId?: string;
	};

	type RelationshipGroup = {
		label: string;
		tone: 'default' | 'blocks' | 'wiki' | 'lineage';
		entries: RelationshipEntry[];
		closureSummary?: string;
	};

	let wsSlug = $derived(page.params.workspace ?? '');
	let username = $derived(page.params.username ?? '');
	let collSlug = $derived(page.params.collection ?? '');
	let itemSlug = $derived(page.params.slug ?? '');

	let item = $state<Item | null>(null);
	let collection = $state<Collection | null>(null);
	let loading = $state(true);
	let error = $state('');

	// ── Scroll position restoration (BUG-1425) ─────────────────────────
	// Item detail pages fetch content async (loadData), so SvelteKit's
	// built-in scroll restoration runs against a still-skeleton document
	// and clamps the saved offset to ~0. The helper parks the snapshot
	// value until ready() is true, then double-RAFs and scrolls.
	//
	// `ready` requires the loaded item's identity to match the URL
	// param — otherwise a same-instance route change (item A →
	// wiki-link to B → back to A) would fire the restore against B's
	// still-rendered content. The match accepts EITHER the slug form
	// OR the issue ref (e.g. `TASK-123`) because `itemUrlId()` (in
	// `$lib/types/index.ts`) prefers refs over slugs when building
	// links, so most app URLs are `/tasks/TASK-123` rather than
	// `/tasks/some-slug`. Without the ref alternative, `ready()`
	// would never become true for ref URLs and the restore would
	// hang forever. Codex BUG-1425 round 5 P1.
	const scrollRestoration = createScrollRestoration({
		ready: () =>
			!loading &&
			item !== null &&
			(item.slug === itemSlug ||
				`${item.collection_prefix}-${item.item_number}` === itemSlug),
		persistKey: () =>
			wsSlug ? `pad-last-scroll-${wsSlug}-${page.url.pathname}` : null,
	});
	export const snapshot = scrollRestoration.snapshot;

	let editorInstance = $state<EditorType | null>(null);

	let editingTitle = $state(false);
	let titleDraft = $state('');
	let titleInputEl = $state<HTMLTextAreaElement>();

	let fields = $derived<Record<string, any>>(item ? parseFields(item) : {});
	// Tags live on item.tags (a JSON-array string), NOT in the schema.
	// parseTags is defensive (tolerates empty/garbage) and dedupes
	// case-insensitively so rendering keys stay unique.
	let tags = $derived(parseTags(item));
	let tagSuggestions = $state<string[]>([]);
	let schema = $derived(collection ? parseSchema(collection) : { fields: [] });
	let settings = $derived<CollectionSettings>(collection ? parseSettings(collection) : { layout: 'balanced', default_view: 'list' });
	let layout = $derived(settings.layout);
	let quickActions = $derived<QuickAction[]>(settings.quick_actions ?? []);

	// Convert wiki-links to markdown links for the editor
	let editorContent = $derived.by(() => {
		if (!item) return '';
		const raw = item.content ?? '';
		const allItems = collectionStore.items ?? [];
		if (allItems.length > 0 && raw.includes('[[')) {
			return wikiLinksToMarkdown(raw, allItems, wsSlug, username);
		}
		return raw;
	});

	let contentDebounceTimer: ReturnType<typeof setTimeout> | undefined;
	let saveStatus = $state<'idle' | 'saving' | 'saved'>('idle');
	let saveStatusTimer: ReturnType<typeof setTimeout> | undefined;
	let confirmDelete = $state(false);
	let deleting = $state(false);
	let rawMode = $state(false);
	let showMoveMenu = $state(false);
	let moving = $state(false);
	let itemLinks = $state<ItemLink[]>([]);
	let workspaceMembers = $state<{ user_id: string; user_name: string; user_email: string; role: string }[]>([]);
	let shareDialogOpen = $state(false);
	let editCollectionOpen = $state(false);
	let editCollectionSection = $state<'general' | 'fields' | 'display' | 'actions' | undefined>(undefined);
	let agentRoles = $state<AgentRole[]>([]);
	let childItemIds = $state<Set<string>>(new Set());
	let hasChildren = $state(false);
	let copied = $state(false);

	// PLAN-1593 / TASK-1596: count of inbound `[[...]]` references to
	// this item, surfaced as a mention badge near the title. Driven by
	// BacklinksPanel via its onCountChange callback so the badge and
	// panel stay consistent — when the panel loads zero rows it
	// notifies us and the badge hides.
	let backlinksCount = $state(0);

	// ── Viewport detection ───────────────────────────────────────────────
	// Drives BottomSheet vs popover branching for mobile-only surfaces on
	// this page (currently the "Move to…" menu). Matches the reference
	// pattern from QuickActionsMenu / EmojiPickerButton / ReactionPicker.
	let isMobile = $state(false);
	$effect(() => {
		if (typeof window === 'undefined') return;
		const mq = window.matchMedia('(max-width: 639.98px)');
		isMobile = mq.matches;
		const onChange = (e: MediaQueryListEvent) => {
			isMobile = e.matches;
		};
		mq.addEventListener('change', onChange);
		return () => mq.removeEventListener('change', onChange);
	});

	async function handleCopyRef() {
		const ref = formatItemRef(item!);
		if (!ref) return;
		const success = await copyToClipboard(ref);
		if (success) {
			copied = true;
			setTimeout(() => { copied = false; }, 1500);
		}
	}
	let relationshipGroups = $derived(item ? buildRelationshipGroups(item, itemLinks, childItemIds) : []);
	let codeContext = $derived(item?.code_context ?? null);
	// isOwner now comes from workspaceStore (PLAN-1100 / TASK-1101) — populated
	// by workspaceStore.setCurrent via the /me endpoint. workspaceMembers is
	// still loaded for the assignee dropdown (line ~947).
	let isOwner = $derived(workspaceStore.isOwner);
	// Per-item edit predicate (PLAN-1100 / TASK-1105). Mirrors the server's
	// ResolveUserPermission cascade: owner → item grant → collection grant
	// → role + visibility. Drives title / content / FieldEditor / delete /
	// status affordance gating below.
	let canEdit = $derived(item ? workspaceStore.canEditItem(item) : false);
	$effect(() => {
		if (wsSlug && collSlug && itemSlug) {
			loadData();
		}
	});

	// Surface the item's ref (e.g. IDEA-592) in the browser tab title.
	// Clear the section so the format reads "{REF} · {Workspace} · Pad".
	$effect(() => {
		titleStore.setPageTitle({
			section: null,
			item: item ? (formatItemRef(item) || null) : null,
		});
	});

	// Sync coordinator — refresh item data on tab resume
	let unsubscribeSync: (() => void) | null = null;
	let unsubscribeSSE: (() => void) | null = null;
	let unsubscribeBeforePrint: (() => void) | null = null;

	// Print header/footer state (PLAN-620 / TASK-623). Initialized on mount
	// and refreshed on `beforeprint` so the printed page reflects the
	// actual moment of print, not whenever the page last loaded.
	let printDate = $state('');
	let printUrl = $state('');

	function refreshPrintMeta() {
		if (typeof window === 'undefined') return;
		printDate = new Date().toLocaleDateString(undefined, {
			year: 'numeric',
			month: 'long',
			day: 'numeric'
		});
		printUrl = window.location.href;
	}

	onMount(() => {
		refreshPrintMeta();
		const handler = () => refreshPrintMeta();
		window.addEventListener('beforeprint', handler);
		unsubscribeBeforePrint = () => window.removeEventListener('beforeprint', handler);

		// Live SSE updates for THIS item's title / fields / archive state.
		// Mirrors the onSync handler below — same edit-conflict guards
		// (saveStatus / editingTitle) and same content-preservation
		// pattern (the editor owns content; replacing it would clobber
		// in-flight edits). The detail page previously had no live
		// subscription, so title/field changes from another client (or
		// session) didn't propagate until a manual refresh — TASK-1243.
		//
		// Comments, reactions, timeline events, and child-item updates
		// already have their own subscriptions inside CommentThread.svelte,
		// ItemTimeline.svelte, and ChildItems.svelte respectively — we
		// only handle item_updated / item_archived / item_restored for
		// the parent item itself here.
		//
		// KNOWN LIMITATION: live content-sync is intentionally NOT handled.
		// Replacing item.content while the editor is mounted would clobber
		// the user's in-flight document. A proper fix needs editor-dirty-
		// state integration; tracked separately.
		unsubscribeSSE = sseService.onItemEvent(async (event) => {
			if (!item || event.item_id !== item.id) return;

			// Archive is destructive and must NOT be gated by the
			// edit-conflict guard below — a user editing a since-archived
			// item should be redirected immediately. Their in-flight save
			// will fail against the archived row, and silently keeping
			// them on a non-existent item is worse than discarding the
			// edit. Per Codex review round 2.
			if (event.type === 'item_archived') {
				goto(`/${username}/${wsSlug}/${collSlug}`);
				return;
			}

			// Non-destructive updates: skip if the user is actively
			// editing the title or has a pending content save in flight.
			// They'll catch up on the next idle event (and the
			// syncService onTabResume path also covers anything missed).
			if (saveStatus === 'saving' || editingTitle) return;

			// Capture the item this event was scoped to *before* awaiting.
			// Otherwise a navigation that completes during the in-flight
			// request would let the resolved fetch clobber the new item
			// (TASK-754-style race guard, mirrored from loadData()).
			const reqItemId = item.id;
			const reqWsSlug = wsSlug;
			const reqItemSlug = itemSlug;

			switch (event.type) {
				case 'item_updated': {
					try {
						const updated = await api.items.get(reqWsSlug, reqItemSlug);
						// Bail if the user navigated away before this resolved.
						if (!item || item.id !== reqItemId) return;
						// Drop TASK-1243's conservative content-skip
						// when collab is active (TASK-1262). Under
						// collab the editor reads from Y.Doc, NOT
						// from the `content` prop — Editor.svelte's
						// `if (ydoc) return` gate at the prop $effect
						// makes adopting updated.content harmless to
						// the live editor while keeping item.content
						// fresh for downstream consumers (UI summaries,
						// search index hints, etc.).
						//
						// Non-collab still preserves the local content
						// to avoid clobbering an unsaved typing burst —
						// the saveStatus guard above already skips when
						// a debounced save is in flight, but a user
						// mid-keystroke with no save yet pending would
						// still lose chars without this branch.
						item = adoptServerItem(updated);
						const links = await api.links.list(reqWsSlug, updated.slug).catch(() => []);
						if (!item || item.id !== reqItemId) return;
						itemLinks = links;
					} catch {
						// Ignore — will catch up on next event
					}
					break;
				}
				case 'item_restored': {
					try {
						const updated = await api.items.get(reqWsSlug, reqItemSlug);
						if (!item || item.id !== reqItemId) return;
						item = adoptServerItem(updated);
					} catch {
						// Ignore — will catch up on next event
					}
					break;
				}
			}
		});

		unsubscribeSync = syncService.onSync(async (result) => {
			if (!wsSlug || !itemSlug || !item) return;

			if (result.type === 'caught_up') return;

			// Deletion is destructive and must run even if the user is
			// editing — same reasoning as the SSE handler's archive case.
			// Check this BEFORE the edit-conflict guard so a deleted item
			// doesn't sit there gated by an in-flight save.
			if (result.type === 'incremental' && result.changes.deleted.includes(item.id)) {
				goto(`/${username}/${wsSlug}/${collSlug}`);
				return;
			}

			// Don't refresh non-destructive updates if the user is actively editing
			if (saveStatus === 'saving' || editingTitle) return;

			// Capture the item this sync was scoped to *before* awaiting.
			// Same race guard as the SSE handler above and loadData() —
			// a navigation that completes mid-flight must not let a stale
			// resolution clobber the newly-loaded item.
			const reqItemId = item.id;
			const reqWsSlug = wsSlug;
			const reqItemSlug = itemSlug;

			if (result.type === 'incremental') {
				// Check if our item is in the changed set
				const updated = result.changes.updated.find(i => i.id === reqItemId);
				if (updated) {
					// Merge server state without disrupting the editor.
					// Same collab-aware adoption rule as the SSE
					// handler above (TASK-1262).
					if (!item || item.id !== reqItemId) return;
					item = adoptServerItem(updated);
					const links = await api.links.list(reqWsSlug, updated.slug).catch(() => []);
					if (!item || item.id !== reqItemId) return;
					itemLinks = links;
				}
				return;
			}

			// Full refresh fallback
			try {
				const updated = await api.items.get(reqWsSlug, reqItemSlug);
				if (!item || item.id !== reqItemId) return;
				item = adoptServerItem(updated);
				const links = await api.links.list(reqWsSlug, updated.slug).catch(() => []);
				if (!item || item.id !== reqItemId) return;
				itemLinks = links;
				syncService.markSynced(); // Advance cursor now that reload succeeded
			} catch {
				// Ignore — will catch up on next event
			}
		});
	});

	onDestroy(() => {
		unsubscribeSync?.();
		unsubscribeSSE?.();
		unsubscribeBeforePrint?.();
		editorStore.resetForDoc();
		collectionStore.setActiveItem(null);
	});

	async function loadData() {
		loading = true;
		error = '';
		// Clear per-item state that must NOT leak across navigation.
		// Without this, navigating from item A (mid-raw-edit) to item B
		// would either (a) seed B's raw editor with A's live markdown
		// via rawSeedMarkdown, or (b) cause flushRawIfPending on B to
		// PATCH A's queued markdown into B. Cancel any in-flight raw
		// debounce too. Per Codex review round 10.
		clearTimeout(contentDebounceTimer);
		contentDebounceTimer = undefined;
		clearTimeout(collabFlushTimer);
		collabFlushTimer = undefined;
		rawSeedMarkdown = null;
		rawPendingMarkdown = null;
		// lastFlushedContent is per-item; resetting prevents the
		// dedupe from incorrectly suppressing the first flush on
		// the next item (which happens to share the same markdown
		// as the last flush of the previous item — vanishingly
		// unlikely but trivial to defend).
		lastFlushedContent = null;
		// Reset transient save UI state too. Without this, a stale
		// raw PATCH that gets discarded by the new race guard
		// (item.id mismatch) leaves saveStatus pinned at 'saving' on
		// the next item, which then suppresses SSE/sync refreshes
		// (the `if (saveStatus === 'saving')` guards above).
		// Per Codex review round 12.
		clearTimeout(saveStatusTimer);
		saveStatusTimer = undefined;
		saveStatus = 'idle';
		// Capture the URL parts this load was scoped to. Used in the catch
		// path to detect whether the user has navigated away before this
		// request rejected — without this the stale catch would clobber a
		// fresh `pad-last-route-{wsSlug}` written by the workspace +layout
		// effect after the user moved on (TASK-754 round-2 race guard).
		const reqUsername = username;
		const reqWsSlug = wsSlug;
		const reqCollSlug = collSlug;
		const reqItemSlug = itemSlug;
		try {
			// Workspace items are needed for wiki-link resolution at Y.Doc
			// seed time (the $effect ~line 904 below) and for the
			// non-collab editorContent derived (~line 103 above). Both
			// silently fall back to raw markdown when the items array is
			// empty, baking literal `[[X]]` text into the Y.Doc the first
			// time a fresh item with wiki-links is opened (BUG-1461). We
			// fold the load into this Promise.all and AWAIT it before
			// `item` is set so the downstream readers always see a
			// populated items list paired with the correct workspace.
			// The freshness gate (workspace-scoped, not length-based) is
			// what prevents a stale cross-workspace items array from
			// satisfying a naive `items.length > 0` check after a
			// workspace switch.
			const itemsPromise = collectionStore.itemsAreFreshFor(wsSlug)
				? Promise.resolve()
				: collectionStore.loadItems(wsSlug);
			const [itemData, collData] = await Promise.all([
				api.items.get(wsSlug, itemSlug),
				api.collections.get(wsSlug, collSlug),
				itemsPromise
			]);
			// Overlay any in-flight optimistic tag edit so navigating away and
			// back mid-save can't show (and then let a follow-up edit overwrite
			// with) stale server tags. See withInflightTags. Per Codex PR #659.
			item = withInflightTags(itemData);
			collection = collData;
			collectionStore.setActiveItem(itemData);
			editorStore.resetForDoc();

			// Fetch child item progress for any item (generalized parent/child)
			try {
				const progress = await api.items.progress(wsSlug, itemData.slug);
				if (progress.total > 0) {
					hasChildren = true;
					computedOverrides = { progress: progress.percentage, _progressDone: progress.done, _progressTotal: progress.total };
				} else {
					hasChildren = false;
					computedOverrides = {};
				}
			} catch {
				hasChildren = false;
				computedOverrides = {};
			}

			// Items for wiki-link resolution are now loaded as part of the
			// Promise.all above so they're awaited before `item` is set
			// (BUG-1461 — previously this was a fire-and-forget call here,
			// which raced the Y.Doc seed and could bake `[[X]]` text in).

			// Load links for this item
			try {
				itemLinks = await api.links.list(wsSlug, itemData.slug);
			} catch { itemLinks = []; }

			// Load workspace members and agent roles for assignment picker
			try {
				const membersData = await api.members.list(wsSlug);
				workspaceMembers = membersData.members ?? [];
			} catch { workspaceMembers = []; }
			try {
				agentRoles = await api.agentRoles.list(wsSlug);
			} catch { agentRoles = []; }
		} catch (e: any) {
			error = e.message ?? 'Failed to load item';
			// Clear the workspace's last-route cache so the workspace
			// switcher (TASK-754) doesn't keep restoring this dead item
			// URL on subsequent re-entries — but only if the stored value
			// still points at THIS failed URL. If the user navigated away
			// while the request was in flight, the +layout effect has
			// already written a newer entry and we must not clobber it.
			try {
				const failedPath = `/${reqUsername}/${reqWsSlug}/${reqCollSlug}/${reqItemSlug}`;
				const cached = localStorage.getItem(`pad-last-route-${reqWsSlug}`);
				if (cached) {
					const cachedPath = cached.split('?')[0].split('#')[0];
					if (cachedPath === failedPath) {
						localStorage.removeItem(`pad-last-route-${reqWsSlug}`);
					}
				}
			} catch {}
		} finally {
			loading = false;

			// Capture the auto-edit intent — actual trigger lives in the
			// $effect below so it can fire even if /me (which feeds canEdit)
			// resolves AFTER loadData() finishes. One-shot. Always reassign
			// (true OR false) so a stale flag from a previous ?new=1 load
			// can't fire on a subsequent non-new item load — Codex round 6.
			pendingNewItemEdit = page.url.searchParams.get('new') === '1';
		}
	}

	// ── Yjs / collab provider lifecycle (PLAN-1248 / TASK-1259) ────────
	// Per loaded item with edit access, mint a fresh Y.Doc and attach a
	// CollabProvider that opens a WebSocket to /api/v1/collab/{itemID}.
	// The Y.Doc is passed down to <Editor /> via the `ydoc` prop, which
	// disables StarterKit history and registers the Collaboration
	// extension instead (TASK-1258 wiring).
	//
	// The lifecycle is keyed on `${item.id}:${canEdit}` — the same key
	// used to re-mount <Editor /> below. When either changes (item swap
	// or permission flip) the previous Y.Doc + provider tear down
	// cleanly and a new pair is constructed. The cleanup function on
	// $effect runs both on key change and on page unmount.
	//
	// View-only viewers (canEdit === false) get the legacy non-collab
	// editor; they can still observe the markdown snapshot persisted
	// by the canonical 5s flush (TASK-1260). Wiring a read-only
	// y-binding for them is a polish step deferred to TASK-1266.
	// rawMode is included in the collab key because raw-markdown saves
	// bypass the y-binding entirely (PATCH writes to items.content
	// directly). Leaving the provider connected during raw mode would
	// (a) tell the server an applier exists for the item, (b) leave
	// the in-memory Y.Doc holding pre-raw-save state, and (c) on the
	// next 5s flush after toggling back, push that stale Y.Doc state
	// over the user's fresh raw save. Destroying the provider while
	// rawMode is active gives the CLI/MCP applier path the right
	// "no active room" answer and lets the regular direct-write path
	// take care of items.content. Switching back mints a fresh Y.Doc
	// + reseeds via op-log replay (and TASK-1261's lazy seed for the
	// post-raw-save markdown). Per Codex review round 2.
	let ydoc = $state<Y.Doc | null>(null);
	let collabProvider = $state<CollabProvider | null>(null);
	// forceRefreshNonce drives Y.Doc/provider recreation when the
	// server signals our resume cursor was stale (TASK-1319). The
	// collab $effect reads it as a reactive dependency, so bumping
	// the value tears down the current provider+doc and rebuilds
	// from items.content via the lazy-seed path. The previous
	// provider's onForceRefresh handler does the bumping.
	let forceRefreshNonce = $state(0);
	// skipFlushOnNextCleanup is the flag the provider's onForceRefresh
	// handler sets so the next collab $effect cleanup tears down
	// without flushing the (known-stale) Y.Doc-derived markdown.
	// Reset at the start of each cleanup so a subsequent normal
	// teardown still runs its flush. Per Codex review round 1 [P1]
	// of TASK-1319.
	let skipFlushOnNextCleanup = false;
	// forceRefreshInFlight blocks any new collab-snapshot flush from
	// being scheduled (or surviving) while the page is mid-recovery
	// from a server force_refresh. Without it, an in-flight refetch +
	// pending nonce bump leaves the stale editor mounted; a local
	// edit during that window arms a NEW collabFlushTimer that fires
	// before cleanup runs and PATCHes stale Y.Doc-derived markdown
	// back to canonical items.content. Reset to false at the END of
	// the new collab $effect run (after rebuild completes). Per
	// Codex round 7 [P1] of TASK-1319.
	let forceRefreshInFlight = false;
	const collabKey = $derived(item && canEdit && !rawMode ? item.id : null);

	// Local user identity broadcast over awareness for the
	// CollaborationCaret extension (TASK-1263). Colour is a
	// deterministic hash of user.id so the same user gets the same
	// hue across sessions / tabs / devices. The route is gated by
	// the root auth check, so by the time the editor mounts
	// `authStore.user` is reliably populated; we still return
	// `undefined` for the unauthenticated path so the editor can
	// safely omit the cursor extension. If the auth race ever does
	// matter in practice we'd add `authStore.user?.id` to the
	// Editor's `{#key}` so a late user load remounts with cursors
	// enabled.
	const collabUserState = $derived(
		authStore.user
			? {
					name: authStore.user.name || authStore.user.username || 'Someone',
					color: userColor(authStore.user.id),
				}
			: undefined,
	);

	// Per-provider latch — true once the current provider has synced
	// at least once. Prevents a mid-session `reconnecting` from
	// re-showing the skeleton over already-rendered content.
	let hasEverSynced = $state(false);

	// Reset latch + null editorInstance whenever the provider INSTANCE
	// changes — covers every way a fresh unsynced Y.Doc gets mounted
	// under the same page component:
	//   1. Item navigation (collabKey derives from item.id; SvelteKit
	//      reuses +page.svelte across [slug] changes).
	//   2. Raw → Rich mode toggle (collabKey derives from rawMode).
	//   3. forceRefreshNonce bump (server force_refresh OR TASK-1376
	//      retryCollabSync), which tears down + rebuilds the provider
	//      against the same item.
	// Reading `collabProvider` registers it as a reactive dep; the
	// void cast silences "unused expression" lint. Source order
	// before the flip effect so a synchronous reset never clobbers a
	// fresh-provider sync that already landed in the same tick. Per
	// CONVE-606 (split route-change-equivalent resets from reactive-
	// state-sync flips).
	//
	// `editorInstance` is nulled alongside the latch because
	// `onEditor` only fires on mount — it does NOT re-fire with null
	// on unmount. During the connecting-skeleton window for a same-
	// item rebuild (rawMode toggle, force_refresh), the old <Editor>
	// is unmounted but `editorInstance` would otherwise still point
	// at the previous (destroyed) instance. An `applier_request`
	// frame arriving on the new provider in that window would then
	// `setContent` on the wrong editor (or a destroyed one) instead
	// of falling through to the server's direct-write fallback. The
	// new editor's mount callback sets `editorInstance` again once
	// the skeleton phase ends. Per Codex review round 2 of TASK-1375.
	$effect(() => {
		void collabProvider;
		hasEverSynced = false;
		editorInstance = null;
	});

	// Flip the latch on the live provider's first sync.
	$effect(() => {
		if (collabProvider?.synced) hasEverSynced = true;
	});

	// Stuck-connecting timeout — if the provider stays in
	// `connecting` for >10s without ever syncing, surface the same
	// ContentError UI as `offline`. Unconditional reset at the top
	// of each effect run gives every fresh provider its own 10s
	// grace (covers item navigation, rawMode toggle, force_refresh,
	// and retry-driven rebuilds — without this, a stuck-connecting
	// flag from a previous provider would carry over and the new
	// provider would immediately show error UI). Only the timer
	// can flip it back to true. Per Codex review round 1.
	let staleConnecting = $state(false);
	$effect(() => {
		if (!collabProvider) return;
		staleConnecting = false;
		if (collabProvider.state !== 'connecting' || hasEverSynced) {
			return;
		}
		const t = setTimeout(() => {
			if (collabProvider?.state === 'connecting' && !hasEverSynced) {
				staleConnecting = true;
			}
		}, 10_000);
		return () => clearTimeout(t);
	});

	// Manual recovery from the initial-connect failure modes
	// (staleConnecting / offline-while-!hasEverSynced). The template
	// gate restricts retry to cases where `!hasEverSynced`, which
	// means the current Y.Doc has never received a sync and therefore
	// cannot hold user edits — tearing down the provider is safe.
	// (Offline AFTER a successful sync keeps the editor mounted; see
	// the gate comment in the template for why.)
	//
	// Bumps forceRefreshNonce so the collab $effect tears down the
	// dead provider and rebuilds. We deliberately do NOT refetch
	// items.content here — the lazy-seed on the new Y.Doc reads from
	// the already-cached item.content, and the new provider's WS
	// replay reconciles against canonical server state via the
	// op-log. If the cursor is below MIN the server sends a real
	// force_refresh which goes through onForceRefresh (which DOES
	// refetch — correctly, because the server is the source of truth
	// in that case). Per Codex review rounds 1 and 2 of TASK-1376.
	function retryCollabSync() {
		if (!item) return;
		staleConnecting = false;
		forceRefreshNonce += 1;
	}

	$effect(() => {
		if (!collabKey) return;
		const itemId = collabKey;
		// Track forceRefreshNonce so a bump from the provider's
		// onForceRefresh handler tears this effect's old provider+doc
		// down via the cleanup return and rebuilds fresh. Reading
		// the rune is enough — Svelte 5 records the dependency. The
		// _ underscore name silences "unused" lints; the side effect
		// is the dependency tracking itself. Per TASK-1319.
		const _refreshNonce = forceRefreshNonce;
		void _refreshNonce;
		// Snapshot the workspace alongside the itemId. activeCollabContext
		// is what the timer-driven + cleanup-driven flushes target so
		// they always PATCH the right URL even after navigation.
		const ctx = { wsSlug, itemId };
		activeCollabContext = ctx;

		const doc = new Y.Doc();
		const provider = new CollabProvider(itemId, doc, {
			// Designated-applier handler: when a CLI / MCP / API caller
			// PATCHes content while this tab is connected, the server
			// asks one tab to apply the markdown via the editor's
			// y-tiptap binding (which translates setContent into Y.Doc
			// ops, propagating to all peers without overwriting the
			// canonical items.content from a stale source). Returning
			// `true` triggers an applier_ack so the server's PATCH
			// returns 200 immediately instead of waiting for the
			// applier-timeout fallback (~30s) and then writing
			// items.content directly. The full ExpiresAtMillis-driven
			// late-apply guard is TASK-1262's concern.
			onApplierRequest: async (markdown, _requestID, expiresAtMillis) => {
				if (!editorInstance) return false;
				// Pre-mutation late-apply guard. The provider already
				// gates the ack on expiry but the actual setContent
				// is owned here; gating the mutation itself is the
				// only way to truly prevent a stale apply when the
				// handler crosses the deadline. Per Codex review
				// round 3 — the provider's post-check alone only
				// suppresses the ack, not the side effect.
				if (expiresAtMillis > 0 && Date.now() > expiresAtMillis) {
					return false;
				}
				try {
					editorInstance.commands.setContent(markdown);
					// Brief notification so users see WHY their editor
					// just changed under them. The applier path is
					// triggered by external (CLI / MCP / API) writes
					// — a silent setContent would otherwise look like
					// a glitch. Per TASK-1262 acceptance criteria.
					toastStore.show('External edit applied', 'info');
					return true;
				} catch (err) {
					console.warn('collab: setContent failed', err);
					return false;
				}
			},
			// Force-refresh handler (TASK-1319). Fires when the
			// server detects our `?since=<id>` is below the current
			// MIN(item_yjs_updates.id) — the rows we expected to
			// replay have been pruned (op-log GC, schema rebuild,
			// PruneAndApply), so reconnecting from local state
			// would corrupt the Y.Doc. Recovery: bump the
			// `collabKey` so the $effect cleanup tears down the
			// provider+doc, then a fresh provider+doc rebuilds from
			// items.content via the lazy-seed path (TASK-1261).
			//
			// We don't explicitly call `provider.destroy()` here —
			// the WS is already closing on the server side and the
			// $effect cleanup runs that destroy as part of swapping
			// in the new provider.
			onForceRefresh: () => {
				toastStore.show(
					'Editor refreshed — rejoining with the latest content from the server.',
					'info',
				);
				// Mark this provider's effect cleanup as a
				// force-refresh tear-down so the cleanup path
				// SKIPS the trailing collab-snapshot flush. Per
				// Codex review round 1 [P1] of TASK-1319: a
				// force_refresh means the local Y.Doc state is
				// known stale (its cursor was below MIN, so the
				// rows it would replay no longer exist); flushing
				// the Y.Doc-derived markdown would overwrite the
				// canonical items.content the fresh provider is
				// supposed to lazy-seed from.
				skipFlushOnNextCleanup = true;
				forceRefreshInFlight = true;
				// Cancel any in-flight 5s flush timer too. The
				// cleanup-skip flag only covers the cleanup path;
				// a timer that already armed before force_refresh
				// fired would otherwise PATCH the (stale) Y.Doc-
				// derived markdown after the cleanup ran. Per
				// Codex round 2 [P1].
				clearTimeout(collabFlushTimer);
				collabFlushTimer = undefined;
				// Refresh items.content from the server BEFORE
				// rebuilding the Y.Doc. Without this, the lazy
				// seed (TASK-1261) on the new doc reads the
				// possibly-stale page-state cache of item.content
				// and re-encodes that into a fresh op-log; the
				// next flush then overwrites canonical server
				// content with our stale view. The await is
				// fire-and-forget at the call site; we bump the
				// nonce only after the GET resolves so the
				// cleanup→rebuild sequence sees fresh content. A
				// failed fetch falls through to the bump anyway
				// (better an editor session against possibly-
				// stale content than no editor session at all);
				// the user is already showing the toast about a
				// refresh. Per Codex round 4 [P1].
				const refreshCtx = ctx;
				void api.items
					.get(refreshCtx.wsSlug, refreshCtx.itemId)
					.then((fresh) => {
						// Apply only if the user is still on this
						// item; a navigation away should not stamp
						// fresh content into the OTHER item's slot.
						if (item && item.id === refreshCtx.itemId) {
							item = withInflightTags(fresh);
						}
						// Refetch succeeded → safe to rebuild.
						forceRefreshNonce += 1;
					})
					.catch((err) => {
						// Refetch failed — we cannot guarantee the
						// cached `item.content` reflects canonical
						// server state. Rebuilding here would
						// lazy-seed a fresh Y.Doc from the cached
						// (possibly-stale) content, then the next
						// flush would PATCH that stale content
						// back to the server — recreating the
						// corruption force_refresh was meant to
						// prevent. Surface a hard error and ask
						// the user to reload manually instead.
						// The provider is already destroyed so
						// the editor is read-only effectively;
						// forceRefreshInFlight stays true so any
						// in-flight or scheduled flush is blocked.
						// Per Codex round 17 [P1] of TASK-1319.
						console.warn(
							'collab: force_refresh item refetch failed; refusing to auto-rebuild',
							err,
						);
						toastStore.show(
							'Could not refresh editor — please reload the page.',
							'error',
						);
					});
			},
		});

		ydoc = doc;
		collabProvider = provider;
		// A fresh provider has been wired; the force-refresh
		// recovery window (if any) has closed. Local edits from
		// this point can safely schedule flushes again. Per Codex
		// round 7 [P1] of TASK-1319.
		forceRefreshInFlight = false;

		return () => {
			// Best-effort flush of items.content BEFORE we tear the
			// provider down. The Y.Doc + op-log are canonical, but
			// downstream consumers (search, share-page, exports,
			// API readers) read items.content; without this flush a
			// user closing the tab right after typing would leave
			// items.content frozen at the prior 5s tick. We pass
			// the captured ctx (NOT live reactive state) so a
			// navigation that already updated `item` and `wsSlug`
			// doesn't mis-route this flush to a different item.
			// keepalive=true lets the request outlive the page
			// lifecycle. Per TASK-1260.
			//
			// EXCEPTION: skip the flush if the cleanup is firing
			// because the user just toggled INTO raw mode. The
			// raw-button onclick already pre-populated
			// rawPendingMarkdown with the live editor markdown, so
			// the 1.2s raw debounce will land items.content cleanly.
			// A keepalive collab-snapshot PATCH from here is
			// fire-and-forget and can arrive AFTER the raw save —
			// clobbering newer raw edits with the older Y.Doc
			// snapshot. The other cleanup triggers (item nav,
			// canEdit flip, page unmount) all benefit from the
			// flush. Per Codex review round 3.
			// Force-refresh tear-down skips the flush — the local
			// Y.Doc is known stale and flushing it would overwrite
			// canonical items.content. Per Codex review round 1
			// [P1] of TASK-1319.
			const skipFlush = skipFlushOnNextCleanup;
			skipFlushOnNextCleanup = false;
			if (!rawMode && !skipFlush) {
				flushCollabNow(ctx, true);
			}
			provider.destroy();
			doc.destroy();
			if (activeCollabContext === ctx) activeCollabContext = null;
			// Reset seededProvider so a future provider for this
			// item (e.g. raw→rich toggle) is re-eligible for the
			// lazy seed if its op-log was pruned in the interim.
			if (seededProvider === provider) seededProvider = null;
			// Defensive — only clear the slot if it still holds the
			// pair we created. A reactive churn that swapped a new
			// pair in before this cleanup ran shouldn't get clobbered.
			if (ydoc === doc) ydoc = null;
			if (collabProvider === provider) collabProvider = null;
		};
	});

	// beforeunload: same flush as $effect cleanup, but routed
	// through the page lifecycle so navigation off-site (close tab,
	// reload, follow external link) lands the markdown snapshot
	// before the WS dies. fetch keepalive: true is the modern
	// equivalent of sendBeacon for non-POST requests; supports up to
	// ~64KB body which dwarfs typical markdown items.
	$effect(() => {
		if (typeof window === 'undefined') return;
		const onBeforeUnload = () => {
			const ctx = activeCollabContext;
			if (ctx) flushCollabNow(ctx, true);
		};
		window.addEventListener('beforeunload', onBeforeUnload);
		return () => window.removeEventListener('beforeunload', onBeforeUnload);
	});

	// Lazy seed (TASK-1261 / PLAN-1248). When a fresh collab session
	// completes its initial sync against an EMPTY op-log, the editor
	// renders a blank document — even if items.content holds existing
	// markdown. This effect fires once per provider, after the
	// `synced` reactive flag flips true: if the Y.Doc fragment is
	// genuinely empty AND items.content is non-empty, it calls
	// editor.commands.setContent with the markdown. The y-tiptap
	// binding turns that into Y.Doc ops which persist to the op-log
	// + propagate to peers — so subsequent connects (and other live
	// peers) see the content via the normal replay path.
	//
	// Idempotence: seededProvider tracks which provider instance we
	// already tried. A new provider (item swap, canEdit flip, etc.)
	// resets eligibility automatically because its reference !==
	// seededProvider.
	//
	// Multi-tab race: if two tabs finish their initial sync in the
	// same microsecond and both find the fragment empty, both fire
	// setContent. Y.Doc CRDT merges the two replace-ops with
	// last-write-wins semantics on the same content — the worst-case
	// outcome is one wasted op. Acceptable for v1; a designated-
	// seeder lock (Y.Map flag) is a follow-up if observed in the
	// wild.
	$effect(() => {
		if (!collabProvider || !ydoc || !editorInstance || !item) return;
		if (!collabProvider.synced) return;
		if (seededProvider === collabProvider) return;
		seededProvider = collabProvider;

		// Y.Doc emptiness check. The Collaboration extension binds
		// to the fragment named 'default' (TASK-1258 / Editor.svelte
		// configure). Length 0 means no XML nodes — i.e. the
		// underlying ProseMirror doc is empty.
		const fragment = ydoc.getXmlFragment('default');
		if (fragment.length > 0) return;

		const seedRaw = item.content ?? '';
		if (!seedRaw.trim()) return;

		// Multi-tab seed election: among connected peers visible
		// in awareness, only the lowest clientID seeds. Yjs
		// concurrent inserts MERGE rather than dedupe, so two
		// tabs both calling setContent would produce duplicated
		// content. Election + recheck shrink the race window to
		// the time between checking awareness and dispatching
		// setContent. Per Codex review round 1.
		const localId = ydoc.clientID;
		const peerIds = Array.from(collabProvider.awareness.getStates().keys());
		// Awareness must include at least our own ID; if it's
		// empty the awareness handshake hasn't completed yet —
		// skip this tick and let the next $effect run try again
		// (a peer's awareness arrival re-triggers via the
		// `synced` dependency edge).
		if (peerIds.length === 0) return;
		const lowestId = peerIds.reduce((min, id) => (id < min ? id : min), peerIds[0]);
		if (lowestId !== localId) return;

		// Match Editor's onUpdate path: seed in URL-form markdown so
		// wiki-links resolve to clickable refs. The 5s flush will
		// later round-trip back to canonical [[wiki-link]] form via
		// markdownToWikiLinks.
		const allItems = collectionStore.items ?? [];
		const seedMd =
			allItems.length > 0 && seedRaw.includes('[[')
				? wikiLinksToMarkdown(seedRaw, allItems, wsSlug, username)
				: seedRaw;

		// Microtask-yield + recheck: a concurrent peer's seed may
		// have already propagated and just hasn't been applied to
		// our fragment yet. Yielding once gives the y-protocol
		// inbound queue a tick to flush. After yield, re-check
		// emptiness AND re-check election (peer set may have
		// changed).
		queueMicrotask(() => {
			if (fragment.length > 0) return;
			const peerIds2 = Array.from(collabProvider!.awareness.getStates().keys());
			if (peerIds2.length === 0) return;
			const lowest2 = peerIds2.reduce((min, id) => (id < min ? id : min), peerIds2[0]);
			if (lowest2 !== localId) return;
			editorInstance!.commands.setContent(seedMd);
		});
	});

	// Handle the ?new=1 auto-edit-title flow reactively. canEdit may flip
	// from false → true after loadData() resolves (workspace layout fires
	// workspaceStore.setCurrent without awaiting it, so /me can land after
	// the page mounts). Per Codex review round 5.
	let pendingNewItemEdit = $state(false);
	$effect(() => {
		if (pendingNewItemEdit && item && canEdit && !loading) {
			pendingNewItemEdit = false;
			goto(`/${username}/${wsSlug}/${collSlug}/${itemSlug}`, { replaceState: true, noScroll: true });
			startEditTitle();
		}
	});

	async function startEditTitle() {
		if (!item || !canEdit) return;
		titleDraft = item.title;
		editingTitle = true;
		// Wait for the DOM to render the textarea, then focus + select all
		await tick();
		if (titleInputEl) {
			autoResizeTitle(titleInputEl);
			titleInputEl.focus();
			titleInputEl.setSelectionRange(0, titleInputEl.value.length);
		}
	}

	function autoResizeTitle(el: HTMLTextAreaElement) {
		el.style.height = 'auto';
		el.style.height = el.scrollHeight + 'px';
	}

	function showSaved() {
		saveStatus = 'saved';
		clearTimeout(saveStatusTimer);
		saveStatusTimer = setTimeout(() => { saveStatus = 'idle'; }, 2000);
	}

	async function saveTitle() {
		editingTitle = false;
		if (!item || titleDraft.trim() === item.title) return;
		saveStatus = 'saving';
		try {
			item = withInflightTags(await api.items.update(wsSlug, item.id, { title: titleDraft.trim() }));
			showSaved();
		} catch {
			saveStatus = 'idle';
			toastStore.show('Failed to update title', 'error');
		}
	}

	function handleTitleKeydown(e: KeyboardEvent) {
		if (e.key === 'Enter') {
			e.preventDefault();
			saveTitle();
			// Move focus to the editor so you can start writing immediately
			requestAnimationFrame(() => editorInstance?.commands.focus());
		} else if (e.key === 'Escape') {
			editingTitle = false;
		}
	}

	async function updateField(key: string, value: any) {
		if (!item) return;
		const updated = { ...fields, [key]: value };
		const payload = JSON.stringify(updated);
		const targetItem = item;
		const targetWs = wsSlug;
		saveStatus = 'saving';

		const doUpdate = (force: boolean) =>
			api.items.update(targetWs, targetItem.id, {
				fields: payload,
				...(force ? { force: true } : {})
			});

		try {
			const fresh = await doUpdate(false);
			if (item && item.id === targetItem.id) item = withInflightTags(fresh);
			showSaved();
		} catch (e) {
			// BUG-1538 / TASK-1539: same open-children-guard recovery
			// path as the collection page's handleStatusChange. When the
			// user is editing the done-field (status) inline on the
			// detail page, surface the structured 409 and offer to
			// force-override instead of toasting a vague "Failed to
			// save".
			if (isOpenChildrenError(e)) {
				const parentRef = formatItemRef(targetItem) ?? targetItem.slug;
				let forced;
				try {
					forced = await confirmOpenChildrenOrThrow(e, parentRef, () => doUpdate(true));
				} catch (retryErr) {
					// The force retry itself failed (network / 500 /
					// fresh validation error after the override).
					saveStatus = 'idle';
					const msg = retryErr instanceof Error ? retryErr.message : 'Failed to save';
					console.error('Forced field update failed:', retryErr);
					toastStore.show(msg, 'error');
					return;
				}
				if (forced) {
					if (item && item.id === targetItem.id) item = withInflightTags(forced);
					showSaved();
					return;
				}
				// User declined to override. Snap the on-page field back
				// to its prior value by leaving `item.fields` untouched
				// (FieldEditor re-renders from `value` prop) and drop
				// the in-flight save indicator. Force `item` to a fresh
				// reference so child components re-prop unambiguously
				// even if they cache by identity.
				saveStatus = 'idle';
				if (item && item.id === targetItem.id) item = { ...item };
				toastStore.show('Status change cancelled', 'info');
				return;
			}
			// Any other failure mode — network, validation, 500, etc.
			saveStatus = 'idle';
			console.error('Failed to save field:', e);
			toastStore.show('Failed to save', 'error');
		}
	}

	// Serialized, coalescing tag saver. Tags are a top-level item column
	// (item.tags), not a schema field, and the chip input makes rapid edits
	// easy — so instead of firing concurrent PATCHes and guarding the races,
	// we keep ONE save in flight per item and coalesce to the latest desired
	// set. This structurally removes the overlap class: no stale completion
	// can clobber a newer set, and `confirmed` always tracks the last
	// server-acknowledged tags so a failed save reverts to server truth (not
	// to an optimistic, unconfirmed value). Scoped by item id so a save still
	// draining for a previous item can't touch the current one. Per Codex
	// PR #659 rounds 1/4/5.
	type TagSaver = {
		itemId: string;
		ws: string;
		pending: string[] | null; // latest desired set not yet sent (coalesced)
		desired: string[]; // latest desired set (in-flight OR pending) for reload reapply
		running: boolean;
		confirmed: string; // last server-acknowledged tags JSON (revert target)
	};
	// Keyed by item id so each item's in-flight saver stays discoverable across
	// navigation — navigating away from A and back must find A's running saver
	// and coalesce into it, not spawn a second concurrent A saver. Per Codex
	// PR #659 round 6.
	const tagSavers = new Map<string, TagSaver>();

	function updateTags(newTags: string[]) {
		if (!item) return;
		const targetItem = item;
		const targetWs = wsSlug;
		// Optimistic so chips react instantly.
		item = { ...item, tags: JSON.stringify(newTags) };

		// Coalesce into this item's running saver if one exists; otherwise
		// start a fresh one. The currently-displayed item is the only one whose
		// tags can be edited, so targetItem.id keys the right saver.
		const existing = tagSavers.get(targetItem.id);
		if (existing && existing.running) {
			existing.pending = newTags;
			existing.desired = newTags;
			return;
		}
		const saver: TagSaver = {
			itemId: targetItem.id,
			ws: targetWs,
			pending: newTags,
			desired: newTags,
			running: false,
			confirmed: targetItem.tags // confirmed baseline captured at burst start
		};
		tagSavers.set(targetItem.id, saver);
		void flushTagSaver(saver);
	}

	async function flushTagSaver(saver: TagSaver) {
		saver.running = true;
		saveStatus = 'saving';
		try {
			while (saver.pending !== null) {
				const toSave = saver.pending;
				saver.pending = null;
				const fresh = await api.items.update(saver.ws, saver.itemId, {
					tags: JSON.stringify(toSave)
				});
				saver.confirmed = fresh.tags;
				// Reconcile the UI to server truth only when nothing newer is
				// queued (avoids flicker) and we're still on this item. Route
				// through adoptServerItem so the tag PATCH echo can't clobber
				// unsaved editor content — its response carries the server's
				// `content`, and non-collab editors mirror item.content. Per
				// Codex PR #659 round 11.
				if (saver.pending === null && item && item.id === saver.itemId) {
					item = adoptServerItem(fresh);
				}
			}
			if (item && item.id === saver.itemId) showSaved();
			// A newly-created tag should appear in autocomplete next time.
			void loadTagSuggestions(saver.ws);
		} catch (e) {
			console.error('Failed to save tags:', e);
			saver.pending = null;
			if (item && item.id === saver.itemId) {
				// Revert to the last server-confirmed tags, not the optimistic set.
				item = { ...item, tags: saver.confirmed };
				saveStatus = 'idle';
				toastStore.show('Failed to save', 'error');
			}
		} finally {
			saver.running = false;
			// Drop the entry once fully drained so the map doesn't accumulate
			// stale savers; guard on identity so a newer saver isn't evicted.
			if (saver.pending === null && tagSavers.get(saver.itemId) === saver) {
				tagSavers.delete(saver.itemId);
			}
		}
	}

	// Overlay an in-flight optimistic tag edit onto any server snapshot of an
	// item before it's assigned to `item`. EVERY `item = <server data>`
	// assignment (realtime SSE/sync, content-save echo, post-action refresh,
	// initial load) must route through this: a tag PATCH owns `item.tags` until
	// it drains, and the refresh handlers' `saveStatus === 'saving'` guard is
	// racy (checked before their own await, not after), so overlaying the
	// saver's latest desired set at assignment time is the only race-free
	// guarantee that a concurrent snapshot can't drop the unsaved tags. Per
	// Codex PR #659 rounds 8/9.
	function withInflightTags(next: Item): Item {
		const saver = tagSavers.get(next.id);
		return saver?.running ? { ...next, tags: JSON.stringify(saver.desired) } : next;
	}

	// Realtime-refresh convenience: applies the content-adoption rule (under
	// collab the editor reads Y.Doc so adopt server content verbatim; otherwise
	// preserve the live local content) and the tag overlay.
	function adoptServerItem(updated: Item): Item {
		return withInflightTags(
			collabProvider ? updated : { ...updated, content: item?.content ?? updated.content }
		);
	}

	// Load the workspace's distinct tags for autocomplete. Pure data-fetch
	// keyed on the workspace slug (see the $effect below) — kept separate from
	// the item-load path per the Svelte 5 effect-splitting convention.
	async function loadTagSuggestions(ws: string) {
		if (!ws) return;
		try {
			const all = await api.tags.list(ws);
			// Drop stale results: if the workspace changed while this request
			// was in flight (page instance reused across navigation, or a
			// post-save reload for a now-previous workspace), don't overwrite
			// the current workspace's suggestions. Per Codex PR #659 round 2.
			if (ws !== wsSlug) return;
			tagSuggestions = all.map((t) => t.tag);
		} catch {
			if (ws === wsSlug) tagSuggestions = [];
		}
	}

	$effect(() => {
		loadTagSuggestions(wsSlug);
	});

	// stampSourceUrl writes the pad_source_url + pad_imported_at orphan
	// keys into the item's `fields` JSON. The keys aren't declared in
	// any collection schema — `internal/items/validate.go` only iterates
	// declared fields, so unknown keys round-trip through PATCH without
	// migration. See PLAN-1467's ghost-field design.
	//
	// Keys are namespaced with the `pad_` prefix so they cannot collide
	// with a user-defined `source_url` field — without the prefix, an
	// existing collection with that key in its schema would conflict
	// both at validate time and at chip-render time. (Per Codex review
	// round 5 finding #2.)
	//
	// Race mitigation: a concurrent updateField call to the same item
	// would, in the legacy pattern, race against our PATCH because both
	// send the FULL fields blob. To minimize the window we re-fetch the
	// item right before the PATCH and merge our keys onto the freshest
	// server snapshot. The window is still non-zero (between fetch and
	// patch-land), and the same race exists in the project's existing
	// updateField path — IDEA-1480 tracks a server-side partial-fields
	// update that would close it system-wide. (Per Codex review round 5
	// finding #1.)
	//
	// Item identity is captured before the await so a navigation
	// during the in-flight PATCH cannot stamp the WRONG item with
	// the source URL: the assignment to `item` is gated on the
	// page still showing the same item we started with.
	async function stampSourceUrl(meta: ImportURLResponse) {
		if (!item) return;
		const targetItem = item;
		const targetWs = wsSlug;
		try {
			// Re-fetch to get the latest fields snapshot from the server,
			// then merge our two keys. This narrows but does not fully
			// close the race against concurrent field edits.
			const latest = await api.items.get(targetWs, targetItem.id);
			if (!item || item.id !== targetItem.id) return;
			const latestFields = parseFields(latest);
			const merged = {
				...latestFields,
				pad_source_url: meta.source_url,
				pad_imported_at: meta.fetched_at
			};
			const fresh = await api.items.update(targetWs, targetItem.id, {
				fields: JSON.stringify(merged)
			});
			if (item && item.id === targetItem.id) {
				item = withInflightTags(fresh);
			}
		} catch (err) {
			// Non-fatal: the content was inserted regardless of whether
			// the stamping succeeded. Toast so the user knows the
			// "Refresh from source" affordance won't be available.
			console.warn('failed to stamp source_url', err);
			if (item && item.id === targetItem.id) {
				toastStore.show(
					'Imported, but source_url not saved — refresh affordance disabled',
					'error'
				);
			}
		}
	}

	// handleImportInserted runs after the modal splices markdown into
	// the editor. Per PLAN-1467 we only stamp source_url when (a) the
	// item had no prior content and (b) source_url is not already set.
	//
	// `ctx.wasEmpty` comes from the modal which captured editor.isEmpty
	// BEFORE inserting. Under collab the editor reflects the
	// authoritative Y.Doc — which can hold user edits that haven't yet
	// been flushed to item.content (the database snapshot). Checking
	// item.content here would let a mixed/manual document be stamped
	// as source-backed if it happened to not have synced yet. Per
	// Codex review round 4.
	function handleImportInserted(meta: ImportURLResponse, ctx: { wasEmpty: boolean }) {
		if (!item) return;
		const alreadyStamped =
			typeof fields.pad_source_url === 'string' && fields.pad_source_url.length > 0;
		if (!ctx.wasEmpty || alreadyStamped) return;
		void stampSourceUrl(meta);
	}

	// "Refresh from source" affordance. Re-fetches the source URL,
	// asks the user to confirm (the action is destructive — it
	// replaces the editor's content), then runs the same insert
	// pipeline as the modal: marked → HTML → editor.insertContent.
	// Diff-preview is intentionally deferred (see PLAN-1467 risks);
	// the Yjs op-log provides recoverable history.
	let refreshing = $state(false);
	async function refreshFromSource() {
		const url = fields.pad_source_url;
		if (!url || typeof url !== 'string' || !item || !editorInstance) return;
		// Capture the item + editor instance before any awaits. A user
		// who confirms the refresh and then navigates to another item
		// before the fetch returns would otherwise have their NEW item
		// clobbered with the OLD item's source content. After each
		// await we re-check the captured values match the live state
		// and bail if not. (Per Codex review round 2.)
		const targetItem = item;
		const targetEditor = editorInstance;
		const ok = typeof window !== 'undefined' &&
			window.confirm(
				`Replace the current content with a fresh fetch from:\n${url}\n\n` +
				'Your existing content will be replaced. Undo is available via the editor history.'
			);
		if (!ok) return;
		refreshing = true;
		try {
			const resp = await api.importURL(url);
			// Bail if the user navigated to a different item — applying
			// the refresh now would target the wrong document AND stamp
			// the wrong source URL.
			if (!item || item.id !== targetItem.id || editorInstance !== targetEditor) {
				return;
			}
			const html = marked.parse(resp.markdown, { async: false }) as string;
			// Replace the entire document: select all, delete, insert.
			// Under collab the Y.Doc tracks each transaction so peers
			// converge on the new state.
			targetEditor
				.chain()
				.focus()
				.selectAll()
				.deleteSelection()
				.insertContent(html)
				.run();
			// Bump imported_at and refresh source_url in case it
			// resolved through a different final URL on this fetch.
			// stampSourceUrl's own guard re-checks identity.
			await stampSourceUrl(resp);
			if (item && item.id === targetItem.id) {
				toastStore.show('Refreshed from source', 'success');
			}
		} catch (err) {
			if (item && item.id === targetItem.id) {
				toastStore.show(err instanceof Error ? err.message : 'Refresh failed', 'error');
			}
		} finally {
			// Always clear the spinner. The page component is reused
			// across item navigation, so leaving `refreshing = true`
			// when the user navigates away would re-emerge as a stuck
			// "Refreshing…" label the next time they open ANY item
			// with a source_url. The visual feedback is per-item only
			// while the user stays on the originating item; that's
			// acceptable — a successful navigation already signals
			// "user moved on". (Per Codex review round 3.)
			refreshing = false;
		}
	}

	async function updateAssignedUser(userId: string | null) {
		if (!item) return;
		saveStatus = 'saving';
		try {
			const update: Record<string, any> = {};
			if (userId) {
				update.assigned_user_id = userId;
			} else {
				update.clear_assigned_user = true;
			}
			item = withInflightTags(await api.items.update(wsSlug, item.id, update));
			showSaved();
		} catch {
			saveStatus = 'idle';
			toastStore.show('Failed to update assignment', 'error');
		}
	}

	async function updateAgentRole(roleId: string | null) {
		if (!item) return;
		saveStatus = 'saving';
		try {
			const update: Record<string, any> = {};
			if (roleId) {
				update.agent_role_id = roleId;
			} else {
				update.clear_agent_role = true;
			}
			item = withInflightTags(await api.items.update(wsSlug, item.id, update));
			showSaved();
		} catch {
			saveStatus = 'idle';
			toastStore.show('Failed to update role', 'error');
		}
	}

	// Tracks the last markdown we successfully PATCHed to items.content
	// in collab-flush mode. Lets the idle-fire dedupe redundant flushes
	// across multiple connected tabs that all converge on the same
	// Y.Doc state — without this, every tab fires its own 5s flush
	// after every shared edit, multiplying server PATCH load by the
	// peer count.
	let lastFlushedContent: string | null = null;

	// 5s-idle timer for the collab flush path. Distinct from
	// contentDebounceTimer (which the legacy non-collab and raw-mode
	// paths still use) so the two firings don't trample each other on
	// rapid mode toggles.
	let collabFlushTimer: ReturnType<typeof setTimeout> | undefined;
	const COLLAB_FLUSH_IDLE_MS = 5_000;

	function handleContentUpdate(markdown: string) {
		// Collab-active path: 5s idle flush of items.content via the
		// `?source=collab-snapshot` bypass (server skips the applier
		// loop, writes items.content directly). Y.Doc op-log is
		// canonical for live state; items.content stays "reasonably
		// fresh" for search / share-page / API consumers. Per
		// TASK-1260 / PLAN-1248.
		if (collabProvider) {
			editorStore.setDirty(true);
			scheduleCollabFlush(markdown);
			return;
		}
		clearTimeout(contentDebounceTimer);
		editorStore.setDirty(true);
		contentDebounceTimer = setTimeout(() => {
			if (!item) return;
			saveStatus = 'saving';
			// Set lastSaveTime BEFORE the API call so the SSE guard works
			// even if the SSE event arrives before the response.
			editorStore.setLastSaveTime(Date.now());
			const allItems = collectionStore.items ?? [];
			let toSave = markdown;
			if (allItems.length > 0) {
				toSave = markdownToWikiLinks(toSave, allItems);
			}
			toSave = cleanBrokenLinks(toSave);
			api.items.update(wsSlug, item.id, { content: toSave }).then(() => {
				// Don't overwrite item -- resetting editorContent would
				// clobber anything typed since the debounce started.
				editorStore.setLastSaveTime(Date.now());
				editorStore.setDirty(false);
				showSaved();
			}).catch(() => {
				saveStatus = 'idle';
				toastStore.show('Failed to save content', 'error');
			});
		}, 1200);
	}

	// activeCollabContext holds the (workspace, item) the currently-
	// connected provider was minted against. Capturing this at
	// $effect-body time (NOT at flush time) makes the flush path
	// resistant to navigation: if the user moves to a new item
	// between schedule and fire, the timer-driven and cleanup-driven
	// flushes still PATCH the OLD item's URL with its OLD markdown,
	// so we never cross-write one item's content into another. Per
	// Codex review round 1.
	let activeCollabContext: { wsSlug: string; itemId: string } | null = null;

	// Provider we've already attempted the lazy seed against. Reset
	// implicitly when collabProvider is replaced (the new provider
	// !== seededProvider, so the seed effect re-fires). Per
	// TASK-1261 / PLAN-1248.
	let seededProvider: CollabProvider | null = null;

	function scheduleCollabFlush(markdown: string) {
		clearTimeout(collabFlushTimer);
		// Force-refresh recovery in flight: block any new flush
		// scheduling. The provider is destroyed but the editor
		// component is still mounted (cleanup hasn't run yet); a
		// local edit during this window must NOT arm a flush timer
		// against the stale Y.Doc state. Per Codex round 7 [P1].
		if (forceRefreshInFlight) return;
		const ctx = activeCollabContext;
		if (!ctx) return;
		collabFlushTimer = setTimeout(() => {
			collabFlushTimer = undefined;
			void runCollabFlush(ctx.wsSlug, ctx.itemId, markdown, false);
		}, COLLAB_FLUSH_IDLE_MS);
	}

	// CollabFlushResult discriminates the three outcomes runCollabFlush
	// can produce so callers can act on them differently:
	//   - 'flushed' — PATCH succeeded; items.content now matches.
	//   - 'deduped' — skipped because lastFlushedContent already
	//     matched; items.content is ALREADY at this content (the
	//     previous successful flush put it there). Treated by the
	//     rich→raw toggle as equivalent to 'flushed' for seeding
	//     purposes — both mean "server has this markdown."
	//   - 'failed' — PATCH errored. The toggle path bails so we
	//     don't enter raw mode with stale state.
	// Per Codex review round 8.
	// 'skipped' is the TASK-1319 force_refresh-recovery path: the
	// flush was blocked because the local Y.Doc state is known
	// stale relative to the canonical server content. Distinct
	// from 'deduped' (server already has this markdown) so callers
	// like the rich→raw toggle can refuse to seed from the stale
	// markdown rather than silently propagate it.
	type CollabFlushResult = 'flushed' | 'deduped' | 'failed' | 'skipped';

	// runCollabFlush PATCHes items.content via the
	// `?source=collab-snapshot` bypass. Takes ws/item from the
	// captured context (NOT live reactive state) so a navigation in
	// flight doesn't mis-route the PATCH to a different item.
	async function runCollabFlush(
		ws: string,
		itemId: string,
		markdown: string,
		keepalive: boolean,
	): Promise<CollabFlushResult> {
		// Force-refresh recovery is in flight: any markdown derived
		// from the soon-to-be-discarded Y.Doc is, by definition,
		// stale relative to the canonical server content. Skipping
		// here covers every direct caller (beforeunload, raw-toggle,
		// flushCollabNow) without needing per-call-site guards. Per
		// Codex round 8 [P1] of TASK-1319.
		if (forceRefreshInFlight) return 'skipped';
		const allItems = collectionStore.items ?? [];
		let toSave = unescapeDocLinks(markdown);
		if (allItems.length > 0) {
			toSave = markdownToWikiLinks(toSave, allItems);
		}
		toSave = cleanBrokenLinks(toSave);
		// Dedupe: skip the PATCH if our last successful flush
		// already landed this exact content. Multiple connected
		// tabs would otherwise each fire a redundant PATCH after
		// every shared edit converges. Returns 'deduped' (NOT
		// 'failed') so callers can distinguish "no work needed"
		// from a real error. Per Codex review round 8.
		if (lastFlushedContent === toSave) return 'deduped';

		// UI mutations only fire when:
		//   - This is a foreground (user-driven) flush (!keepalive),
		//     AND
		//   - The user is still looking at the item we're flushing
		//     (item.id === itemId).
		// Background (keepalive=true) cleanup flushes after
		// navigation MUST NOT touch saveStatus / lastSaveTime —
		// those slots belong to whatever item the user is now on,
		// and stamping them from a stale flush leaves the new page
		// pinned in 'Saving...' indefinitely. Per Codex review
		// round 2.
		const isForegroundCurrent = (): boolean =>
			!keepalive && !!item && item.id === itemId;

		if (isForegroundCurrent()) {
			saveStatus = 'saving';
			editorStore.setLastSaveTime(Date.now());
		}
		try {
			// Pass the provider's per-tab op-log cursor (TASK-1319) so
			// the server can advance the GC watermark when this tab is
			// caught up to MAX(op-log.id). When the cursor is below
			// MAX (peer ops not yet applied here) the server leaves
			// the watermark untouched — the GC sweeper must not delete
			// rows this markdown doesn't reflect. The cursor reflects
			// the provider tracking THIS item; if a navigation has
			// already swapped to another item the foreground flush
			// path bails earlier on dedupe and this code doesn't run.
			const opLogCursor =
				collabProvider && collabProvider.itemID === itemId
					? collabProvider.lastOpLogID
					: undefined;
			await api.items.flushCollabContent(ws, itemId, toSave, {
				keepalive,
				opLogCursor,
			});
			// Post-await force_refresh check: a force_refresh
			// frame can arrive WHILE the PATCH is in flight
			// (server already accepted, items.content already
			// overwritten — the server-side gate covers the
			// happens-before case where MIN had advanced past
			// our cursor by the time the request landed). At
			// minimum, refuse to record this flush as
			// authoritative locally — don't seed lastFlushedContent
			// or update saveStatus from a known-stale base. Per
			// Codex round 10 [P1].
			if (forceRefreshInFlight) return 'skipped';
			// lastFlushedContent is per-item; only seed it if the
			// item we just flushed is still the active one.
			// Otherwise a stale flush could pollute the new page's
			// dedupe state.
			if (item && item.id === itemId) {
				lastFlushedContent = toSave;
			}
			if (isForegroundCurrent()) {
				editorStore.setLastSaveTime(Date.now());
				editorStore.setDirty(false);
				showSaved();
			}
			return 'flushed';
		} catch {
			if (isForegroundCurrent()) {
				saveStatus = 'idle';
				toastStore.show('Failed to save content', 'error');
			}
			return 'failed';
		}
	}

	// flushCollabNow fires the pending flush IMMEDIATELY (cancelling
	// the 5s timer). Takes the explicit ctx the cleanup captured —
	// reading editorInstance.storage at this instant is correct
	// because Svelte runs parent $effect cleanups BEFORE child
	// {#key}-driven unmounts, so the OLD editor (whose markdown we
	// want) is still mounted. Used by $effect cleanup + beforeunload
	// to land any in-flight markdown before the provider tears down.
	function flushCollabNow(ctx: { wsSlug: string; itemId: string }, keepalive: boolean): boolean {
		clearTimeout(collabFlushTimer);
		collabFlushTimer = undefined;
		if (!editorInstance) return false;
		let md: string;
		try {
			md = (editorInstance.storage as any).markdown?.getMarkdown?.() ?? '';
		} catch {
			return false;
		}
		// runCollabFlush is async but its return value is irrelevant
		// for synchronous callers — fire-and-forget under
		// keepalive=true is the contract on the unmount path.
		void runCollabFlush(ctx.wsSlug, ctx.itemId, md, keepalive);
		return true;
	}

	// Latest raw markdown that hasn't yet been PATCHed. Tracked
	// alongside contentDebounceTimer so toggling out of raw mode
	// (via flushRawIfPending below) can synchronously land the
	// pending edit BEFORE the collab provider mints — otherwise the
	// debounced PATCH fires after the provider is up, gets routed
	// through the applier path, and races peer state. Per Codex
	// review round 5.
	let rawPendingMarkdown: string | null = null;

	// One-shot seed for the raw editor when toggling from rich+collab.
	// items.content stays stale under collab (handleContentUpdate is
	// suppressed when the provider is active); without this seed,
	// RawMarkdownEditor would mount with the pre-collab markdown and
	// any subsequent save would silently lose the live Y.Doc state.
	// Reset to null on rich-mode toggle and on item swap. Per Codex
	// review round 9.
	let rawSeedMarkdown = $state<string | null>(null);

	function handleRawContentUpdate(markdown: string) {
		clearTimeout(contentDebounceTimer);
		editorStore.setDirty(true);
		rawPendingMarkdown = markdown;
		contentDebounceTimer = setTimeout(() => {
			if (!item) return;
			saveStatus = 'saving';
			editorStore.setLastSaveTime(Date.now());
			// Capture the item id this PATCH was scoped to so a
			// late-arriving response after navigation to a new item
			// can't apply the old item's snapshot to the new
			// page state (cross-item bleed). Per Codex review round
			// 11.
			const reqItemId = item.id;
			// Raw mode: content is already in storage format (with [[wiki links]])
			const toSave = markdown;
			api.items.update(wsSlug, reqItemId, { content: toSave }).then((updated) => {
				if (!item || item.id !== reqItemId) return;
				editorStore.setLastSaveTime(Date.now());
				// Raw saves change items.content via a path the
				// collab dedupe doesn't see. Without resetting
				// lastFlushedContent, a later collab flush could
				// dedupe a content == lastFlushedContent that no
				// longer reflects server state and skip the PATCH,
				// leaving items.content stuck on the raw save.
				// Per Codex review round 7.
				lastFlushedContent = null;
				// Stale-response guard: only swap in the server's
				// snapshot when no newer raw edit landed during the
				// PATCH. Otherwise RawMarkdownEditor's content-prop
				// mirror would reset the textarea mid-keystroke and
				// drop the queued edit. Mirrors the Round 8 fix in
				// flushRawIfPending. Per Codex review round 9.
				if (rawPendingMarkdown === toSave) {
					item = withInflightTags(updated);
					rawPendingMarkdown = null;
					editorStore.setDirty(false);
					showSaved();
				} else {
					// Newer pending edit; keep local content, adopt
					// server-side metadata only. The next debounce
					// cycle will land the queued edit.
					item = withInflightTags({ ...updated, content: item.content });
				}
			}).catch(() => {
				if (!item || item.id !== reqItemId) return;
				saveStatus = 'idle';
				toastStore.show('Failed to save content', 'error');
			});
		}, 1200);
	}

	// True while flushRawIfPending is awaiting a PATCH response.
	// Used to make the Rich-mode toggle re-entrant-safe and to
	// surface the in-flight state in the UI on rapid double-clicks.
	let rawFlushInFlight = false;

	// Cap on flushRawIfPending's drain loop. If the user is typing
	// fast enough to keep rawPendingMarkdown non-null across this
	// many PATCH round-trips, return false and force them to click
	// again — better than spinning indefinitely.
	const RAW_FLUSH_DRAIN_CAP = 5;

	// flushRawIfPending drains every queued raw edit SYNCHRONOUSLY
	// (one PATCH per drained snapshot, awaited) and returns true
	// only when rawPendingMarkdown is null on exit. Callers are
	// expected to gate state transitions (e.g. enabling collab) on
	// the return value — a stale rawPendingMarkdown left over from
	// a fast typist or a failed PATCH would otherwise re-introduce
	// the "collab active with unsaved raw edit" race the guard is
	// meant to prevent. Per Codex review round 7.
	async function flushRawIfPending(): Promise<boolean> {
		if (!item) return rawPendingMarkdown === null;

		// Re-entrancy: another flush is already running. Wait for
		// it to settle, then re-evaluate from scratch.
		if (rawFlushInFlight) {
			while (rawFlushInFlight) {
				await new Promise((r) => setTimeout(r, 50));
			}
			return rawPendingMarkdown === null;
		}

		if (rawPendingMarkdown === null) return true;

		rawFlushInFlight = true;
		// Capture the item id once for the entire drain. If the user
		// navigates away mid-flush, every iteration's stale response
		// (including the in-flight one) is discarded so it can't
		// clobber the newly-loaded item. Per Codex review round 11.
		const reqItemId = item.id;
		let lastError = false;
		try {
			for (let i = 0; i < RAW_FLUSH_DRAIN_CAP; i++) {
				const markdown: string | null = rawPendingMarkdown;
				if (markdown === null) break;
				clearTimeout(contentDebounceTimer);
				contentDebounceTimer = undefined;
				try {
					saveStatus = 'saving';
					editorStore.setLastSaveTime(Date.now());
					const updated = await api.items.update(wsSlug, reqItemId, { content: markdown });
					if (!item || item.id !== reqItemId) {
						// Navigation completed during the await;
						// abort and let the new item's state take
						// over.
						return false;
					}
					editorStore.setLastSaveTime(Date.now());
					// Raw saves change items.content via a path the
					// collab dedupe doesn't see; reset
					// lastFlushedContent so a future collab flush
					// can't skip a real PATCH. Per Codex review
					// round 7.
					lastFlushedContent = null;
					// Only swap in the server's snapshot when no
					// newer raw edit arrived during the await.
					// RawMarkdownEditor mirrors `item.content` into
					// its textarea unconditionally, so assigning a
					// stale snapshot here would reset the textarea
					// from under the user's keystrokes and lose the
					// queued edit. Per Codex review round 8.
					if (rawPendingMarkdown === markdown) {
						item = withInflightTags(updated);
						rawPendingMarkdown = null;
					} else {
						// Newer edit pending — keep our local
						// content but adopt server-side metadata
						// (timestamps, version, modified_by).
						item = withInflightTags({ ...updated, content: item.content });
					}
				} catch {
					saveStatus = 'idle';
					toastStore.show('Failed to save content', 'error');
					lastError = true;
					break;
				}
			}
			if (!lastError && rawPendingMarkdown === null) {
				editorStore.setDirty(false);
				showSaved();
				return true;
			}
			return false;
		} finally {
			rawFlushInFlight = false;
		}
	}

	let computedOverrides = $state<Record<string, any>>({});
	let childTerminalStatuses = $state<string[] | undefined>(undefined);

	function handleChildrenChange(items: Item[]) {
		// Track child IDs for deduplication in the relationships section
		childItemIds = new Set(items.map(i => i.id));
		hasChildren = items.length > 0;

		// Recompute progress from the actual children
		const total = items.length;
		const allCollections = collectionStore.collections ?? [];
		// Gather terminal statuses from all collections the children belong to
		const termSet = new Set<string>();
		for (const child of items) {
			const col = allCollections.find(c => c.slug === child.collection_slug);
			if (col) {
				for (const ts of getTerminalOptions(col)) termSet.add(ts);
			}
		}
		const termOpts = termSet.size > 0 ? [...termSet] : ['done', 'cancelled'];
		childTerminalStatuses = termOpts;
		const done = items.filter((i) => termOpts.includes(parseFields(i).status)).length;
		const progress = total > 0 ? Math.round((done / total) * 100) : 0;
		computedOverrides = { progress, _progressDone: done, _progressTotal: total };
	}

	function fieldValue(key: string): any {
		if (key in computedOverrides) return computedOverrides[key];
		return fields[key] ?? '';
	}

	function formatFieldDisplay(value: any): string {
		if (value === null || value === undefined || value === '') return '—';
		return String(value).replace(/_/g, ' ').replace(/\b\w/g, c => c.toUpperCase());
	}

	function relationLabel(ref?: string, title?: string, fallback?: string): string {
		if (ref && title) return `${ref} ${title}`;
		if (ref) return ref;
		if (title) return title;
		return fallback || 'Unknown item';
	}

	function relationHref(collectionSlug?: string, refOrSlug?: string): string | null {
		if (!collectionSlug || !refOrSlug) return null;
		return `/${username}/${wsSlug}/${collectionSlug}/${refOrSlug}`;
	}

	function linkEntry(link: ItemLink, useSource: boolean): RelationshipEntry {
		const ref = useSource ? link.source_ref : link.target_ref;
		const title = useSource ? link.source_title : link.target_title;
		const status = useSource ? link.source_status : link.target_status;
		const id = useSource ? link.source_id : link.target_id;
		const slug = useSource ? link.source_slug : link.target_slug;
		const collectionSlug = useSource ? link.source_collection_slug : link.target_collection_slug;
		const href = relationHref(collectionSlug, ref ?? slug);
		return {
			key: `${link.id}:${useSource ? 'source' : 'target'}`,
			label: relationLabel(ref, title, id),
			href,
			status,
			linkId: link.id
		};
	}

	function buildRelationshipGroups(currentItem: Item, links: ItemLink[], excludeChildIds: Set<string> = new Set()): RelationshipGroup[] {
		const grouped = new Map<string, RelationshipGroup>();
		const definitions: Record<string, { label: string; tone: RelationshipGroup['tone'] }> = {
			parent_of: { label: 'Children', tone: 'default' },
			child_of: { label: 'Child of', tone: 'default' },
			blocks: { label: 'Blocks', tone: 'blocks' },
			blocked_by: { label: 'Blocked by', tone: 'blocks' },
			links_to: { label: 'Links to', tone: 'wiki' },
			referenced_by: { label: 'Referenced by', tone: 'wiki' },
			split_from: { label: 'Split from', tone: 'lineage' },
			split_into: { label: 'Split into', tone: 'lineage' },
			supersedes: { label: 'Supersedes', tone: 'lineage' },
			superseded_by: { label: 'Superseded by', tone: 'lineage' },
			implements: { label: 'Implements', tone: 'lineage' },
			implemented_by: { label: 'Implemented by', tone: 'lineage' },
			related: { label: 'Related', tone: 'default' }
		};
		const order = ['parent_of', 'child_of', 'blocks', 'blocked_by', 'links_to', 'referenced_by', 'split_from', 'split_into', 'supersedes', 'superseded_by', 'implements', 'implemented_by', 'related'];

		function addEntry(groupKey: string, entry: RelationshipEntry) {
			const definition = definitions[groupKey];
			if (!definition) return;
			if (!grouped.has(groupKey)) {
				grouped.set(groupKey, { label: definition.label, tone: definition.tone, entries: [] });
			}
			grouped.get(groupKey)?.entries.push(entry);
		}

		for (const link of links) {
			const isSource = link.source_id === currentItem.id;
			switch (link.link_type) {
				case 'parent':
				case 'phase': {
					// If this item is the parent and the child is already shown in ChildItems, skip it
					if (!isSource && excludeChildIds.has(link.source_id)) break;
					addEntry(isSource ? 'child_of' : 'parent_of', linkEntry(link, !isSource));
					break;
				}
				case 'blocks':
					addEntry(isSource ? 'blocks' : 'blocked_by', linkEntry(link, !isSource));
					break;
				case 'wiki_link':
					addEntry(isSource ? 'links_to' : 'referenced_by', linkEntry(link, !isSource));
					break;
				case 'split_from':
					addEntry(isSource ? 'split_from' : 'split_into', linkEntry(link, !isSource));
					break;
				case 'supersedes':
					addEntry(isSource ? 'supersedes' : 'superseded_by', linkEntry(link, !isSource));
					break;
				case 'implements': {
					// If this item is the target (implemented by) and the source is already shown in ChildItems, skip it
					if (!isSource && excludeChildIds.has(link.source_id)) break;
					addEntry(isSource ? 'implements' : 'implemented_by', linkEntry(link, !isSource));
					break;
				}
				default:
					addEntry('related', linkEntry(link, !isSource));
					break;
			}
		}

		// Annotate the matching relationship group with closure summary
		if (currentItem.derived_closure) {
			const closureGroupKey: Record<string, string> = {
				superseded_by: 'superseded_by',
				implemented_by: 'implemented_by',
				split_into: 'split_into'
			};
			const key = closureGroupKey[currentItem.derived_closure.kind];
			if (key && grouped.has(key)) {
				grouped.get(key)!.closureSummary = currentItem.derived_closure.summary;
			}
		}

		return order
			.map((key) => grouped.get(key))
			.filter((group): group is RelationshipGroup => Boolean(group && group.entries.length > 0));
	}

	function handleVersionRestore(updatedItem: Item) {
		item = withInflightTags(updatedItem);
	}

	async function handleDelete() {
		if (!item) return;
		deleting = true;
		try {
			await api.items.delete(wsSlug, item.id);
			toastStore.show('Item deleted', 'success');
			goto(`/${username}/${wsSlug}/${collSlug}`);
		} catch {
			toastStore.show('Failed to delete item', 'error');
			deleting = false;
			confirmDelete = false;
		}
	}

	let allCollections = $derived(collectionStore.collections ?? []);
	let moveTargets = $derived(allCollections.filter(c => c.slug !== collSlug));

	async function handleDeleteLink(linkId?: string) {
		if (!linkId || !item) return;
		try {
			await api.links.delete(wsSlug, linkId);
			itemLinks = itemLinks.filter(l => l.id !== linkId);
			// Refresh item to update parent info
			const refreshed = await api.items.get(wsSlug, itemSlug);
			item = withInflightTags({ ...refreshed, content: item.content });
			toastStore.show('Relationship removed', 'success');
		} catch (e: any) {
			toastStore.show(e.message ?? 'Failed to remove relationship', 'error');
		}
	}

	// ── Add Relationship ─────────────────────────────────────────────────────
	let showAddLink = $state(false);
	let addLinkType = $state('related');
	let addLinkSearch = $state('');
	let addLinkResults = $state<Item[]>([]);
	let addLinkLoading = $state(false);

	async function searchItemsForLink() {
		if (!addLinkSearch.trim()) {
			addLinkResults = [];
			return;
		}
		addLinkLoading = true;
		try {
			const results = await api.search(addLinkSearch, { workspace: wsSlug });
			// Filter out self and items already linked
			const linkedIds = new Set(itemLinks.flatMap(l => [l.source_id, l.target_id]));
			addLinkResults = (results.results || [])
				.map((r) => r.item)
				.filter((i: Item) => i.id !== item?.id && !linkedIds.has(i.id))
				.slice(0, 10);
		} catch {
			addLinkResults = [];
		} finally {
			addLinkLoading = false;
		}
	}

	async function handleCreateLink(targetItem: Item) {
		if (!item) return;
		try {
			const newLink = await api.links.create(wsSlug, item.slug, {
				target_id: targetItem.id,
				link_type: addLinkType
			});
			itemLinks = [...itemLinks, newLink];
			showAddLink = false;
			addLinkSearch = '';
			addLinkResults = [];
			// Refresh item to update parent info
			const refreshed = await api.items.get(wsSlug, itemSlug);
			item = withInflightTags({ ...refreshed, content: item.content });
			toastStore.show('Relationship added', 'success');
		} catch (e: any) {
			toastStore.show(e.message ?? 'Failed to add relationship', 'error');
		}
	}

	async function handleMove(targetSlug: string) {
		if (!item || moving) return;
		moving = true;
		showMoveMenu = false;
		// Capture FULL route identity (workspace, username, source
		// slug, source item id, parent ref) at the moment the user
		// kicked off the move. If the user navigates while the
		// open-children modal is up, we don't want a confirmed force
		// retry to fire against the new route's workspace — that
		// could move the wrong item (Codex review round 2 P1).
		const sourceItem = item;
		const sourceSlug = item.slug;
		const sourceWs = wsSlug;
		const sourceUsername = username;
		const sourceCollSlug = collSlug;
		const sourceItemSlug = itemSlug;
		const parentRef = formatItemRef(sourceItem) ?? sourceItem.slug;
		const doMove = (force: boolean) =>
			api.items.move(sourceWs, sourceSlug, targetSlug, undefined, force ? { force: true } : undefined);
		// After the modal resolves, only honor the success path's
		// navigation/toast if the page is STILL on the source item.
		// We check both `item.id` (cheap object-identity guard) AND
		// the route params (page.params.{username,workspace,
		// collection,slug}) — during same-component navigation `item`
		// can briefly still hold the old object while the URL has
		// already advanced. Comparing both closes that race
		// (Codex review round 3 P1). Stale resolutions complete
		// silently rather than yanking the user back.
		const navIfStillCurrent = (toSlug: string) => {
			const itemStillCurrent = item && item.id === sourceItem.id;
			const routeStillCurrent =
				wsSlug === sourceWs &&
				username === sourceUsername &&
				collSlug === sourceCollSlug &&
				itemSlug === sourceItemSlug;
			if (itemStillCurrent && routeStillCurrent) {
				goto(`/${sourceUsername}/${sourceWs}/${targetSlug}/${toSlug}`, { replaceState: true });
			}
		};
		try {
			const moved = await doMove(false);
			toastStore.show(`Moved to ${targetSlug}`, 'success');
			navIfStillCurrent(moved.slug);
		} catch (e: any) {
			// BUG-1538 / Codex review round 1: the server's
			// open-children guard also fires on POST /move when the
			// move would land the parent terminal in the target
			// collection. Wire the same modal + ?force=true override
			// path used for PATCH status changes.
			if (isOpenChildrenError(e)) {
				let forced;
				try {
					forced = await confirmOpenChildrenOrThrow(e, parentRef, () => doMove(true));
				} catch (retryErr: any) {
					console.error('Forced move failed:', retryErr);
					toastStore.show(retryErr?.message ?? 'Failed to move item', 'error');
					return; // outer finally still resets `moving`
				}
				if (forced) {
					toastStore.show(`Moved to ${targetSlug}`, 'success');
					navIfStillCurrent(forced.slug);
				} else {
					toastStore.show('Move cancelled', 'info');
				}
				return;
			}
			toastStore.show(e.message ?? 'Failed to move item', 'error');
		} finally {
			moving = false;
		}
	}

	// TASK-1264: human-readable label + tooltip for the four-state
	// CollabConnectionState. Centralized here so the markup branches on
	// the variant only once.
	function collabStateLabel(s: CollabConnectionState): string {
		switch (s) {
			case 'synced': return 'Synced';
			case 'connecting': return 'Connecting…';
			case 'reconnecting': return 'Reconnecting…';
			case 'offline': return 'Offline';
		}
	}
	function collabStateTitle(s: CollabConnectionState): string {
		switch (s) {
			case 'synced': return 'Real-time collaboration active. Changes sync instantly.';
			case 'connecting': return 'Connecting to the collaboration server…';
			case 'reconnecting': return 'Connection dropped. Trying to reconnect…';
			case 'offline': return 'Could not reconnect. Edits are saved locally and will sync when the connection is restored.';
		}
	}
</script>

{#if loading}
	<ContentSkeleton variant="page" />
{:else if error}
	<ContentError
		title="Could not load item"
		detail={error}
		onRetry={loadData}
	/>
{:else if item && collection}
	<!-- Print-only footer (hidden on screen, fixed-positioned in print).
	     The repeating print-header was removed as part of BUG-626: a
	     clean page-1-only document header is rendered inside the normal
	     flow by `.title-row` + `.meta-info`. Page number comes from
	     `@page { @bottom-right ... }` in app.css. -->
	<div class="print-footer" aria-hidden="true">
		<span class="print-footer-date">Printed {printDate}</span>
		<span class="print-footer-url">{printUrl}</span>
	</div>

	<div class="item-page">
		<!-- Breadcrumb -->
		<div class="sticky-header">
			<nav class="breadcrumb">
				<a href="/{username}/{wsSlug}">Home</a>
				<span class="sep">/</span>
				{#if item.parent_collection_slug && item.parent_slug}
					{@const parentCollSlug = item.parent_collection_slug}
					{@const parentColl = allCollections.find(c => c.slug === parentCollSlug)}
					<a href="/{username}/{wsSlug}/{parentCollSlug}">{parentColl?.icon ?? ''} {parentColl?.name ?? parentCollSlug}</a>
					<span class="sep">/</span>
					<a href="/{username}/{wsSlug}/{item.parent_collection_slug}/{item.parent_slug}">{item.parent_ref || item.parent_title}</a>
					<span class="sep">/</span>
				{:else}
					<a href="/{username}/{wsSlug}/{collSlug}">{collection.icon} {collection.name}</a>
					<span class="sep">/</span>
				{/if}
				<span class="current">{formatItemRef(item) || item.title}</span>
				{#if formatItemRef(item)}
					<button class="copy-ref-btn" onclick={handleCopyRef} title="Copy item ID">
						{#if copied}
							<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polyline points="20 6 9 17 4 12"></polyline></svg>
							<span class="copied-tooltip">Copied!</span>
						{:else}
							<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="9" y="9" width="13" height="13" rx="2" ry="2"></rect><path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1"></path></svg>
						{/if}
					</button>
				{/if}
			</nav>
		</div>

		<!-- Title -->
		<div class="title-row">
			{#if formatItemRef(item)}
				<span class="item-ref">{formatItemRef(item)}</span>
			{/if}
			{#if editingTitle}
				<textarea
					class="title-input"
					rows="1"
					bind:this={titleInputEl}
					bind:value={titleDraft}
					onblur={saveTitle}
					onkeydown={handleTitleKeydown}
					oninput={(e) => autoResizeTitle(e.currentTarget)}
				></textarea>
			{:else if canEdit}
				<button class="title" onclick={startEditTitle}>
					{item.title}
				</button>
			{:else}
				<!-- Read-only title (PLAN-1100 / TASK-1105) — no click-to-edit. -->
				<h1 class="title title-readonly">{item.title}</h1>
			{/if}
			{#if typeof fields.pad_source_url === 'string' && fields.pad_source_url}
				<!--
					"Refresh from source" chip — visible only when the item
					was created via "Insert from URL" and the modal stamped
					pad_source_url into fields. The `pad_` prefix namespaces
					the ghost-field keys away from any user-defined
					`source_url` column on the collection schema (per Codex
					review round 5 finding #2).

					Click re-fetches and replaces editor content; the
					editor's Yjs op-log handles undo. Hidden for view-only
					users (no canEdit) — refresh is a content-replacing
					action that requires write access. Also hidden in
					raw-markdown mode: the rich Editor is unmounted there
					and refreshFromSource() drives content replacement
					through the Tiptap editor instance, which would either
					fail or update the off-screen rich editor. Read-only
					and raw users see the provenance chip without the
					Refresh button so the import history is still
					discoverable. (Per Codex review round 1.)
				-->
				{#if canEdit && !rawMode}
					<button
						type="button"
						class="source-chip"
						title={`Source: ${fields.pad_source_url}${fields.pad_imported_at ? `\nImported: ${fields.pad_imported_at}` : ''}`}
						disabled={refreshing}
						onclick={refreshFromSource}
					>
						<span class="source-chip-icon" aria-hidden="true">🌐</span>
						<span class="source-chip-label">
							{refreshing ? 'Refreshing…' : 'Refresh from source'}
						</span>
					</button>
				{:else}
					<span class="source-chip source-chip-readonly" title={fields.pad_source_url}>
						<span class="source-chip-icon" aria-hidden="true">🌐</span>
						<span class="source-chip-label">Imported from URL</span>
					</span>
				{/if}
			{/if}
		</div>

		{#if tags.length > 0}
			<div class="header-tags">
				{#each tags as tag, i (i)}
					<a class="header-tag" href={`/${username}/${wsSlug}/tags/${encodeURIComponent(tag)}`}>{tag}</a>
				{/each}
			</div>
		{/if}

		<!-- Meta info -->
		<div class="meta-info">
			<span title={new Date(item.created_at).toLocaleString()}>Created {relativeTime(item.created_at)} by {item.created_by || 'unknown'}</span>
			<span class="meta-sep">·</span>
			<span title={new Date(item.updated_at).toLocaleString()}>Updated {relativeTime(item.updated_at)}</span>
			<span class="save-status" class:saving={saveStatus === 'saving'} class:saved={saveStatus === 'saved'} class:visible={saveStatus !== 'idle'}>
				{#if saveStatus === 'saving'}Saving...{:else}✓ Saved{/if}
			</span>
			{#if collabProvider}
				<!-- Pending-sync indicator (TASK-1264). Visible only while
				     the WS provider exists, which means: canEdit && !rawMode.
				     Read-only / share-page / raw mode never see this badge.
				     Colour map matches the four-state machine in
				     wsProvider.svelte.ts: green=synced, yellow=connecting/
				     reconnecting, red=offline. -->
				<span
					class="collab-state collab-state-{collabProvider.state}"
					title={collabStateTitle(collabProvider.state)}
				>
					<span class="collab-state-dot" aria-hidden="true"></span>
					<span class="collab-state-label">{collabStateLabel(collabProvider.state)}</span>
				</span>
			{/if}
		</div>

		<!-- Actions -->
		<div class="meta-actions">
			<button
				class="action-btn star-btn"
				class:starred={starredStore.isStarred(item.id)}
				onclick={() => item && starredStore.toggle(wsSlug, item.slug, item.id)}
				title={starredStore.isStarred(item.id) ? 'Unstar' : 'Star'}
			>
				{starredStore.isStarred(item.id) ? '★' : '☆'}
			</button>
			{#if collection && (quickActions.length > 0 || isOwner)}
				<QuickActionsMenu
					actions={quickActions}
					{item}
					{collection}
					scope="item"
					{wsSlug}
					canEdit={isOwner}
					onmanage={() => {
						editCollectionSection = 'actions';
						editCollectionOpen = true;
					}}
					oncollectionupdated={(updated) => {
						collection = updated;
					}}
				/>
			{/if}
			<button
				class="action-btn"
				onclick={() => { document.getElementById('item-timeline')?.scrollIntoView({ behavior: 'smooth' }); }}
			>
				Timeline
			</button>
			{#if backlinksCount > 0}
				<!--
					Mention badge (PLAN-1593 / TASK-1596). Surfaces the inbound
					`[[...]]` reference count in the action bar so users can see
					at a glance that this item is referenced from elsewhere AND
					jump straight to the panel without scrolling. Hidden when
					the count is 0 so items with no inbound links don't
					advertise an empty surface. The count is driven by
					BacklinksPanel's onCountChange callback so the two stay in
					sync without a separate API call.
				-->
				<button
					class="action-btn"
					title="Mentioned in {backlinksCount} other item{backlinksCount === 1 ? '' : 's'}"
					onclick={() => { document.getElementById('item-backlinks')?.scrollIntoView({ behavior: 'smooth' }); }}
				>
					📎 {backlinksCount}
				</button>
			{/if}
			{#if canEdit}
				<div class="move-wrapper">
					<button class="action-btn" onclick={() => { showMoveMenu = !showMoveMenu; }} disabled={moving}>
						{moving ? 'Moving...' : 'Move to...'}
					</button>
					{#snippet moveOptions()}
						{#each moveTargets as target (target.slug)}
							<button class="move-option" onclick={() => handleMove(target.slug)}>
								{#if target.icon}<span class="move-icon">{target.icon}</span>{/if}
								{target.name}
							</button>
						{/each}
					{/snippet}
					{#if isMobile && showMoveMenu}
						<!--
							Gate the mobile sheet on `showMoveMenu` so BottomSheet (and
							its global keydown listener) isn't mounted when the menu is
							closed. Same pattern fix as ReactionPicker.
						-->
						<BottomSheet
							open={showMoveMenu}
							onclose={() => (showMoveMenu = false)}
							title="Move to…"
						>
							<div class="move-sheet-body">
								{@render moveOptions()}
							</div>
						</BottomSheet>
					{:else if showMoveMenu}
						<div class="move-dropdown">
							{@render moveOptions()}
						</div>
					{/if}
				</div>
			{/if}
			{#if isOwner}
				<button class="action-btn" onclick={() => { shareDialogOpen = true; }}>
					Share
				</button>
			{/if}
			{#if canEdit}
				{#if confirmDelete}
					<span class="delete-confirm">
						Delete this item?
						<button class="delete-confirm-btn yes" disabled={deleting} onclick={handleDelete}>
							{deleting ? '...' : 'Yes'}
						</button>
						<button class="delete-confirm-btn no" onclick={() => { confirmDelete = false; }}>
							No
						</button>
					</span>
				{:else}
					<button class="action-btn delete-btn" onclick={() => { confirmDelete = true; }}>
						Delete
					</button>
				{/if}
			{/if}
		</div>

		{#if codeContext}
			<div class="code-context-section">
				<h3 class="section-title">Code Context</h3>
				<div class="code-context-card">
					<div class="code-context-meta">
						<span class="code-provider">{formatFieldDisplay(codeContext.provider)}</span>
						{#if codeContext.repo}
							<span class="code-chip">{codeContext.repo}</span>
						{/if}
						{#if codeContext.branch}
							<span class="code-chip">{codeContext.branch}</span>
						{/if}
					</div>
					{#if codeContext.pull_request}
						<div class="code-pr-row">
							<a href={codeContext.pull_request.url} class="code-pr-link" target="_blank" rel="noreferrer">
								PR #{codeContext.pull_request.number}: {codeContext.pull_request.title}
							</a>
							<span class="code-pr-state">{formatFieldDisplay(codeContext.pull_request.state)}</span>
						</div>
						{#if codeContext.pull_request.updated_at}
							<div class="code-pr-updated">
								Updated {relativeTime(codeContext.pull_request.updated_at)}
							</div>
						{/if}
					{/if}
				</div>
			</div>
		{/if}

		<!-- Layout wrapper -->
		<div class="item-body layout-{layout}">
			<!-- Fields -->
			<div class="fields-panel">
				<div class="fields-header">Properties</div>
				{#each schema.fields as field (field.key)}
					{#if field.computed}
						<div class="field-row">
							<span class="field-label">{field.label}</span>
							<div class="field-value">
								{#if field.type === 'number' && field.suffix === '%'}
									{@const pct = Math.min(100, Math.max(0, Number(fieldValue(field.key)) || 0))}
									{@const done = computedOverrides._progressDone}
									{@const total = computedOverrides._progressTotal}
									<div class="progress-bar">
										<div class="progress-fill" style:width="{pct}%"></div>
										<span class="progress-text">
											{#if total != null}
												{done}/{total} tasks · {pct}%
											{:else}
												{pct}%
											{/if}
										</span>
									</div>
								{:else}
									<span class="computed-value">{formatFieldDisplay(fieldValue(field.key))}</span>
								{/if}
							</div>
						</div>
					{:else}
						{@const rawFieldValue = fieldValue(field.key)}
						{@const isFieldEmpty = rawFieldValue == null
							|| rawFieldValue === ''
							|| (Array.isArray(rawFieldValue) && rawFieldValue.length === 0)}
						<div class="field-row" class:print-empty={isFieldEmpty}>
							<span class="field-label">{field.label}</span>
							<div class="field-value">
								<FieldEditor
									{field}
									value={rawFieldValue}
									onchange={(v) => updateField(field.key, v)}
									readonly={!canEdit}
								/>
							</div>
						</div>
					{/if}
				{/each}

				<!-- Tags (item.tags — spans collections, not a schema field) -->
				<div class="field-row">
					<span class="field-label">Tags</span>
					<div class="field-value">
						<TagInput
							{tags}
							suggestions={tagSuggestions}
							onchange={updateTags}
							readonly={!canEdit}
						/>
					</div>
				</div>

				<!-- Assignment: user + role -->
				{#if workspaceMembers.length > 0 || agentRoles.length > 0}
					<div class="fields-header" style="margin-top: var(--space-4)">Assignment</div>
				{/if}
				{#if workspaceMembers.length > 0}
					<div class="field-row">
						<span class="field-label">Assigned to</span>
						<div class="field-value">
							{#if canEdit}
								<select
									class="assignment-select"
									value={item.assigned_user_id ?? ''}
									onchange={(e) => {
										const val = (e.target as HTMLSelectElement).value;
										updateAssignedUser(val || null);
									}}
								>
									<option value="">Unassigned</option>
									{#each workspaceMembers as member (member.user_id)}
										<option value={member.user_id}>{member.user_name}</option>
									{/each}
								</select>
							{:else}
								<!-- Read-only display (PLAN-1100 / TASK-1105). Assignment
								     mutations go through api.items.update which is
								     server-gated on edit permission. -->
								<span class="assignment-readonly">
									{#if item?.assigned_user_id}
										{workspaceMembers.find(m => m.user_id === item!.assigned_user_id)?.user_name ?? '—'}
									{:else}
										Unassigned
									{/if}
								</span>
							{/if}
						</div>
					</div>
				{/if}
				{#if agentRoles.length > 0}
					<div class="field-row">
						<span class="field-label">Role</span>
						<div class="field-value">
							{#if canEdit}
								<select
									class="assignment-select"
									value={item.agent_role_id ?? ''}
									onchange={(e) => {
										const val = (e.target as HTMLSelectElement).value;
										updateAgentRole(val || null);
									}}
								>
									<option value="">No role</option>
									{#each agentRoles as role (role.id)}
										<option value={role.id}>{role.icon ? role.icon + ' ' : ''}{role.name}</option>
									{/each}
								</select>
							{:else}
								<span class="assignment-readonly">
									{#if item?.agent_role_id}
										{@const r = agentRoles.find(r => r.id === item!.agent_role_id)}
										{r ? `${r.icon ? r.icon + ' ' : ''}${r.name}` : '—'}
									{:else}
										No role
									{/if}
								</span>
							{/if}
						</div>
					</div>
				{/if}
			</div>

			<!-- Content editor -->
			<div class="content-panel">
				<div class="editor-mode-toggle">
					<button
						class="mode-btn"
						class:active={!rawMode}
						onclick={async () => {
							// Flush any pending raw debounce SYNCHRONOUSLY
							// before the collab provider mints; otherwise
							// the deferred PATCH fires post-mint and gets
							// routed through the applier path.
							// Stay in raw mode if the flush failed — the
							// user retains their unsaved edits to retry
							// or copy out. Per Codex review round 6.
							const ok = await flushRawIfPending();
							if (ok) {
								rawSeedMarkdown = null;
								rawMode = false;
							}
						}}
						title="Rich text editor"
					>Rich</button>
					<button
						class="mode-btn"
						class:active={rawMode}
						onclick={async () => {
							// Toggle rich+collab → raw. We need to
							// land the live Y.Doc state in
							// items.content BEFORE activating raw
							// mode so the raw editor (and any
							// subsequent navigation) sees the
							// current markdown. The provider stays
							// connected during the await, which means
							// a concurrent peer (e.g. same user's
							// other tab) can keep editing the Y.Doc
							// while we flush — those edits would be
							// lost from the seed if we captured md
							// once. Loop-flush until stable: re-read
							// the editor's current markdown after
							// each PATCH; if it changed, flush again.
							// Capped at 3 iterations to bound the
							// transition under aggressive concurrent
							// typing. Per Codex review round 5.
							if (collabProvider && editorInstance && item) {
								const ws = wsSlug;
								const itemId = item.id;
								const ed = editorInstance;
								try {
									// keepalive=false on the explicit
									// toggle — the await is
									// foreground/synchronous and
									// keepalive's ~64KB body cap can
									// reject larger payloads,
									// silently leaving items.content
									// stale. Cleanup-driven flushes
									// still use keepalive=true; this
									// path doesn't need it because
									// the user clicked a button and
									// is expecting to wait. Per
									// Codex review round 8.
									let md = (ed.storage as any).markdown?.getMarkdown?.();
									let lastFlushed: string | null = null;
									let aborted = false;
									for (let i = 0; i < 3; i++) {
										if (typeof md !== 'string') break;
										const result = await runCollabFlush(ws, itemId, md, false);
										if (result === 'failed' || result === 'skipped') {
											// 'failed' — PATCH errored;
											//   runCollabFlush already
											//   surfaced the toast.
											// 'skipped' — force_refresh
											//   recovery is in flight,
											//   the Y.Doc-derived md is
											//   known stale (TASK-1319
											//   round 9 [P1]); seeding
											//   raw mode from it would
											//   silently overwrite the
											//   canonical server content
											//   on the next raw save.
											// Either way, refuse to
											// enter raw mode.
											if (result === 'skipped') {
												toastStore.show(
													'Editor is recovering — try Markdown again in a moment.',
													'info',
												);
											}
											aborted = true;
											break;
										}
										// 'flushed' or 'deduped' —
										// items.content matches md.
										// Treat both as a successful
										// flush for seeding purposes.
										lastFlushed = md;
										if (result === 'deduped') break;
										const mdAfter = (ed.storage as any).markdown?.getMarkdown?.();
										if (mdAfter === md) break;
										md = mdAfter;
									}
									if (aborted) return;
									// Bail if the user navigated to a
									// different item while we were
									// awaiting flushes — applying
									// rawMode + rawSeedMarkdown to
									// the new page would be wrong.
									// Per Codex review round 7.
									if (!item || item.id !== itemId) return;
									if (lastFlushed !== null) rawSeedMarkdown = lastFlushed;
								} catch {
									// Fall through; RawMarkdownEditor
									// will seed from item.content.
								}
							}
							// Cancel any pending timer-driven flush
							// scheduled by edits during the await
							// window — left armed, it would fire
							// post-rawMode and PATCH a stale rich
							// markdown over a subsequent raw save.
							// Per Codex review round 5.
							clearTimeout(collabFlushTimer);
							collabFlushTimer = undefined;
							rawMode = true;
						}}
						title="Raw markdown editor"
					>Markdown</button>
				</div>
				{#if rawMode}
					{#key item.id}
						<RawMarkdownEditor content={rawSeedMarkdown ?? item.content ?? ''} onUpdate={handleRawContentUpdate} readonly={!canEdit} />
					{/key}
				{:else}
					<!--
						Key on canEdit so the Editor is reconstructed when the user
						gains/loses edit permission. BlockDragHandle (and any future
						extensions whose registration is gated on `editable`) are
						decided at construction time — without re-keying, an editor
						mounted before /me resolves would never gain the drag handle
						even after canEdit flips true. Per Codex review round 4.
					-->
					<!--
						Editable users mount the Editor only AFTER the Y.Doc
						is constructed by the $effect above — Editor.svelte
						decides its extension list once at onMount time
						(StarterKit history vs Collaboration), so a mount
						with `ydoc=undefined` would never gain Collaboration
						even after the prop later flips. The conditional
						gate adds at most a single sub-frame delay (the
						$effect runs in the same reactive cycle) and
						guarantees the first mount has the binding
						registered. Per Codex review round 1.

						View-only viewers (canEdit === false) keep mounting
						immediately under the legacy non-collab path; their
						read-only y-binding is deferred to TASK-1266.
					-->
					{#if !canEdit}
						{#key `${item.id}:false`}
							<Editor
								content={editorContent}
								onUpdate={handleContentUpdate}
								editable={false}
								itemId={item.id}
								onEditor={(e) => editorInstance = e}
								onImportInserted={handleImportInserted}
							/>
						{/key}
					{:else if ydoc}
						<!--
							Error gate FIRST so a stuck-connecting condition surfaces
							error UI instead of a perpetual shimmer. Both branches
							that fire here imply `!hasEverSynced` (staleConnecting's
							effect only arms its timer when !hasEverSynced; offline +
							hasEverSynced is handled below). That invariant is what
							makes `retryCollabSync` safe to call: with no prior sync,
							the current Y.Doc cannot hold user edits, so tearing down
							the provider can't lose unflushed work. The offline +
							hasEverSynced case deliberately KEEPS the editor mounted
							— the corner badge (line ~1700) already signals offline,
							the existing reconnect loop in CollabProvider keeps
							trying, and any in-progress user edits remain bound to
							the live Y.Doc. Per Codex review round 2 of TASK-1376.
						-->
						{#if (collabProvider?.state === 'offline' && !hasEverSynced) || staleConnecting}
							<ContentError
								title="Content unavailable"
								detail="Could not sync with the server. Reload the editor to try again."
								onRetry={retryCollabSync}
							/>
						{:else if collabProvider?.state === 'connecting' && !hasEverSynced}
							<ContentSkeleton variant="inline" />
						{:else}
							{#key `${item.id}:true:${forceRefreshNonce}`}
								<Editor
									content={editorContent}
									onUpdate={handleContentUpdate}
									editable={true}
									itemId={item.id}
									ydoc={ydoc}
									awareness={collabProvider?.awareness}
									collabUser={collabUserState}
									onEditor={(e) => editorInstance = e}
									onImportInserted={handleImportInserted}
								/>
							{/key}
						{/if}
					{/if}
					{#if canEdit}
						<EditorBubbleMenu
							editor={editorInstance}
							{wsSlug}
							collections={collectionStore.collections}
							onItemCreated={() => collectionStore.loadItems(wsSlug)}
						/>
						<EditorLinkPopover editor={editorInstance} />
					{/if}
				{/if}
			</div>
		</div>

		{#if relationshipGroups.length > 0}
			<div class="relationships-section">
				<h3 class="section-title">Relationships</h3>
				<div class="relationship-groups">
					{#each relationshipGroups as group (group.label)}
						<div class="relationship-group">
							<h4 class="relationship-group-title">{group.label}</h4>
							{#if group.closureSummary}
								<p class="closure-inline-summary">✓ {group.closureSummary}</p>
							{/if}
							<div class="links-list">
								{#each group.entries as entry (entry.key)}
									<div class="link-row" class:tone-blocks={group.tone === 'blocks'} class:tone-wiki={group.tone === 'wiki'} class:tone-lineage={group.tone === 'lineage'}>
										{#if entry.href}
											<a href={entry.href} class="link-target">{entry.label}</a>
										{:else}
											<span class="link-target">{entry.label}</span>
										{/if}
										<span class="link-row-actions">
											{#if entry.status}
												<span class="link-status">{formatFieldDisplay(entry.status)}</span>
											{/if}
											{#if entry.linkId && canEdit}
												<button class="link-delete-btn" title="Remove relationship" onclick={() => handleDeleteLink(entry.linkId)}>×</button>
											{/if}
										</span>
									</div>
								{/each}
							</div>
						</div>
					{/each}
				</div>
			</div>
		{/if}

		<!-- Add Relationship — gated on canEdit (PLAN-1100 / TASK-1105). -->
		{#if item && canEdit}
			<div class="add-relationship-section">
				{#if !showAddLink}
					<button class="add-relationship-btn" onclick={() => { showAddLink = true; }}>
						+ Add relationship
					</button>
				{:else}
					<div class="add-link-form">
						<div class="add-link-header">
							<h4>Add Relationship</h4>
							<button class="add-link-close" onclick={() => { showAddLink = false; addLinkSearch = ''; addLinkResults = []; }}>×</button>
						</div>
						<div class="add-link-controls">
							<select bind:value={addLinkType} class="add-link-type-select">
								<option value="related">Related</option>
								<option value="blocks">Blocks</option>
								<option value="implements">Implements</option>
								<option value="split_from">Split from</option>
								<option value="supersedes">Supersedes</option>
								<option value="parent">Parent</option>
							</select>
							<input
								type="text"
								class="add-link-search"
								placeholder="Search items..."
								bind:value={addLinkSearch}
								oninput={() => searchItemsForLink()}
							/>
						</div>
						{#if addLinkLoading}
							<div class="add-link-loading">Searching...</div>
						{:else if addLinkResults.length > 0}
							<div class="add-link-results">
								{#each addLinkResults as result (result.id)}
									<button class="add-link-result" onclick={() => handleCreateLink(result)}>
										{#if formatItemRef(result)}
											<span class="add-link-ref">{formatItemRef(result)}</span>
										{/if}
										<span class="add-link-title">{result.title}</span>
									</button>
								{/each}
							</div>
						{:else if addLinkSearch.trim().length > 0}
							<div class="add-link-loading">No results</div>
						{/if}
					</div>
				{/if}
			</div>
		{/if}

		<!-- Child Items: always mounted so SSE subscriptions stay active even when starting with 0 children -->
		{#if item}
			<ChildItems {wsSlug} {username} {itemSlug} itemId={item.id} parentFields={fields} terminalStatuses={childTerminalStatuses} onChildrenChange={handleChildrenChange} {canEdit} />
		{/if}

		<!--
			Mentioned-in panel (PLAN-1593 / TASK-1596). Placed between
			child items and the timeline so the reading order matches the
			page's narrative arc: this item, its decomposition (children),
			what references it (backlinks), then the activity stream. The
			panel collapses entirely when there are zero backlinks — no
			empty header, no whitespace waste — and notifies the badge via
			the count callback. Anchor id is referenced by the "📎 N"
			action button's smooth scroll.
		-->
		{#if item}
			<div id="item-backlinks">
				<BacklinksPanel
					{wsSlug}
					{username}
					{itemSlug}
					itemId={item.id}
					onCountChange={(n) => { backlinksCount = n; }}
				/>
			</div>
		{/if}

		<!-- Unified Timeline (comments + activity + versions) -->
		<div id="item-timeline" class="timeline-section">
			<ItemTimeline
				{wsSlug}
				{username}
				{itemSlug}
				currentContent={item.content ?? ''}
				items={collectionStore.items ?? []}
				onRestore={handleVersionRestore}
				itemId={item.id}
				collectionId={item.collection_id}
			/>
		</div>

	</div>

	{#if isOwner && item}
		<ShareDialog
			{wsSlug}
			type="item"
			targetSlug={item.slug}
			targetName={formatItemRef(item) || item.title}
			bind:open={shareDialogOpen}
		/>
	{/if}

	{#if isOwner && collection}
		<EditCollectionModal
			bind:open={editCollectionOpen}
			{collection}
			{wsSlug}
			initialSection={editCollectionSection}
			onupdated={(updated) => {
				if (!updated) {
					// Archive case — the collection is gone. Navigate away from
					// this now-invalid item route rather than leaving the user
					// with stale state that would hit deleted resources.
					collectionStore.loadCollections(wsSlug);
					void goto(`/${username}/${wsSlug}`);
					return;
				}
				collection = updated;
				collectionStore.loadCollections(wsSlug);
				// If the owner renamed the collection, its slug may have
				// changed. The current `/[collection]/[slug]` URL still
				// points at the old slug and subsequent loadData() calls
				// (which fetch by collSlug) would 404. Navigate to the
				// new slug while preserving the item slug. The new route
				// will trigger its own loadData() via the $effect on
				// wsSlug/collSlug/itemSlug, so no explicit refresh here.
				if (updated.slug !== collSlug && itemSlug) {
					void goto(`/${username}/${wsSlug}/${updated.slug}/${itemSlug}`);
					return;
				}
				// Non-navigating update: schema or field mappings may have
				// changed (rename / migration), so reload the item so fields
				// reflect the new shape. Without this, a subsequent
				// updateField() would write stale fields JSON back and
				// clobber migrated values.
				void loadData();
			}}
			onclose={() => {
				editCollectionOpen = false;
				editCollectionSection = undefined;
			}}
		/>
	{/if}
{/if}

<style>
	.center-message {
		display: flex;
		align-items: center;
		justify-content: center;
		height: 50vh;
		color: var(--text-muted);
	}

	.item-page {
		max-width: var(--content-max-width);
		margin: 0 auto;
		padding: var(--space-6) var(--space-6) var(--space-10);
	}

	.sticky-header {
		position: sticky;
		top: 0;
		z-index: 10;
		background: var(--bg-primary);
		margin: 0 calc(-1 * var(--space-6));
		padding: var(--space-2) var(--space-6);
		border-bottom: 1px solid transparent;
		transition: border-color 0.15s ease;
	}
	/*
		Historical note (TASK-1124): there used to be a `@media (max-width:
		768px) { .sticky-header { top: 45px; } }` rule here that pushed the
		breadcrumb 45px below the top of its scroll container — to clear the
		old slim `.mobile-header` (hamburger + switcher) that lived inside
		.main-content on mobile. After the IDEA-1121 mobile chrome
		consolidation, that header no longer exists; the topbar is now
		`position: fixed` outside .main-content and .app-layout pads itself
		by --topbar-height on mobile, so the breadcrumb's scroll container
		already starts below the topbar. `top: 0` is correct on both
		surfaces now — the override is removed to close the gap that the
		stale 45px offset was producing.
	*/

	/* Breadcrumb */
	.breadcrumb {
		display: flex;
		align-items: center;
		gap: var(--space-1);
		font-size: 0.85em;
		color: var(--text-muted);
		margin-bottom: 0;
	}
	.breadcrumb a {
		color: var(--text-secondary);
		text-decoration: none;
	}
	.breadcrumb a:hover {
		color: var(--accent-blue);
		text-decoration: underline;
	}
	.sep { color: var(--text-muted); }
	.current { color: var(--text-primary); }
	.copy-ref-btn {
		position: relative;
		display: inline-flex;
		align-items: center;
		justify-content: center;
		width: 24px;
		height: 24px;
		padding: 0;
		margin-left: 2px;
		background: none;
		border: none;
		border-radius: var(--radius-sm);
		color: var(--text-muted);
		cursor: pointer;
		transition: color 0.15s ease, background 0.15s ease;
	}
	.copy-ref-btn:hover {
		color: var(--text-primary);
		background: var(--bg-tertiary);
	}
	.copied-tooltip {
		position: absolute;
		top: 100%;
		left: 50%;
		transform: translateX(-50%);
		margin-top: 4px;
		padding: 2px 8px;
		font-size: 0.75em;
		font-family: var(--font-sans);
		color: var(--text-on-accent);
		background: var(--accent-green, #22c55e);
		border-radius: var(--radius-sm);
		white-space: nowrap;
		pointer-events: none;
		z-index: 20;
	}

	/* Title */
	.title-row { margin-bottom: var(--space-2); display: flex; align-items: baseline; gap: var(--space-2); }
	.header-tags {
		display: flex;
		flex-wrap: wrap;
		gap: var(--space-1, 0.25rem);
		margin-bottom: var(--space-2);
	}
	.header-tag {
		display: inline-flex;
		align-items: center;
		padding: 0.1em 0.55em;
		font-size: 0.75em;
		line-height: 1.5;
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: 999px;
		color: var(--text-secondary);
		text-decoration: none;
		white-space: nowrap;
	}
	.header-tag:hover {
		color: var(--text-primary);
		border-color: var(--text-tertiary, var(--text-secondary));
	}
	.item-ref {
		font-family: var(--font-mono);
		font-size: 0.85em;
		color: var(--text-muted);
		background: var(--bg-tertiary);
		padding: 2px 8px;
		border-radius: var(--radius-sm);
		white-space: nowrap;
		flex-shrink: 0;
	}
	.title {
		display: block;
		font-size: 1.6em;
		font-weight: 700;
		cursor: text;
		border-radius: var(--radius);
		padding: 2px 4px;
		margin: -2px -4px;
		text-align: left;
		width: 100%;
		color: var(--text-primary);
		background: none;
		border: none;
	}
	.title:hover {
		background: var(--bg-secondary);
	}
	/* Read-only title for users without canEditItem (PLAN-1100 / TASK-1105).
	   Removes the click-to-edit affordance: no hover background, no
	   text-cursor, default heading flow. */
	.title-readonly {
		cursor: default;
		margin: 0;
		font-weight: 700;
	}
	.title-readonly:hover {
		background: none;
	}
	/* Assignment fallback display for read-only mode. Mirrors the height
	   and padding of the live `.assignment-select` so the row doesn't
	   reflow when the user gains/loses edit permission. */
	.assignment-readonly {
		display: inline-flex;
		align-items: center;
		min-height: 30px;
		color: var(--text-primary);
		font-size: 0.88em;
	}
	.title-input {
		font-size: 1.6em;
		font-weight: 700;
		width: 100%;
		background: var(--bg-secondary);
		border: 1px solid var(--accent-blue);
		border-radius: var(--radius);
		padding: 2px 4px;
		color: var(--text-primary);
		resize: none;
		overflow: hidden;
		line-height: 1.3;
		font-family: inherit;
	}

	/* Refresh-from-source chip (TASK-1474). Lives in .title-row right
	   of the title; small, unobtrusive, click to re-fetch. */
	.source-chip {
		display: inline-flex;
		align-items: center;
		gap: 4px;
		padding: 2px 8px;
		border-radius: 999px;
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		font-size: 0.75em;
		color: var(--text-secondary);
		cursor: pointer;
		font-family: inherit;
	}
	.source-chip:hover:not(:disabled) {
		background: var(--bg-tertiary, var(--bg-secondary));
		color: var(--text-primary);
	}
	.source-chip:disabled {
		opacity: 0.6;
		cursor: progress;
	}
	.source-chip-readonly {
		cursor: default;
	}
	.source-chip-icon {
		font-size: 0.9em;
		line-height: 1;
	}

	/* Meta */
	.meta-info {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		font-size: 0.8em;
		color: var(--text-muted);
		margin-bottom: var(--space-2);
		flex-wrap: wrap;
	}
	.meta-sep { color: var(--text-muted); }
	.save-status {
		font-size: 0.85em;
		margin-left: var(--space-2);
		opacity: 0;
		transition: opacity 0.2s;
	}
	.save-status.visible { opacity: 1; }
	.save-status.saving { color: var(--text-muted); }
	.save-status.saved { color: var(--accent-green); }

	/* Collab connection state badge (TASK-1264). Always-visible while
	   the WS provider exists; colour communicates state. The dot is the
	   primary signal, the label exists for accessibility and clarity
	   but is small and unobtrusive per the Plan body's "no flashing,
	   green dot" guidance. */
	.collab-state {
		display: inline-flex;
		align-items: center;
		gap: 0.35em;
		font-size: 0.85em;
		margin-left: var(--space-2);
		color: var(--text-muted);
	}
	.collab-state-dot {
		width: 0.55em;
		height: 0.55em;
		border-radius: 50%;
		background: var(--text-muted);
	}
	.collab-state-synced { color: var(--accent-green); }
	.collab-state-synced .collab-state-dot { background: var(--accent-green); }
	.collab-state-connecting,
	.collab-state-reconnecting { color: var(--accent-yellow, #d4a017); }
	.collab-state-connecting .collab-state-dot,
	.collab-state-reconnecting .collab-state-dot {
		background: var(--accent-yellow, #d4a017);
	}
	.collab-state-offline { color: var(--accent-red, #c0392b); }
	.collab-state-offline .collab-state-dot {
		background: var(--accent-red, #c0392b);
	}

	/* Layout variants */
	.item-body {
		display: flex;
		flex-direction: column;
		gap: var(--space-6);
	}

	.layout-balanced .fields-panel {
		display: grid;
		grid-template-columns: 1fr 1fr;
		gap: 0 var(--space-6);
		padding-bottom: var(--space-4);
		border-bottom: 1px solid var(--border);
	}
	.layout-balanced .fields-header {
		grid-column: 1 / -1;
	}
	.layout-balanced .field-row:last-child {
		border-bottom: none;
	}

	.layout-fields-primary .fields-panel {
		order: -1;
	}

	/* Content-primary: fields as compact horizontal row */
	.layout-content-primary .fields-panel {
		display: flex;
		flex-wrap: wrap;
		gap: var(--space-2);
		padding-bottom: var(--space-4);
		border-bottom: 1px solid var(--border);
	}
	.layout-content-primary .fields-header {
		display: none;
	}
	.layout-content-primary .field-row {
		flex-direction: row;
		align-items: center;
		gap: var(--space-2);
		padding: 0;
		border: none;
	}
	.layout-content-primary .field-label {
		font-size: 0.75em;
		white-space: nowrap;
	}
	/* Fields panel */
	.fields-panel {
		display: flex;
		flex-direction: column;
		gap: 0;
	}
	.fields-header {
		font-size: 0.7em;
		font-weight: 600;
		text-transform: uppercase;
		letter-spacing: 0.05em;
		color: var(--text-muted);
		padding: var(--space-2) 0;
		margin-bottom: var(--space-1);
	}
	.field-row {
		display: flex;
		align-items: center;
		gap: var(--space-3);
		padding: var(--space-2) 0;
		border-bottom: 1px solid color-mix(in srgb, var(--border) 50%, transparent);
	}
	.field-row:last-child {
		border-bottom: none;
	}
	.field-label {
		font-size: 0.82em;
		color: var(--text-secondary);
		font-weight: 500;
		width: 90px;
		flex-shrink: 0;
	}
	.field-value {
		flex: 1;
		min-width: 0;
	}
	.computed-value {
		font-size: 0.88em;
		color: var(--text-secondary);
	}
	.assignment-select {
		width: 100%;
		padding: 5px 8px;
		font-size: 0.88em;
		font-family: inherit;
		color: var(--text-primary);
		background: var(--bg-tertiary);
		border: 1px solid var(--border);
		border-radius: var(--radius-sm);
		cursor: pointer;
		appearance: auto;
	}
	.assignment-select:hover {
		border-color: var(--accent-blue);
	}
	.assignment-select:focus {
		outline: 2px solid var(--accent-blue);
		outline-offset: -1px;
	}
	.progress-bar {
		position: relative;
		height: 22px;
		background: var(--bg-tertiary);
		border-radius: var(--radius-sm);
		overflow: hidden;
	}
	.progress-fill {
		height: 100%;
		background: var(--accent-blue);
		opacity: 0.25;
		border-radius: var(--radius-sm);
		transition: width 0.3s ease;
	}
	.progress-text {
		position: absolute;
		inset: 0;
		display: flex;
		align-items: center;
		justify-content: center;
		font-size: 0.8em;
		font-weight: 500;
		color: var(--text-primary);
	}

	/* Content */
	.content-panel {
		min-height: 300px;
	}

	.editor-mode-toggle {
		display: flex;
		gap: 1px;
		margin-bottom: var(--space-3);
		background: var(--bg-tertiary);
		border-radius: var(--radius-sm);
		padding: 2px;
		width: fit-content;
	}

	.mode-btn {
		padding: var(--space-1) var(--space-3);
		font-size: 0.75em;
		font-weight: 500;
		color: var(--text-muted);
		background: none;
		border: none;
		border-radius: var(--radius-sm);
		cursor: pointer;
		transition: color 0.15s, background 0.15s;
	}

	.mode-btn:hover {
		color: var(--text-secondary);
	}

	.mode-btn.active {
		background: var(--bg-secondary);
		color: var(--text-primary);
		box-shadow: 0 1px 2px rgba(0, 0, 0, 0.1);
	}

	/* Code context */
	.code-context-section {
		margin-bottom: var(--space-6);
	}
	.code-context-card {
		padding: var(--space-4);
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		display: flex;
		flex-direction: column;
		gap: var(--space-3);
	}
	.code-context-meta {
		display: flex;
		flex-wrap: wrap;
		gap: var(--space-2);
		align-items: center;
	}
	.code-provider {
		font-size: 0.8em;
		font-weight: 700;
		text-transform: uppercase;
		letter-spacing: 0.04em;
		color: var(--accent-blue);
	}
	.code-chip {
		font-family: var(--font-mono);
		font-size: 0.8em;
		color: var(--text-secondary);
		background: var(--bg-tertiary);
		padding: 2px 8px;
		border-radius: 999px;
	}
	.code-pr-row {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: var(--space-3);
		flex-wrap: wrap;
	}
	.code-pr-link {
		font-weight: 600;
		color: var(--text-primary);
		text-decoration: none;
	}
	.code-pr-link:hover {
		color: var(--accent-blue);
		text-decoration: underline;
	}
	.code-pr-state {
		font-size: 0.75em;
		font-weight: 600;
		color: var(--text-muted);
		background: var(--bg-tertiary);
		padding: 2px 8px;
		border-radius: 999px;
	}
	.code-pr-updated {
		font-size: 0.8em;
		color: var(--text-muted);
	}

	.closure-inline-summary {
		margin: 0 0 var(--space-2) 0;
		font-size: 0.8em;
		color: var(--accent-green);
		font-weight: 500;
	}

	/* Relationships */
	.relationships-section {
		margin-top: var(--space-6);
		padding-top: var(--space-6);
		border-top: 1px solid var(--border);
	}
	.section-title {
		font-size: 0.8em;
		font-weight: 600;
		text-transform: uppercase;
		letter-spacing: 0.06em;
		color: var(--text-muted);
		margin-bottom: var(--space-3);
	}
	.links-list {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
	}
	.relationship-groups {
		display: flex;
		flex-direction: column;
		gap: var(--space-4);
	}
	.relationship-group {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
	}
	.relationship-group-title {
		margin: 0;
		font-size: 0.95em;
		font-weight: 600;
		color: var(--text-secondary);
	}
	.link-row {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: var(--space-3);
		padding: var(--space-2) var(--space-3);
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		font-size: 0.9em;
		flex-wrap: wrap;
	}
	.link-row.tone-blocks {
		border-left: 3px solid var(--accent-orange);
	}
	.link-row.tone-wiki {
		border-left: 3px solid var(--accent-blue);
	}
	.link-row.tone-lineage {
		border-left: 3px solid var(--accent-green);
	}
	.link-target {
		font-weight: 500;
		color: var(--text-primary);
		text-decoration: none;
	}
	.link-target:hover {
		color: var(--accent-blue);
		text-decoration: underline;
	}
	.link-status {
		font-size: 0.75em;
		font-weight: 600;
		color: var(--text-muted);
		background: var(--bg-tertiary);
		padding: 2px 8px;
		border-radius: 999px;
		white-space: nowrap;
	}
	.link-row-actions {
		display: flex;
		align-items: center;
		gap: var(--space-2);
	}
	.link-delete-btn {
		display: none;
		background: none;
		border: none;
		color: var(--text-muted);
		cursor: pointer;
		padding: 0 var(--space-1);
		font-size: 1rem;
		line-height: 1;
	}
	.link-delete-btn:hover {
		color: var(--danger);
	}
	.link-row:hover .link-delete-btn {
		display: inline;
	}

	/* Add Relationship */
	.add-relationship-section {
		margin-top: var(--space-4);
	}
	.add-relationship-btn {
		background: none;
		border: 1px dashed var(--border-color);
		color: var(--text-muted);
		padding: var(--space-2) var(--space-3);
		border-radius: var(--radius-md);
		cursor: pointer;
		font-size: 0.85rem;
		width: 100%;
		text-align: left;
	}
	.add-relationship-btn:hover {
		color: var(--text-primary);
		border-color: var(--text-muted);
	}
	.add-link-form {
		border: 1px solid var(--border-color);
		border-radius: var(--radius-md);
		padding: var(--space-3);
		background: var(--bg-secondary);
	}
	.add-link-header {
		display: flex;
		justify-content: space-between;
		align-items: center;
		margin-bottom: var(--space-2);
	}
	.add-link-header h4 {
		margin: 0;
		font-size: 0.85rem;
		font-weight: 600;
	}
	.add-link-close {
		background: none;
		border: none;
		color: var(--text-muted);
		cursor: pointer;
		font-size: 1.2rem;
		padding: 0;
		line-height: 1;
	}
	.add-link-controls {
		display: flex;
		gap: var(--space-2);
		margin-bottom: var(--space-2);
	}
	.add-link-type-select {
		padding: var(--space-1) var(--space-2);
		border: 1px solid var(--border-color);
		border-radius: var(--radius-sm);
		background: var(--bg-primary);
		color: var(--text-primary);
		font-size: 0.8rem;
		min-width: 120px;
	}
	.add-link-search {
		flex: 1;
		padding: var(--space-1) var(--space-2);
		border: 1px solid var(--border-color);
		border-radius: var(--radius-sm);
		background: var(--bg-primary);
		color: var(--text-primary);
		font-size: 0.8rem;
	}
	.add-link-results {
		display: flex;
		flex-direction: column;
		gap: 1px;
		max-height: 200px;
		overflow-y: auto;
	}
	.add-link-result {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		padding: var(--space-2);
		background: var(--bg-primary);
		border: none;
		border-radius: var(--radius-sm);
		cursor: pointer;
		text-align: left;
		color: var(--text-primary);
		font-size: 0.8rem;
	}
	.add-link-result:hover {
		background: var(--bg-hover);
	}
	.add-link-ref {
		color: var(--text-muted);
		font-family: var(--font-mono);
		font-size: 0.75rem;
		flex-shrink: 0;
	}
	.add-link-title {
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}
	.add-link-loading {
		padding: var(--space-2);
		color: var(--text-muted);
		font-size: 0.8rem;
	}

	/* Timeline */
	.timeline-section {
		margin-top: var(--space-6);
		padding-top: var(--space-6);
		border-top: 1px solid var(--border);
	}

	/* History */
	.meta-actions {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		margin-bottom: var(--space-6);
		flex-wrap: wrap;
	}
	.action-btn {
		padding: var(--space-1) var(--space-3);
		min-width: 70px;
		text-align: center;
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		color: var(--text-secondary);
		font-size: 0.85em;
		cursor: pointer;
		transition: all 0.1s;
		white-space: nowrap;
	}
	.action-btn:hover {
		background: var(--bg-tertiary);
		color: var(--text-primary);
	}
	.action-btn.active {
		background: var(--accent-blue);
		border-color: var(--accent-blue);
		color: #fff;
	}
	.star-btn.starred {
		color: var(--accent-amber);
	}
	.star-btn:hover {
		color: var(--accent-amber);
	}
	.move-wrapper {
		position: relative;
	}
	.move-dropdown {
		position: absolute;
		top: 100%;
		right: 0;
		margin-top: var(--space-1);
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		box-shadow: 0 4px 12px rgba(0, 0, 0, 0.3);
		z-index: 100;
		min-width: 180px;
		padding: var(--space-1);
	}
	.move-option {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		width: 100%;
		padding: var(--space-2) var(--space-3);
		background: none;
		border: none;
		color: var(--text-primary);
		font-size: 0.85em;
		cursor: pointer;
		border-radius: var(--radius-sm);
		text-align: left;
	}
	.move-option:hover {
		background: var(--bg-hover);
	}
	.move-icon {
		font-size: 1.1em;
	}
	/* Inside the mobile BottomSheet the options sit in the normal document
	   flow — give them a little breathing room vs the dropdown padding. */
	.move-sheet-body {
		padding: 0 var(--space-2) var(--space-3);
	}
	.move-sheet-body .move-option {
		padding: var(--space-3);
		font-size: 1em;
	}
	.delete-btn:hover {
		color: var(--accent-orange);
		border-color: var(--accent-orange);
	}
	.delete-confirm {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		font-size: 0.85em;
		color: var(--accent-orange);
		font-weight: 500;
	}
	.delete-confirm-btn {
		padding: 2px var(--space-2);
		border-radius: var(--radius);
		font-size: 0.85em;
		cursor: pointer;
		border: 1px solid var(--border);
		background: var(--bg-secondary);
		color: var(--text-secondary);
	}
	.delete-confirm-btn.yes {
		color: var(--accent-orange);
		border-color: var(--accent-orange);
	}
	.delete-confirm-btn.yes:hover {
		background: var(--accent-orange);
		color: #fff;
	}
	.delete-confirm-btn.no:hover {
		background: var(--bg-tertiary);
		color: var(--text-primary);
	}
	.delete-confirm-btn:disabled {
		opacity: 0.5;
		cursor: not-allowed;
	}
	@media (max-width: 768px) {
		.layout-balanced .fields-panel {
			grid-template-columns: 1fr;
		}
	}

	/* Print footer (fixed-positioned, visible only in print). The
	   page-1 document header lives in normal flow via `.title-row` +
	   `.meta-info` — no repeating print header. See BUG-626. */
	.print-footer {
		display: none;
	}

	/* =============================================================
	   PRINT — item detail page (PLAN-620 / TASK-622).
	   Layout-level chrome is hidden by app.css's base @media print
	   block (TASK-621); this block formats the item page CONTENT:
	   title, metadata, properties-as-definition-list, markdown body.
	   Comments / activity / version history are stripped. Relationships
	   and code context stay because they're document-level context.
	   ============================================================= */
	@media print {
		.item-page {
			max-width: none;
			margin: 0;
			padding: 0;
		}

		/* Hide interactive / screen-only chrome inside the item page. */
		.sticky-header,
		.meta-actions,
		.editor-mode-toggle,
		.add-relationship-section,
		.save-status,
		.collab-state,
		.copy-ref-btn,
		.copied-tooltip,
		.link-delete-btn {
			display: none !important;
		}
		#item-timeline,
		.timeline-section {
			display: none !important;
		}

		/* Title row — page-1 document header block (BUG-626).
		   DOM order is [item-ref, title]; in print we want [title, ref]
		   visually, so `order` flips the display sequence without
		   touching the template. Title takes the remaining width and
		   wraps naturally; `.item-ref` stays compact on the right,
		   baseline-aligned to the title's first line. */
		.title-row {
			display: flex;
			justify-content: flex-start;
			align-items: baseline;
			gap: 12pt;
			margin: 0 0 4pt 0;
			padding: 0;
			border: none;
		}
		.title-row .item-ref {
			order: 2;
			flex: 0 0 auto;
			font-size: 10pt;
			font-weight: 500;
			color: #555;
			margin-left: auto;
			padding: 0;
			background: transparent;
			border: none;
			letter-spacing: 0.03em;
			font-variant-numeric: tabular-nums;
			white-space: nowrap;
		}
		.title-row .title,
		.title-row .title-input {
			order: 1;
			flex: 1 1 auto;
			display: block;
			font-size: 20pt;
			font-weight: 700;
			color: #000;
			background: transparent;
			border: none;
			padding: 0;
			margin: 0;
			text-align: left;
			cursor: default;
			line-height: 1.25;
			-webkit-appearance: none;
			appearance: none;
			resize: none;
			outline: none;
			box-shadow: none;
			overflow: visible;
			width: auto;
			min-width: 0;
			font-family: inherit;
			white-space: pre-wrap;
		}

		/* Meta info subtitle (created / updated by). Border-bottom
		   separates the page-1 document header block from the
		   properties card below. BUG-626. */
		.meta-info {
			font-size: 9pt;
			color: #555;
			margin: 0 0 12pt 0;
			padding: 0 0 10pt 0;
			border: none;
			border-bottom: 1px solid #ccc;
			display: block;
		}
		.meta-info .meta-sep { color: #888; }

		/* Code context section — compact. */
		.code-context-section {
			margin: 0 0 12pt 0;
			page-break-inside: avoid;
			break-inside: avoid;
		}
		.code-context-section .section-title {
			font-size: 10pt;
			font-weight: 600;
			text-transform: uppercase;
			letter-spacing: 0.05em;
			color: #333;
			margin: 0 0 4pt 0;
		}
		.code-context-card {
			border: 1px solid #ccc !important;
			background: #fafafa !important;
			padding: 6pt 8pt !important;
			border-radius: 3pt;
			font-size: 9.5pt;
		}

		/* Body: stack fields + content vertically instead of side-by-side. */
		.item-body,
		.item-body.layout-balanced,
		.item-body.layout-focus {
			display: block !important;
			grid-template-columns: none !important;
			gap: 0 !important;
		}

		/* Properties panel → definition-list style block under the title. */
		.fields-panel {
			border: 1px solid #ccc !important;
			background: #fafafa !important;
			padding: 8pt 10pt !important;
			margin: 0 0 14pt 0 !important;
			border-radius: 3pt;
			page-break-inside: avoid;
			break-inside: avoid;
			grid-template-columns: none !important;
			display: block !important;
		}
		.fields-header {
			font-size: 9pt !important;
			font-weight: 600 !important;
			text-transform: uppercase;
			letter-spacing: 0.05em;
			color: #333 !important;
			margin: 0 0 4pt 0 !important;
			padding: 0;
			border: none;
		}
		.fields-header + .fields-header,
		.fields-panel > .fields-header ~ .fields-header {
			margin-top: 8pt !important;
		}
		.field-row {
			display: grid;
			grid-template-columns: minmax(80pt, 120pt) 1fr;
			gap: 6pt;
			padding: 1.5pt 0;
			border: none !important;
			font-size: 10pt;
			align-items: baseline;
		}
		/* Non-computed fields whose raw value is blank (null / "" / []) —
		   skip the whole row in print so we don't print a label with no
		   value (e.g. an unset "Category"). Flag applied at the template
		   level because :empty can't see the FieldEditor child. BUG-626. */
		.field-row.print-empty {
			display: none !important;
		}
		.field-label {
			color: #555 !important;
			font-weight: 500;
			font-size: 9.5pt;
		}
		.field-value {
			color: #000 !important;
			font-size: 10pt;
		}

		/* Form-widget strip rules for .field-value are in app.css
		   (global) because most of those controls render inside the
		   FieldEditor child component and Svelte scoped selectors
		   from this file don't cross component boundaries. Keep the
		   assignment-select rule here since those selects are inline
		   in this template. */
		.assignment-select {
			appearance: none !important;
			-webkit-appearance: none !important;
			background: transparent !important;
			border: none !important;
			padding: 0 !important;
			margin: 0 !important;
			color: #000 !important;
			font: inherit !important;
			cursor: default !important;
			pointer-events: none;
			box-shadow: none !important;
		}

		/* Progress bar → compact text percentage. */
		.progress-bar {
			background: #eee !important;
			border: 1px solid #ccc !important;
			height: 10pt;
			overflow: hidden;
			position: relative;
		}
		.progress-fill {
			background: #888 !important;
		}
		.progress-text {
			color: #000 !important;
			font-size: 8.5pt;
		}

		/* Content panel: full width, no padding from screen. */
		.content-panel {
			padding: 0 !important;
			margin: 0;
		}

		/* Relationships: keep but compact. */
		.relationships-section {
			margin: 14pt 0 0 0 !important;
			padding: 8pt 0 0 0 !important;
			border-top: 1px solid #ccc !important;
			page-break-inside: avoid;
			break-inside: avoid;
		}
		.relationships-section .section-title {
			font-size: 10pt;
			font-weight: 600;
			text-transform: uppercase;
			letter-spacing: 0.05em;
			color: #333;
			margin: 0 0 4pt 0;
		}
		.relationship-groups {
			display: block !important;
		}
		.relationship-group {
			margin: 0 0 6pt 0 !important;
		}
		.relationship-group-title {
			font-size: 9.5pt;
			font-weight: 600;
			color: #333;
			margin: 0 0 2pt 0;
		}
		.links-list {
			margin: 0;
			padding: 0;
		}
		.link-row {
			display: block !important;
			padding: 1pt 0 !important;
			border: none !important;
			background: transparent !important;
			font-size: 9.5pt;
		}
		.link-target {
			color: #000 !important;
			text-decoration: none;
		}

		/* -------------------------------------------------------------
		   TASK-623 / BUG-626 — rendered footer on every printed page.
		   The repeating print-header was removed as part of BUG-626;
		   only the fixed footer (date + URL) remains. Page number
		   lives in `@page @bottom-right` (see app.css). @page margins
		   are owned globally in app.css.
		   ------------------------------------------------------------- */
		.print-footer {
			display: flex;
			position: fixed;
			left: 0;
			right: 0;
			bottom: 0;
			box-sizing: border-box;
			padding: 10pt 1.2in 10pt 0.6in;
			background: #fff;
			color: #555;
			font-size: 9pt;
			line-height: 1.35;
			gap: 6pt;
			align-items: center;
			z-index: 1000;
			border-top: 1px solid #ccc;
			justify-content: space-between;
		}
		.print-footer-date {
			white-space: nowrap;
			color: #555;
		}
		.print-footer-url {
			flex: 1;
			text-align: center;
			padding: 0 8pt;
			color: #666;
			font-size: 8pt;
			word-break: break-all;
			/* Clamp to a single line so long URLs don't push the footer
			   height, which would otherwise overlap the body content. */
			white-space: nowrap;
			overflow: hidden;
			text-overflow: ellipsis;
		}
	}
</style>
