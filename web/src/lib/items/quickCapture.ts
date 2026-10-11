// Quick capture confirms in place (TASK-3590). Adding an item used to open it
// at once, so jotting down an interruption was a context switch. Now "Add"
// stays where the user is, and this toast says what was added, with an Open
// action (and the link kept in the notification tray) for when they do want
// to go there.
import { goto } from '$app/navigation';
import { toastStore } from '$lib/stores/toast.svelte';

interface Captured {
	title: string;
	collection_prefix?: string | null;
	item_number?: number | null;
}

/** "TASK-12", or the title when the item has no ref. */
export function capturedLabel(item: Captured): string {
	return item.collection_prefix && item.item_number ? `${item.collection_prefix}-${item.item_number}` : item.title;
}

/** The confirmation toast for an item added without opening it. */
export function announceCaptured(item: Captured, url: string): void {
	toastStore.show(`Added ${capturedLabel(item)}`, 'success', undefined, url, {
		label: 'Open',
		onAction: () => goto(url),
	});
}
