/**
 * A fresh idempotency key for a write (a chat send, a push, a copy).
 *
 * `crypto.randomUUID` exists only in a secure context (https, or localhost). A
 * self-hosted Pad on a LAN address over plain http is not one, and a bare call
 * there throws "crypto.randomUUID is not a function" (BUG-4312: the chat composer
 * cleared the person's text and sent nothing). `getRandomValues` is available
 * everywhere, so the fallback builds the same v4 UUID shape from it.
 */
export function newOpKey(): string {
	if (typeof crypto !== 'undefined' && typeof crypto.randomUUID === 'function') {
		return crypto.randomUUID();
	}
	const b = new Uint8Array(16);
	crypto.getRandomValues(b);
	b[6] = (b[6] & 0x0f) | 0x40; // version 4
	b[8] = (b[8] & 0x3f) | 0x80; // variant 10
	const h = Array.from(b, (x) => x.toString(16).padStart(2, '0')).join('');
	return `${h.slice(0, 8)}-${h.slice(8, 12)}-${h.slice(12, 16)}-${h.slice(16, 20)}-${h.slice(20)}`;
}
