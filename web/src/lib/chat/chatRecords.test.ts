import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
const auth = vi.hoisted(() => ({ userId: 'A', listeners: [] as Array<(previous: string) => void> }));
vi.mock('$lib/stores/auth.svelte', () => ({ authStore: {
	get userId() { return auth.userId; },
	onIdentityChange(fn: (previous: string) => void) { auth.listeners.push(fn); return () => {}; }
} }));
function storage(): Storage {
	const data = new Map<string, string>();
	return {
		get length() { return data.size; }, key: (i) => [...data.keys()][i] ?? null,
		getItem: (key) => data.get(key) ?? null, setItem: (key, value) => { data.set(key, value); },
		removeItem: (key) => { data.delete(key); }, clear: () => data.clear()
	};
}
let records: typeof import('./chatRecords');
function identity(id: string, previous = auth.userId) {
	auth.userId = id;
	for (const fn of auth.listeners) fn(previous);
}
beforeEach(async () => {
	vi.resetModules(); auth.userId = 'A'; auth.listeners = [];
	vi.stubGlobal('sessionStorage', storage());
	records = await import('./chatRecords');
});
afterEach(() => vi.unstubAllGlobals());
function seed(userId: string, n: number) {
	sessionStorage.setItem(`pad:chat-draft:${userId}:${n}`, JSON.stringify({ userId, n, text: userId, replySeq: null }));
	sessionStorage.setItem(`pad:chat-pending:${userId}:${n}:op`, JSON.stringify({ userId, n, op_key: 'op', body: userId, reply_seq: 4 }));
}
describe('chat records', () => {
	it('isolates A to B to A conversation navigation and users; reads are non-destructive', () => {
		records.setDraft('A', 1, 'first', 4); records.setDraft('A', 2, 'second');
		records.savePending('A', 1, 'op', 'sending', 4);
		expect(records.getDraft('A', 2)?.text).toBe('second');
		expect(records.getDraft('A', 1)).toEqual({ userId: 'A', n: 1, text: 'first', replySeq: 4 });
		expect(records.getDraft('A', 1)?.text).toBe('first');
		expect(records.getDraft('B', 1)).toBeNull();
		expect(records.listPending('A', 2)).toEqual([]); expect(records.listPending('B', 1)).toEqual([]);
		expect(records.listPending('A', 1)).toEqual([{ userId: 'A', n: 1, op_key: 'op', body: 'sending', reply_seq: 4 }]);
	});
	it('ignores corrupt JSON and malformed records', () => {
		sessionStorage.setItem('pad:chat-draft:A:1', '{'); sessionStorage.setItem('pad:chat-pending:A:1:op', '{');
		sessionStorage.setItem('pad:chat-pending:A:1:bad', JSON.stringify({ userId: 'A', n: 1 }));
		expect(records.getDraft('A', 1)).toBeNull(); expect(records.listPending('A', 1)).toEqual([]);
	});
	it('uses memory when storage is blocked, including removal', () => {
		vi.stubGlobal('sessionStorage', { getItem() { throw Error('blocked'); }, setItem() { throw Error('blocked'); }, get length() { throw Error('blocked'); } });
		expect(records.setDraft('A', 1, 'memory')).toBe(false); records.savePending('A', 1, 'op', 'body');
		expect(records.getDraft('A', 1)?.text).toBe('memory'); expect(records.listPending('A', 1)).toHaveLength(1);
		records.clearDraft('A', 1); records.removePending('A', 1, 'op');
		expect(records.getDraft('A', 1)).toBeNull(); expect(records.listPending('A', 1)).toEqual([]);
	});
	it('purges both prefixes across closed conversations and seals later writes', () => {
		seed('A', 1); seed('B', 99); sessionStorage.setItem('pad:unrelated', 'keep');
		records.purgeAll(); records.setDraft('A', 1, 'teardown'); records.savePending('A', 1, 'op', 'late');
		expect(sessionStorage.length).toBe(1); expect(records.getDraft('A', 1)).toBeNull();
		expect(records.listPending('A', 1)).toEqual([]); expect(sessionStorage.getItem('pad:unrelated')).toBe('keep');
	});
	it('refuses writes for a non-current user', () => {
		records.setDraft('B', 1, 'wrong'); records.savePending('B', 1, 'op', 'wrong'); expect(sessionStorage.length).toBe(0);
	});
	it('purgeNotUser uses values, including user IDs with colons, never key segments', () => {
		seed('A', 1); seed('B:with:colon', 2);
		sessionStorage.setItem('pad:chat-draft:misleading:1', JSON.stringify({ userId: 'B:with:colon', n: 1, text: 'keep', replySeq: null }));
		records.purgeNotUser('B:with:colon'); expect(sessionStorage.length).toBe(3);
		expect(sessionStorage.getItem('pad:chat-draft:A:1')).toBeNull(); expect(sessionStorage.getItem('pad:chat-pending:A:1:op')).toBeNull();
		identity('B:with:colon'); expect(records.getDraft('B:with:colon', 2)?.text).toBe('B:with:colon');
		expect(records.listPending('B:with:colon', 2)).toHaveLength(1);
	});
	it('purgeNotUser with no identity keeps every record (session expiry reloads with an empty identity)', () => {
		seed('A', 1);
		records.purgeNotUser('');
		expect(sessionStorage.getItem('pad:chat-draft:A:1')).not.toBeNull();
		expect(sessionStorage.getItem('pad:chat-pending:A:1:op')).not.toBeNull();
	});
	it('signed-out notification purges and seals without confirmSignOut', () => {
		seed('A', 1); identity(''); expect(sessionStorage.length).toBe(0);
		auth.userId = 'A'; records.setDraft('A', 1, 'late teardown'); expect(sessionStorage.length).toBe(0);
	});
	it('establishing B purges A, keeps B, and unseals writes; returning to A cannot restore old text', () => {
		records.purgeAll(); seed('A', 1); seed('B', 2); identity('B');
		expect(sessionStorage.length).toBe(2); expect(records.getDraft('B', 2)?.text).toBe('B');
		expect(records.setDraft('B', 3, 'new')).toBe(true);
		identity('A'); expect(records.getDraft('A', 1)).toBeNull(); expect(sessionStorage.length).toBe(0);
	});
	it('same-user reauthentication from an expired session keeps records', () => {
		seed('A', 1); identity('A', ''); expect(records.getDraft('A', 1)?.text).toBe('A');
		expect(records.listPending('A', 1)).toHaveLength(1);
	});
	it('a second tab independently observes sign-out and seals its own sessionStorage', async () => {
		const firstStorage = sessionStorage; seed('A', 1);
		vi.resetModules(); vi.stubGlobal('sessionStorage', storage());
		const observing = await import('./chatRecords'); seed('A', 2);
		expect(auth.listeners).toHaveLength(2);
		auth.userId = ''; auth.listeners[1]('A'); // only the observing tab receives the event
		expect(sessionStorage.length).toBe(0);
		auth.userId = 'A'; observing.setDraft('A', 2, 'late'); expect(sessionStorage.length).toBe(0);
		expect(firstStorage.length).toBe(2);
	});
});
