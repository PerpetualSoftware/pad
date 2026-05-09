<script lang="ts">
	import { page } from '$app/state';
	import { tick, onMount, onDestroy } from 'svelte';
	import { api } from '$lib/api/client';
	import { collectionStore } from '$lib/stores/collections.svelte';
	import { syncService } from '$lib/services/sync.svelte';
	import { sseService } from '$lib/services/sse.svelte';
	import Editor from '$lib/components/editor/Editor.svelte';
	import EditorBubbleMenu from '$lib/components/editor/EditorBubbleMenu.svelte';
	import EditorLinkPopover from '$lib/components/editor/EditorLinkPopover.svelte';
	import RawMarkdownEditor from '$lib/components/editor/RawMarkdownEditor.svelte';
	import type { Editor as EditorType } from '@tiptap/core';
	import * as Y from 'yjs';
	import { CollabProvider } from '$lib/collab/wsProvider.svelte';
	import FieldEditor from '$lib/components/fields/FieldEditor.svelte';
	import ItemTimeline from '$lib/components/timeline/ItemTimeline.svelte';
	import ChildItems from '$lib/components/ChildItems.svelte';
	import { goto } from '$app/navigation';
	import { relativeTime, wikiLinksToMarkdown, markdownToWikiLinks, cleanBrokenLinks, unescapeDocLinks } from '$lib/utils/markdown';
	import { toastStore } from '$lib/stores/toast.svelte';
	import { editorStore } from '$lib/stores/editor.svelte';
	import type { Item, Collection, CollectionSettings, QuickAction, ItemLink, AgentRole } from '$lib/types';
	import { parseFields, parseSchema, parseSettings, formatItemRef, getTerminalOptions } from '$lib/types';
	import QuickActionsMenu from '$lib/components/common/QuickActionsMenu.svelte';
	import BottomSheet from '$lib/components/common/BottomSheet.svelte';
	import EditCollectionModal from '$lib/components/collections/EditCollectionModal.svelte';
	import ShareDialog from '$lib/components/ShareDialog.svelte';
	import { copyToClipboard } from '$lib/utils/clipboard';
	import { starredStore } from '$lib/stores/starred.svelte';
	import { titleStore } from '$lib/stores/title.svelte';
	import { workspaceStore } from '$lib/stores/workspace.svelte';

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

	let editorInstance = $state<EditorType | null>(null);

	let editingTitle = $state(false);
	let titleDraft = $state('');
	let titleInputEl = $state<HTMLTextAreaElement>();

	let fields = $derived<Record<string, any>>(item ? parseFields(item) : {});
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
						item = { ...updated, content: item.content };
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
						item = { ...updated, content: item.content };
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
					// Merge server state without disrupting the editor
					if (!item || item.id !== reqItemId) return;
					item = {
						...updated,
						content: item.content
					};
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
				item = {
					...updated,
					content: item.content
				};
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
			const [itemData, collData] = await Promise.all([
				api.items.get(wsSlug, itemSlug),
				api.collections.get(wsSlug, collSlug)
			]);
			item = itemData;
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

			// Also load items for wiki-link resolution if not already loaded
			if ((collectionStore.items ?? []).length === 0) {
				collectionStore.loadItems(wsSlug);
			}

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
	const collabKey = $derived(item && canEdit && !rawMode ? item.id : null);

	$effect(() => {
		if (!collabKey) return;
		const itemId = collabKey;
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
					return true;
				} catch (err) {
					console.warn('collab: setContent failed', err);
					return false;
				}
			},
		});

		ydoc = doc;
		collabProvider = provider;

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
			if (!rawMode) {
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
			item = await api.items.update(wsSlug, item.id, { title: titleDraft.trim() });
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
		saveStatus = 'saving';
		try {
			item = await api.items.update(wsSlug, item.id, { fields: JSON.stringify(updated) });
			showSaved();
		} catch {
			saveStatus = 'idle';
			toastStore.show('Failed to save', 'error');
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
			item = await api.items.update(wsSlug, item.id, update);
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
			item = await api.items.update(wsSlug, item.id, update);
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
	type CollabFlushResult = 'flushed' | 'deduped' | 'failed';

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
			await api.items.flushCollabContent(ws, itemId, toSave, { keepalive });
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
					item = updated;
					rawPendingMarkdown = null;
					editorStore.setDirty(false);
					showSaved();
				} else {
					// Newer pending edit; keep local content, adopt
					// server-side metadata only. The next debounce
					// cycle will land the queued edit.
					item = { ...updated, content: item.content };
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
						item = updated;
						rawPendingMarkdown = null;
					} else {
						// Newer edit pending — keep our local
						// content but adopt server-side metadata
						// (timestamps, version, modified_by).
						item = { ...updated, content: item.content };
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
		item = updatedItem;
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
			item = { ...refreshed, content: item.content };
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
			item = { ...refreshed, content: item.content };
			toastStore.show('Relationship added', 'success');
		} catch (e: any) {
			toastStore.show(e.message ?? 'Failed to add relationship', 'error');
		}
	}

	async function handleMove(targetSlug: string) {
		if (!item || moving) return;
		moving = true;
		showMoveMenu = false;
		try {
			const moved = await api.items.move(wsSlug, item.slug, targetSlug);
			toastStore.show(`Moved to ${targetSlug}`, 'success');
			goto(`/${username}/${wsSlug}/${targetSlug}/${moved.slug}`, { replaceState: true });
		} catch (e: any) {
			toastStore.show(e.message ?? 'Failed to move item', 'error');
		} finally {
			moving = false;
		}
	}
</script>

{#if loading}
	<div class="center-message">Loading...</div>
{:else if error}
	<div class="center-message">{error}</div>
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
		</div>

		<!-- Meta info -->
		<div class="meta-info">
			<span title={new Date(item.created_at).toLocaleString()}>Created {relativeTime(item.created_at)} by {item.created_by || 'unknown'}</span>
			<span class="meta-sep">·</span>
			<span title={new Date(item.updated_at).toLocaleString()}>Updated {relativeTime(item.updated_at)}</span>
			<span class="save-status" class:saving={saveStatus === 'saving'} class:saved={saveStatus === 'saved'} class:visible={saveStatus !== 'idle'}>
				{#if saveStatus === 'saving'}Saving...{:else}✓ Saved{/if}
			</span>
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
										if (result === 'failed') {
											// PATCH errored — refuse
											// to enter raw mode
											// rather than silently
											// seed from stale state.
											// runCollabFlush already
											// surfaced the toast.
											// Per Codex review round 8.
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
								onEditor={(e) => editorInstance = e}
							/>
						{/key}
					{:else if ydoc}
						{#key `${item.id}:true`}
							<Editor
								content={editorContent}
								onUpdate={handleContentUpdate}
								editable={true}
								ydoc={ydoc}
								onEditor={(e) => editorInstance = e}
							/>
						{/key}
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

		<!-- Unified Timeline (comments + activity + versions) -->
		<div id="item-timeline" class="timeline-section">
			<ItemTimeline
				{wsSlug}
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
