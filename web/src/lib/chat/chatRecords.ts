import { authStore } from '$lib/stores/auth.svelte';

export interface ChatDraft {
	userId: string;
	n: number;
	text: string;
	replySeq: number | null;
}
export interface PendingRecord {
	userId: string;
	n: number;
	op_key: string;
	body: string;
	reply_seq: number | null;
}

const DRAFT = 'pad:chat-draft:';
const PENDING = 'pad:chat-pending:';
const draftKey = (user: string, n: number) => `${DRAFT}${user}:${n}`;
const pendingKey = (user: string, n: number, op: string) => `${PENDING}${user}:${n}:${op}`;
const isChatKey = (key: string) => key.startsWith(DRAFT) || key.startsWith(PENDING);
// Failed writes override storage; null remembers a removal if storage is blocked.
const memory = new Map<string, string | null>();
let sealed = false;
const allowed = (user: string) => !sealed && !!user && user === authStore.userId;

function read(key: string): string | null {
	if (memory.has(key)) return memory.get(key) ?? null;
	try { return sessionStorage.getItem(key); } catch { return null; }
}
function write(key: string, value: string | null): boolean {
	try {
		if (value === null) sessionStorage.removeItem(key);
		else sessionStorage.setItem(key, value);
		memory.delete(key);
		return true;
	} catch {
		memory.set(key, value);
		return false;
	}
}
function keys(): string[] {
	const found = new Set(memory.keys());
	try {
		for (let i = 0; i < sessionStorage.length; i++) {
			const key = sessionStorage.key(i);
			if (key && isChatKey(key)) found.add(key);
		}
	} catch { /* Memory still works when storage is unavailable. */ }
	return [...found];
}
function parse(key: string): Record<string, unknown> | null {
	try {
		const value: unknown = JSON.parse(read(key) ?? 'null');
		return value && typeof value === 'object' ? value as Record<string, unknown> : null;
	} catch { return null; }
}
const reply = (value: unknown) => value === null || (typeof value === 'number' && Number.isSafeInteger(value) && value > 0);

export function getDraft(userId: string, n: number): ChatDraft | null {
	if (!allowed(userId)) return null;
	const value = parse(draftKey(userId, n));
	if (!value || value.userId !== userId || value.n !== n || typeof value.text !== 'string' || !reply(value.replySeq)) return null;
	return value as unknown as ChatDraft;
}

/** True when sessionStorage accepted the draft; false when kept only in memory or refused. */
export function setDraft(userId: string, n: number, text: string, replySeq: number | null = null): boolean {
	if (!allowed(userId)) return false;
	return write(draftKey(userId, n), text === '' ? null : JSON.stringify({ userId, n, text, replySeq }));
}
export function clearDraft(userId: string, n: number): void {
	if (allowed(userId)) write(draftKey(userId, n), null);
}
export function savePending(userId: string, n: number, op_key: string, body: string, reply_seq: number | null = null): void {
	if (allowed(userId)) write(pendingKey(userId, n, op_key), JSON.stringify({ userId, n, op_key, body, reply_seq }));
}
export function removePending(userId: string, n: number, opKey: string): void {
	if (allowed(userId)) write(pendingKey(userId, n, opKey), null);
}
export function listPending(userId: string, n: number): PendingRecord[] {
	if (!allowed(userId)) return [];
	const result: PendingRecord[] = [];
	for (const key of keys()) {
		if (!key.startsWith(PENDING)) continue;
		const value = parse(key);
		if (value?.userId === userId && value.n === n && typeof value.op_key === 'string' &&
			typeof value.body === 'string' && reply(value.reply_seq)) result.push(value as unknown as PendingRecord);
	}
	return result;
}

/** Seal before clearing: teardown and late results cannot put private text back. */
export function purgeAll(): void {
	sealed = true;
	for (const key of keys()) write(key, null);
}
export function purgeNotUser(userId: string): void {
	for (const key of keys()) {
		if (parse(key)?.userId !== userId) write(key, null);
	}
}
/** Only identity establishment and a failed explicit logout may release the seal. */
export function unsealChatRecords(): void {
	sealed = false;
}

authStore.onIdentityChange(() => {
	if (!authStore.userId) purgeAll();
	else {
		unsealChatRecords();
		purgeNotUser(authStore.userId);
	}
});
