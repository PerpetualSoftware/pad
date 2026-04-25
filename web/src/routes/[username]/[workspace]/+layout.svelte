<script lang="ts">
	import { page } from '$app/state';
	import { onMount, onDestroy, untrack } from 'svelte';
	import { workspaceStore } from '$lib/stores/workspace.svelte';
	import { collectionStore } from '$lib/stores/collections.svelte';
	import { editorStore } from '$lib/stores/editor.svelte';
	import { sseService } from '$lib/services/sse.svelte';
	import { syncService } from '$lib/services/sync.svelte';
	import { api } from '$lib/api/client';
	import { toastStore } from '$lib/stores/toast.svelte';
	import { starredStore } from '$lib/stores/starred.svelte';
	import { titleStore } from '$lib/stores/title.svelte';

	let { children } = $props();

	let wsSlug = $derived(page.params.workspace ?? '');
	let username = $derived(page.params.username ?? '');
	let unsubscribeSSE: (() => void) | null = null;
	let unsubscribeSync: (() => void) | null = null;

	onMount(() => {
		// Initialize the sync coordinator (sets up visibilitychange listener once)
		syncService.init();

		// Listen for sync results to refresh collection metadata
		unsubscribeSync = syncService.onSync((result) => {
			if (!wsSlug) return;
			if (result.type === 'full_refresh' || (result.type === 'incremental' && result.changes.collections_changed)) {
				collectionStore.loadCollections(wsSlug);
			}
		});

		connectSSE();
	});

	onDestroy(() => {
		unsubscribeSSE?.();
		unsubscribeSync?.();
		sseService.disconnect();
		titleStore.clearPageTitle();
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
			});
		}
	});

	function connectSSE() {
		unsubscribeSSE?.();
		if (!wsSlug) return;

		sseService.connect(wsSlug);

		unsubscribeSSE = sseService.onItemEvent(async (event) => {
			const activeItem = collectionStore.activeItem;
			const isExternal = event.source !== 'web';

			switch (event.type) {
				case 'item_created': {
					// Reload collections to update counts
					collectionStore.loadCollections(wsSlug);
					try {
						const item = await api.items.get(wsSlug, event.item_id);
						collectionStore.addItem(item);
					} catch {
						// Item might not be fetchable by event ID, refresh collection
					}
					if (isExternal) {
						const who = event.actor === 'agent' ? 'Agent' : (event.actor_name || 'CLI');
						const link = event.collection ? `/${username}/${wsSlug}/${event.collection}/${event.item_id}` : undefined;
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
					collectionStore.loadCollections(wsSlug);

					if (activeItem && activeItem.id === event.item_id) {
						if (editorStore.dirty) {
							editorStore.setExternalChange(true);
						} else {
							try {
								const updated = await api.items.get(wsSlug, activeItem.slug);
								collectionStore.setActiveItem(updated);
								collectionStore.updateItemInList(updated);
							} catch {}
						}
					} else {
						// Update the item in the store's items list even if it's not the active item
						const existing = collectionStore.items.find(i => i.id === event.item_id);
						if (existing) {
							try {
								const updated = await api.items.get(wsSlug, existing.slug);
								collectionStore.updateItemInList(updated);
							} catch {}
						}
					}
					break;
				}

				case 'item_archived': {
					collectionStore.loadCollections(wsSlug);
					collectionStore.removeItem(event.item_id);
					break;
				}

				case 'item_restored': {
					collectionStore.loadCollections(wsSlug);
					break;
				}
			}
		});
	}
</script>

{@render children()}
