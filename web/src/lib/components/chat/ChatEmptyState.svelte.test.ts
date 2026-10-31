// TASK-4186 PR B: the empty /messages help. Unknown is not none: only two good
// reads that found nothing may say "No one to message yet".
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render } from '@testing-library/svelte';
import { flushSync } from 'svelte';

// tick() is the component's only call after the clipboard write that is not a
// plain state write, so a spy on it observes whether copy() kept running after
// unmount (Svelte 5 does not warn on writes to a destroyed component).
const tickSpy = vi.hoisted(() => vi.fn());
vi.mock('svelte', async (importOriginal) => {
	const orig = await importOriginal<typeof import('svelte')>();
	return {
		...orig,
		tick: (...a: Parameters<typeof orig.tick>) => {
			tickSpy();
			return orig.tick(...a);
		}
	};
});
vi.mock('$lib/api/client', () => ({ api: { chat: {}, sessions: { list: vi.fn() } } }));
vi.mock('$lib/stores/auth.svelte', () => ({
	authStore: {
		authenticated: true,
		identityFence: () => () => true,
		onIdentityChange: () => () => {}
	}
}));
vi.mock('$lib/stores/chat.svelte', () => ({ chatStore: { version: 0, unreadByNumber: {} } }));

import ChatEmptyState from './ChatEmptyState.svelte';
import { chatInbox } from '$lib/stores/chatInbox.svelte';
import type { LiveSession } from '$lib/types';

const NONE = 'No one to message yet';

function set(over: {
	loaded?: boolean;
	failed?: boolean;
	sessions?: LiveSession[] | 'unavailable' | null;
}) {
	chatInbox.conversations = [];
	chatInbox.loaded = over.loaded ?? true;
	chatInbox.failed = over.failed ?? false;
	chatInbox.sessions = over.sessions === undefined ? [] : over.sessions;
	flushSync();
}

const session = (agent: string) =>
	({ id: `s-${agent}`, agent, label: '', accepting_work: true }) as unknown as LiveSession;

beforeEach(() => set({}));
afterEach(cleanup);

describe('ChatEmptyState', () => {
	it('an empty roster and no conversations shows the heading, body and both snippets', () => {
		const { container } = render(ChatEmptyState);
		const text = container.textContent ?? '';
		expect(text).toContain(NONE);
		expect(text).toContain('Chats are with your agents.');
		const codes = [...container.querySelectorAll('code')].map((c) => c.textContent);
		expect(codes).toEqual(['/pad:connect', 'pad watch --stream --for-session']);
		// Only the slash command is copyable; the monitor line is run by the harness.
		expect(container.querySelectorAll('button').length).toBe(1);
		expect(text).toContain('linked to this workspace (.pad.toml)');
		// Under another harness the session must be armed; the detail is in docs/api.md.
		expect(text).toContain('pad session arm');
		expect(text).toContain('Agent chat identity and arming');
		expect(text).not.toContain('PAD_AGENT=<name>');
	});

	it('states the name order exactly as ResolveAgentName resolves it', () => {
		const { container } = render(ChatEmptyState);
		// SOURCE OF TRUTH: internal/cli/agent_identity.go ResolveAgentName
		// (registry row, then .pad.toml agent_name, then $PAD_AGENT). The Go test
		// TestAgentNameCopyMatchesResolverOrder pins the behaviour; this literal
		// pins the copy. Change all three together.
		expect(container.textContent).toContain(
			'registered name, then agent_name in .pad.toml, then PAD_AGENT'
		);
	});

	it('a non-empty roster hides the help and says Start a conversation', () => {
		set({ sessions: [session('rook')] });
		const { container } = render(ChatEmptyState);
		const text = container.textContent ?? '';
		expect(text).toContain('Start a conversation');
		expect(text).not.toContain(NONE);
		expect(container.querySelector('code')).toBeNull();
	});

	it.each([
		['loading', { loaded: false }],
		['failed', { failed: true }],
		['sessions unavailable', { sessions: 'unavailable' as const }],
		['sessions not read yet', { sessions: null }]
	])('%s does not claim no one to message', (_n, over) => {
		set(over);
		const { container } = render(ChatEmptyState);
		expect(container.textContent).not.toContain(NONE);
		expect(container.querySelector('code')).toBeNull();
	});

	describe('copy button accessibility', () => {
		const writeText = vi.fn();
		beforeEach(() => {
			writeText.mockReset();
			tickSpy.mockClear();
			vi.useFakeTimers();
			Object.defineProperty(navigator, 'clipboard', {
				value: { writeText },
				configurable: true
			});
		});
		afterEach(() => vi.useRealTimers());

		it('labels the button per command', () => {
			const { getByRole } = render(ChatEmptyState);
			expect(getByRole('button', { name: 'Copy Claude Code command' })).toBeTruthy();
		});

		it('announces Copied in a polite live region', async () => {
			writeText.mockResolvedValue(undefined);
			const { getByRole, container } = render(ChatEmptyState);
			const live = container.querySelector('[aria-live="polite"]')!;
			expect(live.getAttribute('role')).toBe('status');
			await fireEvent.click(getByRole('button', { name: 'Copy Claude Code command' }));
			expect(writeText).toHaveBeenCalledWith('/pad:connect');
			expect(live.textContent).toBe('Copied');
		});

		it('says it could not copy when the clipboard rejects', async () => {
			writeText.mockRejectedValue(new Error('denied'));
			const { getByRole, container } = render(ChatEmptyState);
			await fireEvent.click(getByRole('button', { name: 'Copy Claude Code command' }));
			expect(container.querySelector('[aria-live="polite"]')!.textContent).toBe(
				"Couldn't copy \u2014 select the command"
			);
		});

		it('says it could not copy when navigator.clipboard is undefined', async () => {
			Object.defineProperty(navigator, 'clipboard', { value: undefined, configurable: true });
			const { getByRole, container } = render(ChatEmptyState);
			await fireEvent.click(getByRole('button', { name: 'Copy Claude Code command' }));
			expect(container.querySelector('[aria-live="polite"]')!.textContent).toContain(
				"Couldn't copy"
			);
		});

		it('each quick copy re-announces: the live region clears, then says Copied again', async () => {
			writeText.mockResolvedValue(undefined);
			const { getByRole, container } = render(ChatEmptyState);
			const live = container.querySelector('[aria-live="polite"]')!;
			const seen: string[] = [];
			const mo = new MutationObserver(() => seen.push(live.textContent ?? ''));
			mo.observe(live, { childList: true, characterData: true, subtree: true });
			const btn = getByRole('button', { name: 'Copy Claude Code command' });
			await fireEvent.click(btn);
			await fireEvent.click(btn);
			await Promise.resolve(); // let observer callbacks flush
			mo.disconnect();
			expect(live.textContent).toBe('Copied');
			// Without the clear, the second copy changes nothing and seen has one entry.
			expect(seen.filter((t) => t === 'Copied').length).toBe(2);
			expect(seen).toContain('');
		});

		it('a second quick copy is not reset by the first timer', async () => {
			writeText.mockResolvedValue(undefined);
			const { getByRole, container } = render(ChatEmptyState);
			const btn = getByRole('button', { name: 'Copy Claude Code command' });
			await fireEvent.click(btn);
			await vi.advanceTimersByTimeAsync(0); // settle writeText and announce(): the timer is set
			vi.advanceTimersByTime(1000);
			await fireEvent.click(btn);
			await vi.advanceTimersByTimeAsync(0);
			vi.advanceTimersByTime(700); // 1700ms after the first copy, 700ms after the second
			flushSync();
			expect(btn.textContent?.trim()).toBe('Copied');
			vi.advanceTimersByTime(900);
			flushSync();
			expect(btn.textContent?.trim()).toBe('Copy');
		});

		it('two clicks while writeText is pending: the first timer cannot reset the second copy early', async () => {
			const resolvers: Array<() => void> = [];
			writeText.mockImplementation(() => new Promise<void>((r) => resolvers.push(r)));
			const { getByRole, container } = render(ChatEmptyState);
			const live = container.querySelector('[aria-live="polite"]')!;
			const btn = getByRole('button', { name: 'Copy Claude Code command' });
			await fireEvent.click(btn);
			await fireEvent.click(btn);
			expect(resolvers.length).toBe(2);
			resolvers[0](); // first copy lands at t=0: its timer expires at 1500
			await vi.advanceTimersByTimeAsync(1000);
			resolvers[1](); // second lands at t=1000: its timer expires at 2500
			await vi.advanceTimersByTimeAsync(1400); // t=2400, past the first timer
			flushSync();
			expect(live.textContent).toBe('Copied');
			expect(btn.textContent?.trim()).toBe('Copied');
			await vi.advanceTimersByTimeAsync(200);
			flushSync();
			expect(btn.textContent?.trim()).toBe('Copy');
		});

		it('clears its timers on destroy', async () => {
			writeText.mockResolvedValue(undefined);
			const { getByRole, unmount } = render(ChatEmptyState);
			await fireEvent.click(getByRole('button', { name: 'Copy Claude Code command' }));
			await vi.advanceTimersByTimeAsync(0); // settle writeText and tick()
			expect(vi.getTimerCount()).toBe(1);
			unmount();
			expect(vi.getTimerCount()).toBe(0);
		});

		it('a copy failure keeps its message past an earlier success timer', async () => {
			const { getByRole, container } = render(ChatEmptyState);
			const live = container.querySelector('[aria-live="polite"]')!;
			const btn = getByRole('button', { name: 'Copy Claude Code command' });
			writeText.mockResolvedValueOnce(undefined);
			await fireEvent.click(btn);
			await vi.advanceTimersByTimeAsync(500); // success timer expires at ~1500
			writeText.mockRejectedValueOnce(new Error('denied'));
			await fireEvent.click(btn);
			await vi.advanceTimersByTimeAsync(2000); // well past the first timer
			flushSync();
			expect(live.textContent).toBe("Couldn't copy \u2014 select the command");
			expect(vi.getTimerCount()).toBe(0);
		});

		it('a stale attempt that rejects after a newer success does not touch its status or timer', async () => {
			const settle: Array<{ ok: () => void; fail: () => void }> = [];
			writeText.mockImplementation(
				() => new Promise<void>((ok, bad) => settle.push({ ok, fail: () => bad(new Error('denied')) }))
			);
			const { getByRole, container } = render(ChatEmptyState);
			const live = container.querySelector('[aria-live="polite"]')!;
			const btn = getByRole('button', { name: 'Copy Claude Code command' });
			await fireEvent.click(btn); // A
			await fireEvent.click(btn); // B
			settle[1].ok();
			await vi.advanceTimersByTimeAsync(0); // B lands, its timer is set
			expect(live.textContent).toBe('Copied');
			settle[0].fail(); // A rejects late
			await vi.advanceTimersByTimeAsync(1000);
			flushSync();
			expect(live.textContent).toBe('Copied');
			expect(btn.textContent?.trim()).toBe('Copied');
			expect(vi.getTimerCount()).toBe(1);
			await vi.advanceTimersByTimeAsync(499); // 1499ms after B landed
			flushSync();
			expect(btn.textContent?.trim()).toBe('Copied');
			await vi.advanceTimersByTimeAsync(1); // B's own 1500ms
			flushSync();
			expect(btn.textContent?.trim()).toBe('Copy');
			expect(live.textContent).toBe('');
		});

		it('an attempt that resolved and is awaiting tick cannot overwrite a newer failure', async () => {
			const settle: Array<{ ok: () => void; fail: () => void }> = [];
			writeText.mockImplementation(
				() => new Promise<void>((ok, bad) => settle.push({ ok, fail: () => bad(new Error('denied')) }))
			);
			const { getByRole, container } = render(ChatEmptyState);
			const live = container.querySelector('[aria-live="polite"]')!;
			const btn = getByRole('button', { name: 'Copy Claude Code command' });
			const seen: string[] = [];
			const mo = new MutationObserver(() => seen.push(live.textContent ?? ''));
			mo.observe(live, { childList: true, characterData: true, subtree: true });
			btn.click(); // A
			settle[0].ok();
			await Promise.resolve(); // A's continuation runs up to its await tick()
			btn.click(); // B starts while A is awaiting tick
			settle[1].fail();
			await vi.advanceTimersByTimeAsync(0); // both attempts have run to the end
			expect(vi.getTimerCount()).toBe(0); // A set no timer after going stale
			await vi.advanceTimersByTimeAsync(2000);
			flushSync();
			mo.disconnect();
			expect(live.textContent).toBe("Couldn't copy \u2014 select the command");
			expect(seen).not.toContain('Copied'); // A never announces over B
			expect(btn.textContent?.trim()).toBe('Copy');
		});

		it('a stale success timer does not reset the state of a newer attempt still pending', async () => {
			let calls = 0;
			let resolveB!: () => void;
			writeText.mockImplementation(() =>
				++calls === 1 ? Promise.resolve() : new Promise<void>((r) => (resolveB = r))
			);
			const { getByRole } = render(ChatEmptyState);
			const btn = getByRole('button', { name: 'Copy Claude Code command' });
			await fireEvent.click(btn); // A succeeds, timer at 1500
			await vi.advanceTimersByTimeAsync(0);
			expect(btn.textContent?.trim()).toBe('Copied');
			await fireEvent.click(btn); // B pending
			await vi.advanceTimersByTimeAsync(1600); // A's timer fires while B is the latest
			flushSync();
			expect(btn.textContent?.trim()).toBe('Copied');
			resolveB();
			await vi.advanceTimersByTimeAsync(1500);
			flushSync();
			expect(btn.textContent?.trim()).toBe('Copy');
		});

		it('two failures in a row are each announced: the live region clears, then repeats the message', async () => {
			writeText.mockRejectedValue(new Error('denied'));
			const { getByRole, container } = render(ChatEmptyState);
			const live = container.querySelector('[aria-live="polite"]')!;
			const seen: string[] = [];
			const mo = new MutationObserver(() => seen.push(live.textContent ?? ''));
			mo.observe(live, { childList: true, characterData: true, subtree: true });
			const btn = getByRole('button', { name: 'Copy Claude Code command' });
			await fireEvent.click(btn);
			await vi.advanceTimersByTimeAsync(0);
			await fireEvent.click(btn);
			await vi.advanceTimersByTimeAsync(0);
			await Promise.resolve();
			mo.disconnect();
			const msg = "Couldn't copy \u2014 select the command";
			expect(live.textContent).toBe(msg);
			expect(seen.filter((t) => t === msg).length).toBe(2);
			expect(seen).toContain('');
		});

		it('writes no state and calls no tick() when writeText settles after unmount', async () => {
			let resolve!: () => void;
			writeText.mockImplementation(() => new Promise<void>((r) => (resolve = r)));
			const { getByRole, unmount } = render(ChatEmptyState);
			await fireEvent.click(getByRole('button', { name: 'Copy Claude Code command' }));
			tickSpy.mockClear(); // testing-library's own tick() inside fireEvent is not ours
			unmount();
			resolve();
			await vi.advanceTimersByTimeAsync(0);
			expect(tickSpy).not.toHaveBeenCalled();
			expect(vi.getTimerCount()).toBe(0);
		});
	});
});
