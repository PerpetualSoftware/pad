// What the composer may send (TASK-4044 PR 3b, rulings 4cf4d4b9 and 2de240d9,
// 3b ruling d87482de). Every number and rule is the server's, read at
// internal/server/handlers_chat.go:243-267 (CONVE-4093).

import { trimSpace } from '$lib/chat/presence';

/** The body cap: `len(in.Body) > 64*1024`, UTF-8 BYTES, not UTF-16 units (N2). */
export const MAX_BODY_BYTES = 64 * 1024;
/**
 * The request cap: `decodeJSONWithLimit(r, &in, 128*1024)` reads through
 * MaxBytesReader on the RAW request bytes, so JSON escaping counts (N3): 65,536
 * quotes are within the body cap and about twice the request cap.
 */
export const MAX_REQUEST_BYTES = 128 * 1024;
/** A stand-in with a real op_key's length (a UUID), for checks made before one exists. */
export const PLACEHOLDER_OP_KEY = '00000000-0000-4000-8000-000000000000';

export type SendBlock = 'empty' | 'too_long' | 'too_large';

const encoder = new TextEncoder();

export function utf8Bytes(s: string): number {
	return encoder.encode(s).length;
}

/**
 * Why this draft cannot be sent, or null. `empty` uses Go's TrimSpace (N1: a
 * lone U+0085 is empty to the server, a lone U+FEFF is not).
 */
export function validateDraft(body: string, replyToSeq: number | undefined, opKey: string): SendBlock | null {
	if (trimSpace(body) === '') return 'empty';
	if (utf8Bytes(body) > MAX_BODY_BYTES) return 'too_long';
	const payload = JSON.stringify({ body, ...(replyToSeq !== undefined ? { reply_to_seq: replyToSeq } : {}), op_key: opKey });
	if (utf8Bytes(payload) > MAX_REQUEST_BYTES) return 'too_large';
	return null;
}

/** The server's 400 for a request over the cap (`Invalid JSON body (maximum 128 KiB)`). */
export function isRequestTooLargeError(err: unknown): boolean {
	const e = err as { code?: unknown; message?: unknown } | null;
	return !!e && e.code === 'validation_error' && typeof e.message === 'string' && e.message.startsWith('Invalid JSON body');
}

/** Our own copy for a refusal; never the server's sentence. */
export const NOTICE: Record<Exclude<SendBlock, 'empty'> | 'reply_deleted' | 'reply_missing', string> = {
	too_long: 'That message is longer than 65,536 bytes. Shorten it to send.',
	too_large:
		'That message is too large to send: quotes, backslashes and control characters take extra space when sent. Shorten it.',
	reply_deleted: 'The message you were replying to was deleted. Your text is kept; send it as a new message.',
	reply_missing: 'The message you were replying to is no longer available. Your text is kept; send it as a new message.'
};
