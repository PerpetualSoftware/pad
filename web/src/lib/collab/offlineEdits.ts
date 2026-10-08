/**
 * TASK-2199: what the item editor does about edits that have not reached the
 * server. The collaborative editor keeps them in this tab only (no browser
 * storage): a reconnect sends them, closing the tab loses them, and a
 * force_refresh on reconnect (the server pruned or rebuilt the op-log while
 * the tab was away) rebuilds the editor from the server's text, so they are
 * handed back to copy instead of being written over it.
 */

/** A tab's version of an item that a reload discarded, held for copying. */
export interface OfflineRecovery {
	itemId: string;
	text: string;
}

/**
 * What a force_refresh discards that the user still needs. Null when there is
 * nothing to hand back: no unsent local edits, no editor text to read, or the
 * text equals what the server already has.
 */
export function offlineRecoveryOnForceRefresh(input: {
	unsentLocalEdits: boolean;
	liveMarkdown: string | null;
	storedContent: string;
	itemId: string;
}): OfflineRecovery | null {
	if (!input.unsentLocalEdits || input.liveMarkdown === null) return null;
	if (input.liveMarkdown === input.storedContent) return null;
	return { itemId: input.itemId, text: input.liveMarkdown };
}

/** Whether closing or reloading the tab must raise the browser's prompt. */
export function unloadLosesEdits(input: {
	rawDirty: boolean;
	unsentLocalEdits: boolean;
	recovery: OfflineRecovery | null;
}): boolean {
	return input.rawDirty || input.unsentLocalEdits || input.recovery !== null;
}

/**
 * The question to ask before an in-app navigation away from the item, or null
 * when leaving loses nothing. An uncopied recovered version asks first, since
 * it is the one copy left.
 */
export function leaveQuestion(input: {
	unsentLocalEdits: boolean;
	recovery: OfflineRecovery | null;
}): string | null {
	if (input.recovery) return "Your offline version of this item hasn't been copied. Leave anyway?";
	if (input.unsentLocalEdits) {
		return "Some of your edits haven't reached the server yet, and leaving loses them. Leave anyway?";
	}
	return null;
}
