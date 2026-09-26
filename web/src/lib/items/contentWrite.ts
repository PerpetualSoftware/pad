// How a browser editor sends an item's body (BUG-3050 U1).
//
// Four doors loaded `item.content`, let the user edit fields or the body, and
// sent the body back on every save WITHOUT a version token: the playbook
// editor, the conventions inline edit, the playbook duplicate, and ItemDetail's
// raw-editor fallback seed. A tab with unflushed edits holds content that is
// NOT in `item.content` (the op-log is ahead of the row: `content_state:
// applied_pending_flush`), and per BUG-3133 a tokenless content write REPLACES
// those edits, and with no tab open PRUNES them from the op-log. That is the
// "re-read later, never re-send" rule broken, at the write.
//
// The rule these doors now follow: send the body only when the user CHANGED
// it, and then always with the row's token, so the server refuses with
// `409 content_pending_flush` instead of replacing edits it cannot see. The
// refusal becomes a plain choice for the user (pendingEditsDialog), never an
// automatic overwrite.
import { PadApiError } from '$lib/api/client';
import type { Item } from '$lib/types';
import { occTokenFor } from '$lib/items/occToken';

/**
 * The body half of an update: nothing when the body is unchanged since load,
 * else the body with the token of the row it was loaded from.
 */
export function contentWriteFor(
	body: string,
	loaded: Pick<Item, 'content' | 'seq' | 'updated_at'>,
): { content: string; expected_seq?: number; expected_updated_at?: string } | Record<string, never> {
	if (body === (loaded.content ?? '')) return {};
	const token = occTokenFor(loaded);
	return token.kind === 'seq'
		? { content: body, expected_seq: token.value }
		: { content: body, expected_updated_at: token.value };
}

/** The server refused a token-guarded content write because a tab holds unflushed edits. */
export function isContentPendingFlush(err: unknown): boolean {
	return err instanceof PadApiError && err.code === 'content_pending_flush';
}
