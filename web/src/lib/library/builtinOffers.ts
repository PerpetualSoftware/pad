import type { BuiltinListEntry } from '$lib/types';

/**
 * The Library page's view of the built-ins already in a workspace (TASK-3462
 * U3b). An entry is matched by its stable KEY, so renaming the item does not
 * make the library offer it again. An item seeded before origins were
 * recorded has no key on the server's listing until the legacy adoption pass
 * (U4) runs, so the title match it always used still counts.
 */
export function builtinActive(
	entries: BuiltinListEntry[],
	entry: { key?: string; title: string },
	activeTitles: Set<string>
): boolean {
	if (entry.key && entries.some((e) => e.key === entry.key)) return true;
	return activeTitles.has(entry.title);
}

export interface BuiltinOffer {
	/** The item the offer is about; the badge links to it. */
	entry: BuiltinListEntry;
	/** "Update available" (unedited) or "Library changed" (edited too). */
	label: string;
	/** A sentence for the tooltip. */
	title: string;
}

/**
 * The offer to show on a library card, or null. An unedited copy reads
 * "Update available"; an edited one "Library changed", because taking the
 * update replaces the edits. When several items share the key, an unedited
 * one is preferred: it is the safer one to open first.
 */
export function builtinOfferLabel(entries: BuiltinListEntry[], key: string | undefined): BuiltinOffer | null {
	if (!key) return null;
	const mine = entries.filter((e) => e.key === key);
	const available = mine.find((e) => e.state === 'update_available');
	if (available) {
		return {
			entry: available,
			label: 'Update available',
			title: "Pad's library has a newer version, and this workspace's copy is unedited. Open it to review and accept."
		};
	}
	const diverged = mine.find((e) => e.state === 'diverged');
	if (diverged) {
		return {
			entry: diverged,
			label: 'Library changed',
			title: "Pad's library has a newer version, and this workspace's copy was edited. Open it to compare before deciding."
		};
	}
	return null;
}
