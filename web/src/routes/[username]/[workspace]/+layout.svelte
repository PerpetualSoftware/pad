<script lang="ts">
	import { page } from '$app/state';
	import { onMount, onDestroy, untrack } from 'svelte';
	import { workspaceStore } from '$lib/stores/workspace.svelte';
	import { collectionStore } from '$lib/stores/collections.svelte';
	import { editorStore } from '$lib/stores/editor.svelte';
	import { sseService } from '$lib/services/sse.svelte';
	import { syncService } from '$lib/services/sync.svelte';
	import { api } from '$lib/api/client';
	import { toastStore, quietExternalToasts } from '$lib/stores/toast.svelte';
	import { starredStore } from '$lib/stores/starred.svelte';
	import { localIndex } from '$lib/stores/localIndex.svelte';
	import { titleStore } from '$lib/stores/title.svelte';
	import { authStore } from '$lib/stores/auth.svelte';
	import { registerWorkspaceTools, type WebMcpHandle } from '$lib/webmcp/register';
	import { paneOverlay } from '$lib/stores/paneOverlay.svelte';
	import ConnectBanner from '$lib/components/ConnectBanner.svelte';
	import VerifyEmailBanner from '$lib/components/VerifyEmailBanner.svelte';
	import BottomNav from '$lib/components/layout/BottomNav.svelte';
	import MobileContextBar from '$lib/components/layout/MobileContextBar.svelte';

	let { children } = $props();

	let wsSlug = $derived(page.params.workspace ?? '');
	let username = $derived(page.params.username ?? '');
	let unsubscribeSSE: (() => void) | null = null;
	let unsubscribeSync: (() => void) | null = null;
	let webmcpTeardown: WebMcpHandle | null = null;
	let webmcpToken = 0;

	onMount(() => {
		// Initialize the sync coordinator (sets up visibilitychange listener once)
		syncService.init();

		// Listen for sync results to refresh collection metadata, and to DRIVE
		// THE WORKSPACE RECONCILE (TASK-2921 / IDEA-2901).
		//
		// The reconcile used to be driven from the collection route, so a
		// `sync_required` arriving while the user sat on item detail, the graph,
		// the copy dialog or any other workspace route reached nothing — the
		// index was still live and still being READ (ItemPicker reads it from
		// the copy dialog, which is not a collection route) with nothing
		// reconciling it. This layout is mounted for every route under
		// `[username]/[workspace]` and it is where the signal source itself
		// lives — `syncService.init()` and `connectSSE()` below, `disconnect()`
		// in onDestroy — so it is the owner whose lifetime actually matches the
		// signal's. A store-owned subscription would have outlived its source.
		unsubscribeSync = syncService.onSync(async (result) => {
			// CAPTURE the workspace this result belongs to. `wsSlug` is derived
			// from the route and changes under an async callback: switching from
			// A to B while A's sync is in flight would otherwise reconcile B on
			// A's result and mark A synced (codex round 5 P1). Everything below
			// uses the captured value, and the re-read after the await is a
			// guard, not a use.
			const ws = wsSlug;
			if (!ws) return;
			if (result.type === 'full_refresh' || (result.type === 'incremental' && result.changes.collections_changed)) {
				collectionStore.loadCollections(ws);
			}
			// Always reconcile, even for `caught_up` — SSE delivers events, not
			// delta data, and a previous failure won't recover without a fresh
			// attempt. The localIndex cursor is independent of
			// syncService.lastSyncTime and per-row seq guards make repeated
			// calls idempotent.
			let caughtUp = false;
			try {
				caughtUp = await localIndex.reconcile(ws);
			} catch {
				// 401/403 purge and the error banner are handled where the cache
				// state is read — `bootstrapState` and `pendingResyncFor`. There
				// is nothing route-specific to do here, and throwing out of a
				// subscriber would take the other subscribers with it.
			}
			// `markSynced` advances syncService's OWN cursor and is workspace-
			// level, so it moves here with the outcome it depends on. Only on a
			// clean catch-up: a failed reconcile leaves the cursor where it is
			// so the next tab-resume retries.
			// `markSynced` advances a SHARED, workspace-agnostic cursor, so it
			// must not be advanced on the strength of a result for a workspace
			// the user has since left — the reconcile that vouched for it was
			// about a different cache.
			if (caughtUp && result.type === 'full_refresh' && wsSlug === ws) {
				syncService.markSynced();
			}
		});

		connectSSE();
	});

	onDestroy(() => {
		unsubscribeSSE?.();
		unsubscribeSync?.();
		sseService.disconnect();
		titleStore.clearPageTitle();
		webmcpToken++;        // invalidate any in-flight registration
		webmcpTeardown?.();
		webmcpTeardown = null;
	});

	// These two effects are split on purpose:
	//
	// 1. Workspace-name sync: runs whenever `workspaceStore.current` resolves
	//    or changes. It only touches `workspace` — if it also cleared
	//    section/item, an async workspace-name arrival AFTER a leaf page had
	//    set its section/item would wipe that context.
	//
	// 2. Route-change clear: depends only on `page.url.pathname`, so it runs
	//    exactly once per SPA navigation and clears section/item. Leaf pages
	//    wired to the title store (workspace home, collection list, item
	//    detail, activity) re-set their parts in child `$effect`s that run
	//    after this one — Svelte 5 guarantees parent effects run before
	//    child effects. Unwired routes (settings, roles, etc.) inherit the
	//    cleared state and fall back to `{Workspace} · Pad`.
	$effect(() => {
		titleStore.setPageTitle({ workspace: workspaceStore.current?.name ?? null });
	});
	$effect(() => {
		page.url.pathname;
		titleStore.setPageTitle({ section: null, item: null });
	});

	// Persist the user's last-visited route per workspace so the workspace
	// switcher (WorkspaceSwitcher.svelte) can restore it on switch instead
	// of always landing on the dashboard. Implements IDEA-753 / TASK-754.
	//
	// We persist `pathname + search` so URL-carried state (collection
	// view mode, sort, group-by, filters, search query — see e.g.
	// `/[collection]/+page.svelte` which mutates `?view=...`) is part of
	// the restored location.
	//
	// Per CONVE-606, this is its own effect with a clean dependency list
	// (wsSlug + pathname + search). Combining with the title sync above
	// would re-run it on async workspace-name resolution and could
	// overwrite the saved route at unexpected times. Storage failures
	// (private-mode quota, disabled storage) are swallowed — restoration
	// just won't kick in.
	$effect(() => {
		if (!wsSlug) return;
		try {
			localStorage.setItem(
				`pad-last-route-${wsSlug}`,
				page.url.pathname + page.url.search
			);
		} catch {
			// localStorage unavailable; silent no-op.
		}
	});

	// Initialize workspace, load collections, and reconnect SSE when the
	// workspace slug changes. The body is wrapped in `untrack` because
	// `workspaceStore.setCurrent(wsSlug)` synchronously reads
	// `workspaces.find(...)`, which would otherwise establish a reactive
	// dependency on the entire workspaces array. With that dependency,
	// any reorder via the topbar (which calls `workspaceStore.loadAll()`
	// after persisting) re-runs this whole effect — re-initializing
	// the SSE callback and reassigning `workspaceStore.current` to a
	// fresh object — and the resulting reactivity cascade flickers the
	// current page. Wrapping in untrack keeps the only tracked dep the
	// `wsSlug` read in the if-check, matching the comment's intent.
	$effect(() => {
		if (wsSlug) {
			untrack(() => {
				workspaceStore.setCurrent(wsSlug);
				collectionStore.loadCollections(wsSlug);
				starredStore.load(wsSlug);
				syncService.setWorkspace(wsSlug);
				connectSSE();
				connectWebMCP();
			});
		}
	});

	// Re-attempt WebMCP registration when the auth gate resolves. The
	// workspace $effect above runs inside untrack() (so a workspace switch
	// doesn't drag in the whole workspaces array as a dep), which means it
	// does NOT react to auth loading. On a cold load where auth resolves
	// after the workspace effect has already run, this effect re-runs
	// connectWebMCP() once `webmcp_enabled` + `user` are present. Reading
	// both here establishes the reactive deps; connectWebMCP() is
	// idempotent (tears down any prior registration) and re-gated inside
	// registerWorkspaceTools(), so re-running it is safe.
	$effect(() => {
		// Establish reactive deps on the auth gate.
		const enabled = authStore.session?.webmcp_enabled;
		const user = authStore.user;
		if (!enabled || !user) return;
		connectWebMCP();
	});

	function connectSSE() {
		unsubscribeSSE?.();
		if (!wsSlug) return;

		sseService.connect(wsSlug);

		unsubscribeSSE = sseService.onItemEvent(async (event) => {
			const activeItem = collectionStore.activeItem;
			const isExternal = event.source !== 'web';

			// Pull the row data for this event into the local index (TASK-2921).
			// The SSE payload carries metadata only, so the reconcile is the only
			// thing that fetches rows; `classifySSEEvent` short-circuits the
			// duplicates the server's replay buffer re-delivers after a
			// tab-resume. Deliberately NOT filtered by collection — an item moved
			// into or out of any collection still needs its delta applied. This
			// ran on the collection route until this unit, which is why an item
			// created elsewhere never reached the index of a user sitting on a
			// non-collection route.
			// Captured for the same reason as the sync handler above: this
			// callback awaits, and `wsSlug` is derived from the route.
			//
			// Used for the WHOLE callback, not just the reconcile (codex round 6
			// P1). The later `loadCollections` / `api.items.get` / link-building
			// calls read the reactive slug after their own awaits and predate
			// this unit — but the reconcile added ANOTHER await in front of all
			// of them, so an event for workspace A crossing a switch to B now has
			// a wider window to load B's collections off A's event. Every use in
			// here means "the workspace this event arrived for", which is what
			// the subscription was opened on.
			const eventWs = wsSlug;
			if (eventWs && localIndex.classifySSEEvent(eventWs, event) !== 'stale') {
				try {
					await localIndex.reconcile(eventWs);
				} catch {
					// As above: the cache-state readers own the reaction.
				}
			}

			// THE INDEX IS PER-WORKSPACE; THE UI BELOW IS NOT (codex round 7 P1).
			// `localIndex` is keyed by workspace, so reconciling `eventWs` above
			// is right whatever route the user is on now. `collectionStore` and
			// the toasts are GLOBAL — they describe the ONE workspace being
			// looked at — so applying A's event to them while B is on screen
			// corrupts B's UI with A's data, and capturing the slug does not help
			// because the slug was never the problem for these.
			//
			// So: reconcile first, unconditionally, then bail if the route moved.
			// The two halves want opposite things and the await between them is
			// what makes the distinction visible at all.
			if (wsSlug !== eventWs) return;

			switch (event.type) {
				case 'item_created': {
					// Reload collections to update counts
					collectionStore.loadCollections(eventWs);
					try {
						const item = await api.items.get(eventWs, event.item_id);
						collectionStore.addItem(item);
					} catch {
						// Item might not be fetchable by event ID, refresh collection
					}
					// `quietExternalToasts` is the BUG-2334 e2e kill switch for exactly
					// this toast — the one place another actor's SSE traffic becomes a
					// click-intercepting surface. See its doc in the toast store.
					if (isExternal && !quietExternalToasts()) {
						const who = event.actor === 'agent' ? 'Agent' : (event.actor_name || 'CLI');
						const link = event.collection ? `/${username}/${eventWs}/${event.collection}/${event.item_id}` : undefined;
						toastStore.show(`${who} created: ${event.title}`, 'info', 4000, link);
					}
					break;
				}

				case 'item_updated': {
					// Skip all side-effects for self-triggered content saves
					const isSelfSave = activeItem
						&& activeItem.id === event.item_id
						&& (editorStore.dirty || Date.now() - editorStore.lastSaveTime < 5000);

					if (isSelfSave) break;

					// Only reload collections for external/non-editor updates
					// (e.g. status changes, field edits from another tab)
					collectionStore.loadCollections(eventWs);

					if (activeItem && activeItem.id === event.item_id) {
						if (editorStore.dirty) {
							editorStore.setExternalChange(true);
						} else {
							try {
								const updated = await api.items.get(eventWs, activeItem.slug);
								// Fence the late continuation (PLAN-2179 / TASK-2181): on the
								// focus-follows-editing host `collectionStore.activeItem`
								// ping-pongs master↔pane on each click, so the active item may
								// have SWITCHED during this await. Don't clobber the new active
								// side's reclaim with the item this event was for; the list
								// refresh below is still valid for the fetched item regardless.
								if (collectionStore.activeItem?.id === activeItem.id) {
									collectionStore.setActiveItem(updated);
								}
								collectionStore.updateItemInList(updated);
							} catch {}
						}
					} else {
						// Update the item in the store's items list even if it's not the active item
						const existing = collectionStore.items.find(i => i.id === event.item_id);
						if (existing) {
							try {
								const updated = await api.items.get(eventWs, existing.slug);
								collectionStore.updateItemInList(updated);
							} catch {}
						}
					}
					break;
				}

				case 'item_archived': {
					collectionStore.loadCollections(eventWs);
					collectionStore.removeItem(event.item_id);
					break;
				}

				case 'item_restored': {
					collectionStore.loadCollections(eventWs);
					break;
				}

				case 'collection_updated': {
					// BUG-2601: a RENAME changes the collection's slug without
					// touching its items, so no /items-changes delta ever
					// re-stamps the cached rows' `collection_slug` — after any
					// rename, `getByCollection(newSlug)` returns [] and the
					// renamed collection renders an empty board (sidebar still
					// counts the items) until each item is individually
					// modified. Retag the rows by STABLE collection id here,
					// on the workspace-global subscriber, so the heal applies
					// regardless of which route is open when the event lands.
					// Route re-targeting stays with the per-page handlers
					// (collection page + ItemDetail); this case owns the DATA.
					if (event.new_slug && event.collection_id) {
						localIndex.retagCollection(
							eventWs,
							event.collection_id,
							event.new_slug,
							authStore.user?.id ?? null,
						);
					}
					// Refresh sidebar/pickers for EVERY collection_updated —
					// icon / name / sort-order changes matter to the nav
					// even without a rename (codex round 1 P2).
					collectionStore.loadCollections(eventWs);
					break;
				}
			}
		});
	}

	function connectWebMCP() {
		webmcpTeardown?.();
		webmcpTeardown = null;
		const token = ++webmcpToken;
		if (!wsSlug) return;

		registerWorkspaceTools(wsSlug)
			.then((handle) => {
				// A newer registration superseded this one while the async
				// tool-surface fetch was in flight — discard the stale tools.
				if (token !== webmcpToken) {
					handle();
					return;
				}
				webmcpTeardown = handle;
			})
			.catch(() => {});
	}
</script>

<!--
	App-shell chrome (PLAN-2105 / TASK-2131). While a mobile detail-pane overlay
	is up (`paneOverlay.mobileOverlayActive`, set by PaneHost) every app-shell
	sibling the layout renders sits BEHIND the full-screen modal, so all of them
	are marked `inert` — out of the focus order and the screen-reader tree.
	`aria-modal` on the pane isn't reliably honored on its own, so the background
	must physically leave the a11y tree; that has to cover the banners
	(VerifyEmailBanner's Resend, ConnectBanner's action) as well as the context
	bar / bottom nav, or their controls stay reachable behind the modal. Only
	`{@render children()}` — which contains the pane itself — is left interactive.
	`display: contents` wrappers carry the `inert` (which cascades to descendants)
	without adding a box, so the fixed chrome renders exactly as before. Off mobile
	/ pane closed the signal is false and the attribute is absent — no desktop change.
-->
<div style="display: contents" inert={paneOverlay.mobileOverlayActive}>
	<VerifyEmailBanner />

	<ConnectBanner
		{wsSlug}
		serverUrl={typeof window !== 'undefined' ? window.location.origin : ''}
		workspaceName={workspaceStore.current?.name ?? ''}
	/>

	<MobileContextBar />
</div>

{@render children()}

<div style="display: contents" inert={paneOverlay.mobileOverlayActive}>
	<BottomNav />
</div>
