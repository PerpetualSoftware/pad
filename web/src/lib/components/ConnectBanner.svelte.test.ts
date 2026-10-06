// BUG-3447: the "Connect an AI agent" banner learned of agent activity only
// on a workspace change or when its own modal closed, so it stayed up while an
// agent was visibly working. It now re-checks on item events (coalesced) while
// it is showing, without blanking itself during the re-check, and stops once
// activity is seen.
import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest';
import { cleanup, render } from '@testing-library/svelte';
import { tick } from 'svelte';

const mocks = vi.hoisted(() => ({
	callbacks: [] as Array<(e: Record<string, unknown>) => void>,
	answers: [] as Array<{ has_agent_activity: boolean } | Promise<{ has_agent_activity: boolean }>>,
	gets: 0,
}));

vi.mock('$lib/api/client', () => ({
	api: {
		dashboard: {
			get: vi.fn(() => {
				mocks.gets++;
				return Promise.resolve(mocks.answers.shift() ?? { has_agent_activity: false });
			}),
		},
	},
}));
vi.mock('$lib/services/sse.svelte', () => ({
	sseService: {
		onItemEvent: (cb: (e: Record<string, unknown>) => void) => {
			mocks.callbacks.push(cb);
			return () => (mocks.callbacks = mocks.callbacks.filter((c) => c !== cb));
		},
	},
}));
vi.mock('$lib/stores/auth.svelte', () => ({
	authStore: { mcpPublicUrl: '', identityEpoch: 0, identityFence: () => () => true, onIdentityChange: () => () => {} },
}));

import ConnectBanner from './ConnectBanner.svelte';

async function flush() {
	for (let i = 0; i < 10; i++) {
		await Promise.resolve();
		await tick();
	}
}
const banner = () => document.querySelector('.banner');
const fire = (e: Record<string, unknown>) => {
	for (const cb of [...mocks.callbacks]) cb(e);
};

beforeEach(() => {
	vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
	mocks.callbacks = [];
	mocks.answers = [];
	mocks.gets = 0;
	try {
		localStorage.clear();
	} catch {}
});
afterEach(() => {
	cleanup();
	vi.useRealTimers();
});

describe('ConnectBanner follows agent activity live (BUG-3447)', () => {
	it('hides once an item event shows the agent at work, without blanking during the re-check', async () => {
		mocks.answers = [{ has_agent_activity: false }];
		render(ConnectBanner, { props: { wsSlug: 'ws', serverUrl: 'http://x' } });
		await flush();
		expect(banner(), 'control: no agent activity yet, the banner shows').not.toBeNull();
		expect(mocks.callbacks.length, 'the banner did not subscribe to item events').toBeGreaterThan(0);

		let answer!: (v: { has_agent_activity: boolean }) => void;
		mocks.answers = [new Promise((r) => (answer = r))];
		fire({ type: 'item_created', item_id: 'i1', actor: 'agent' });
		fire({ type: 'item_created', item_id: 'i2', actor: 'agent' });
		await vi.advanceTimersByTimeAsync(2000);
		await flush();
		expect(mocks.gets, 'one coalesced re-check').toBe(2);
		expect(banner(), 'the banner blanked while the re-check was out').not.toBeNull();
		answer({ has_agent_activity: true });
		await flush();
		expect(banner()).toBeNull();
	});

	it('does not re-check once agent activity is known', async () => {
		mocks.answers = [{ has_agent_activity: true }];
		render(ConnectBanner, { props: { wsSlug: 'ws', serverUrl: 'http://x' } });
		await flush();
		fire({ type: 'item_created', item_id: 'i1' });
		await vi.advanceTimersByTimeAsync(2000);
		await flush();
		expect(mocks.gets).toBe(1);
	});

	it('a re-check armed in one workspace does not fire into the next (codex r1)', async () => {
		mocks.answers = [{ has_agent_activity: false }, { has_agent_activity: false }];
		const r = render(ConnectBanner, { props: { wsSlug: 'ws', serverUrl: 'http://x' } });
		await flush();
		fire({ type: 'item_created', item_id: 'i1' });
		await r.rerender({ wsSlug: 'other', serverUrl: 'http://x' });
		await flush();
		expect(mocks.gets, "control: the switch re-checks the next workspace").toBe(2);
		await vi.advanceTimersByTimeAsync(2000);
		await flush();
		expect(mocks.gets, 'the stale re-check fired into the next workspace').toBe(2);
		expect(banner(), "the next workspace's own answer stands").not.toBeNull();
	});

	it('an item event during the first check still re-checks: that answer may predate the write (codex r2)', async () => {
		let answer!: (v: { has_agent_activity: boolean }) => void;
		mocks.answers = [new Promise((r) => (answer = r)), { has_agent_activity: true }];
		render(ConnectBanner, { props: { wsSlug: 'ws', serverUrl: 'http://x' } });
		await flush();
		fire({ type: 'item_created', item_id: 'i1', actor: 'agent' });
		answer({ has_agent_activity: false });
		await flush();
		await vi.advanceTimersByTimeAsync(2000);
		await flush();
		expect(mocks.gets, 'the event during the first check was dropped').toBe(2);
		expect(banner()).toBeNull();
	});

	it('a quiet re-check that overtakes the first check and fails leaves the banner showing (codex r3)', async () => {
		let firstAnswer!: (v: { has_agent_activity: boolean }) => void;
		const failed = Promise.reject(new Error('offline'));
		failed.catch(() => {}); // consumed later by the mock; not unhandled
		mocks.answers = [new Promise((r) => (firstAnswer = r)), failed];
		render(ConnectBanner, { props: { wsSlug: 'ws', serverUrl: 'http://x' } });
		await flush();
		fire({ type: 'item_created', item_id: 'i1' });
		await vi.advanceTimersByTimeAsync(2000);
		await flush();
		expect(mocks.gets, 'the quiet re-check went out while the first check was pending').toBe(2);
		firstAnswer({ has_agent_activity: false }); // superseded by the re-check
		await flush();
		expect(banner(), 'unknown after a failed re-check hid the banner for good').not.toBeNull();
	});

	it("an agent's collection is agent activity too: it re-checks on collection_updated (lead)", async () => {
		mocks.answers = [{ has_agent_activity: false }, { has_agent_activity: true }];
		render(ConnectBanner, { props: { wsSlug: 'ws', serverUrl: 'http://x' } });
		await flush();
		fire({ type: 'collection_updated', collection_id: 'c1' });
		await vi.advanceTimersByTimeAsync(2000);
		await flush();
		expect(mocks.gets).toBe(2);
		expect(banner()).toBeNull();
	});
});
