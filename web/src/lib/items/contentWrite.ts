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
import type { PendingEditsReason } from '$lib/stores/pendingEditsDialog.svelte';

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

/**
 * The sentence a write owes its user when it DELETED another tab's unsaved
 * edits (BUG-3230 U2): `warnings.pruned_pending_edits` counts the op-log rows a
 * direct content write removed. The web sends such a write only after the user
 * chose to overwrite, so this confirms what their choice did rather than
 * reporting a surprise. Rows are editor update frames, not keystrokes, so the
 * count is given as "changes". Null when nothing was deleted.
 */
export function prunedEditsNotice(resp: Pick<Item, 'warnings'> | null | undefined): string | null {
	const n = resp?.warnings?.pruned_pending_edits ?? 0;
	if (!(n > 0)) return null;
	return `${n} unsaved ${n === 1 ? 'change' : 'changes'} from another tab ${n === 1 ? 'was' : 'were'} discarded.`;
}

/**
 * The sentence a write owes its user when its body went to a tab's LIVE
 * document instead of the stored row (BUG-3230 U3):
 * `warnings.content_outcome: applied_pending_flush` (BUG-2995). The row, and so
 * this page's next read, keeps the OLD body until that tab saves, which usually
 * takes seconds but is not guaranteed (BUG-3000), so the sentence promises no
 * time and no outcome. A save outside the item pane
 * reports it, because nothing on that page shows the live document. Null
 * otherwise.
 */
export function contentOutcomeNotice(resp: Pick<Item, 'warnings'> | null | undefined): string | null {
	if (resp?.warnings?.content_outcome !== 'applied_pending_flush') return null;
	return 'The item is open in another tab, so the new body went to that tab\'s editor. Until that tab saves it, this page may still show the old body.';
}

/** The server refused a token-guarded content write because a tab holds unflushed edits. */
export function isContentPendingFlush(err: unknown): boolean {
	return err instanceof PadApiError && err.code === 'content_pending_flush';
}

/**
 * An open tab refused to apply the content because it holds typing the server
 * has not stored yet (BUG-3542): 409 content_not_applied, apply_reason
 * unconfirmed_edits. Other fields in the write may have landed; the content
 * did not.
 */
export function isContentNotAppliedUnconfirmed(err: unknown): boolean {
	return (
		err instanceof PadApiError &&
		err.code === 'content_not_applied' &&
		err.details?.apply_reason === 'unconfirmed_edits'
	);
}

/**
 * Either refusal above: another tab holds edits the row does not, so the text
 * was not stored. Both have the same answer for the person saving: keep the
 * text, and replace those edits only if they choose to (overwrite_pending_edits
 * lifts both). TASK-3548.
 */
export function isEditsNotStoredRefusal(err: unknown): boolean {
	return isContentPendingFlush(err) || isContentNotAppliedUnconfirmed(err);
}

/**
 * An unconfirmed-edits refusal still committed its row write (it versions the
 * old body before the open tab refuses), so the row's seq moved under the
 * caller's token, and an overwrite resent with that token meets update_conflict
 * (TASK-3548). The caller re-reads the item after such a refusal (only then:
 * isContentNotAppliedUnconfirmed) and asks this: is the stored body still
 * `baseContent`, the body the token was taken against? If so nothing the caller
 * has not seen changed it, and the fresh row's seq is the token to resend with;
 * if not, the caller's stale path decides.
 */
export function stillOnBase(fresh: { content?: string | null } | null | undefined, baseContent: string): boolean {
	return !!fresh && (fresh.content ?? '') === baseContent;
}

/**
 * The other half of that question for a write that sent more than the body
 * (codex, TASK-3548): does the re-read row hold exactly the title and every
 * fields_patch value this write sent? The refused write landed them, so it
 * should; if not, someone else changed them since, and resending this write
 * with the fresh token would put the old values back over a change the user
 * has not seen. Fields compare as JSON.
 */
export function holdsWhatWasSent(
	fresh: { title?: string; fields?: string | Record<string, unknown> | null } | null | undefined,
	sent: { title?: string; fields_patch?: Record<string, unknown> },
): boolean {
	if (!fresh) return false;
	if (sent.title !== undefined && fresh.title !== sent.title) return false;
	const patch = sent.fields_patch;
	if (!patch || Object.keys(patch).length === 0) return true;
	let stored: Record<string, unknown>;
	try {
		stored = typeof fresh.fields === 'string' ? (JSON.parse(fresh.fields || '{}') as Record<string, unknown>) : (fresh.fields ?? {});
	} catch {
		return false;
	}
	return Object.entries(patch).every(([k, v]) => JSON.stringify(stored[k]) === JSON.stringify(v));
}

/**
 * Why a content_pending_flush refusal happened (BUG-3244): 'set_aside' when the
 * server counted edits an editor upgrade set aside (details.set_aside_rows),
 * which no tab will ever store, else 'pending'. Copy that tells the user what to
 * do branches on it.
 */
export function pendingEditsReason(err: unknown): PendingEditsReason {
	if (err instanceof PadApiError) {
		const n = err.details?.set_aside_rows;
		if (typeof n === 'number' && n > 0) return 'set_aside';
	}
	return 'pending';
}
