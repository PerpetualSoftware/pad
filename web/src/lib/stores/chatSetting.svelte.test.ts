import { describe, expect, it, vi, beforeEach } from 'vitest';

/** TASK-4315: the Chat setting store reads the session user and writes through the API behind an identity fence. */
const state = vi.hoisted(() => ({
	user: null as null | { id: string; chat_enabled?: boolean },
	epoch: 0,
	setChatEnabledCalls: [] as Array<[string, boolean]>,
	put: undefined as undefined | ((v: boolean) => Promise<{ chat_enabled?: boolean }>)
}));

vi.mock('$lib/api/client', () => ({
	api: { auth: { setChatEnabled: (v: boolean) => state.put!(v) } }
}));
vi.mock('$lib/stores/auth.svelte', () => ({
	authStore: {
		get user() { return state.user; },
		get userId() { return state.user?.id ?? ''; },
		identityFence() { const captured = state.epoch; return () => state.epoch === captured; },
		setChatEnabled(id: string, v: boolean) {
			state.setChatEnabledCalls.push([id, v]);
			if (state.user && state.user.id === id) state.user.chat_enabled = v;
		}
	}
}));

const { chatSetting } = await import('./chatSetting.svelte');

function deferred<T>() {
	let resolve!: (v: T) => void;
	let reject!: (e: unknown) => void;
	const promise = new Promise<T>((res, rej) => { resolve = res; reject = rej; });
	return { promise, resolve, reject };
}

beforeEach(() => {
	state.user = { id: 'u1' };
	state.epoch = 0;
	state.setChatEnabledCalls = [];
	state.put = async (v) => ({ chat_enabled: v });
});

describe('chatSetting', () => {
	it('is off for a signed-out caller, an absent field and an explicit false', () => {
		state.user = null;
		expect(chatSetting.enabled).toBe(false);
		state.user = { id: 'u1' };
		expect(chatSetting.enabled).toBe(false);
		state.user = { id: 'u1', chat_enabled: false };
		expect(chatSetting.enabled).toBe(false);
	});

	it('is on only when the session user says true', () => {
		state.user = { id: 'u1', chat_enabled: true };
		expect(chatSetting.enabled).toBe(true);
	});

	it('set() writes the server value into the session for the same user', async () => {
		expect(await chatSetting.set(true)).toBe(true);
		expect(state.setChatEnabledCalls).toEqual([['u1', true]]);
		expect(chatSetting.enabled).toBe(true);
	});

	it('set() stores what the server answered, not what was asked', async () => {
		state.put = async () => ({ chat_enabled: false });
		expect(await chatSetting.set(true)).toBe(false);
		expect(chatSetting.enabled).toBe(false);
	});

	it('a failed write rejects and leaves the session alone', async () => {
		state.put = async () => { throw new Error('boom'); };
		await expect(chatSetting.set(true)).rejects.toThrow('boom');
		expect(state.setChatEnabledCalls).toEqual([]);
		expect(chatSetting.enabled).toBe(false);
	});

	it('an answer that settles after an identity change is not written to the new session', async () => {
		const d = deferred<{ chat_enabled?: boolean }>();
		state.put = () => d.promise;
		const pending = chatSetting.set(true);
		state.epoch++;
		state.user = { id: 'u2' };
		d.resolve({ chat_enabled: true });
		await pending;
		expect(state.setChatEnabledCalls).toEqual([]);
		expect(chatSetting.enabled).toBe(false);
	});
});
