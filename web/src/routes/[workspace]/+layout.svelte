<script lang="ts">
	import { page } from '$app/state';
	import { onMount, onDestroy } from 'svelte';
	import { workspaceStore } from '$lib/stores/workspace.svelte';
	import { collectionStore } from '$lib/stores/collections.svelte';
	import { editorStore } from '$lib/stores/editor.svelte';
	import { sseService } from '$lib/services/sse.svelte';
	import { api } from '$lib/api/client';
	import { toastStore } from '$lib/stores/toast.svelte';

	let { children } = $props();

	let wsSlug = $derived(page.params.workspace ?? '');
	let unsubscribeSSE: (() => void) | null = null;

	onMount(() => {
		connectSSE();
	});

	onDestroy(() => {
		unsubscribeSSE?.();
		sseService.disconnect();
	});

	// Initialize workspace, load collections, and reconnect SSE when workspace changes
	$effect(() => {
		if (wsSlug) {
			workspaceStore.setCurrent(wsSlug);
			collectionStore.loadCollections(wsSlug);
			connectSSE();
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
						const who = event.actor === 'agent' ? 'Agent' : 'CLI';
						toastStore.show(`${who} created: ${event.title}`, 'info');
					}
					break;
				}

				case 'item_updated': {
					collectionStore.loadCollections(wsSlug);

					if (activeItem && activeItem.id === event.item_id) {
						const timeSinceLastSave = Date.now() - editorStore.lastSaveTime;
						if (timeSinceLastSave < 2000) break;

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
