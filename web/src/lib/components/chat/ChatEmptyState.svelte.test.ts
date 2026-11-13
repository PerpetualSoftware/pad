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
		// TASK-4515: the shared centred state, an h2 under the page's h1, and a real Button.
		expect(container.querySelector('.chat-state h2')?.textContent).toBe(NONE);
		expect(container.querySelector('button')!.matches('.btn.secondary')).toBe(true);
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
		// Real macrotask: lets every pending microtask chain (writeText, tick) run out.
		const flush = () => new Promise<void>((r) => setTimeout(r, 0));
		beforeEach(() => {
			writeText.mockReset();
			tickSpy.mockClear();
			Object.defineProperty(navigator, 'clipboard', {
				value: { writeText },
				configurable: true
			});
		});

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
			await flush(); // a real second click is a later task; observer batches per microtask
			await fireEvent.click(btn);
			await flush();
			mo.disconnect();
			expect(live.textContent).toBe('Copied');
			// Without the click's reset of status, the second copy changes nothing and seen
			// has one entry.
			expect(seen.filter((t) => t === 'Copied').length).toBe(2);
			expect(seen).toContain('');
		});

		const COPY = { name: 'Copy Claude Code command' };
		const FAIL = "Couldn't copy \u2014 select the command";
		const deferred = () => {
			const settle: Array<{ ok: () => void; fail: () => void }> = [];
			writeText.mockImplementation(
				() =>
					new Promise<void>((ok, bad) =>
						settle.push({
							ok,
							fail: () => bad(new Error('denied'))
						})
					)
			);
			return settle;
		};

		it('a success followed by a failing click shows the failure and not Copied', async () => {
			const { getByRole, container } = render(ChatEmptyState);
			const live = container.querySelector('[aria-live="polite"]')!;
			const btn = getByRole('button', COPY);
			writeText.mockResolvedValueOnce(undefined);
			await fireEvent.click(btn);
			await flush();
			expect(btn.textContent?.trim()).toBe('Copied');
			writeText.mockRejectedValueOnce(new Error('denied'));
			await fireEvent.click(btn);
			await flush();
			expect(live.textContent).toBe(FAIL);
			expect(btn.textContent?.trim()).toBe('Copy');
		});

		it('a stale attempt that rejects after a newer success leaves the success standing', async () => {
			const settle = deferred();
			const { getByRole, container } = render(ChatEmptyState);
			const live = container.querySelector('[aria-live="polite"]')!;
			const btn = getByRole('button', COPY);
			await fireEvent.click(btn); // A
			await fireEvent.click(btn); // B
			settle[1].ok();
			await flush();
			expect(live.textContent).toBe('Copied');
			settle[0].fail(); // A rejects late
			await flush();
			flushSync();
			expect(live.textContent).toBe('Copied');
			expect(btn.textContent?.trim()).toBe('Copied');
		});

		it('an attempt that resolved and is awaiting tick cannot overwrite a newer failure', async () => {
			const settle = deferred();
			const { getByRole, container } = render(ChatEmptyState);
			const live = container.querySelector('[aria-live="polite"]')!;
			const btn = getByRole('button', COPY);
			const seen: string[] = [];
			const mo = new MutationObserver(() => seen.push(live.textContent ?? ''));
			mo.observe(live, { childList: true, characterData: true, subtree: true });
			btn.click(); // A
			settle[0].ok();
			await Promise.resolve(); // A's continuation runs up to its await tick()
			btn.click(); // B starts while A is awaiting tick
			settle[1].fail();
			await flush(); // both attempts have run to the end
			flushSync();
			mo.disconnect();
			expect(live.textContent).toBe(FAIL);
			expect(seen).not.toContain('Copied'); // A never announces over B
			expect(btn.textContent?.trim()).toBe('Copy');
		});

		it("a pending newer attempt does not inherit the previous attempt's Copied (A paused in announce, B pending)", async () => {
			const settle = deferred();
			const { getByRole, container } = render(ChatEmptyState);
			const live = container.querySelector('[aria-live="polite"]')!;
			const btn = getByRole('button', COPY);
			btn.click(); // A
			settle[0].ok();
			await Promise.resolve(); // A has set copied and is paused at tick() in announce()
			btn.click(); // B: writeText never settles in this test
			await flush();
			flushSync();
			expect(settle.length).toBe(2);
			expect(btn.textContent?.trim()).toBe('Copy');
			expect(btn.textContent).not.toContain('Copied');
			expect(live.textContent).toBe('');
		});

		it('a click resets a prior Copied immediately, before writeText settles', async () => {
			const { getByRole, container } = render(ChatEmptyState);
			const live = container.querySelector('[aria-live="polite"]')!;
			const btn = getByRole('button', COPY);
			writeText.mockResolvedValueOnce(undefined);
			await fireEvent.click(btn);
			await flush();
			expect(btn.textContent?.trim()).toBe('Copied');
			expect(live.textContent).toBe('Copied');
			writeText.mockImplementationOnce(() => new Promise<void>(() => {}));
			btn.click();
			flushSync(); // synchronously after the click event
			expect(btn.textContent?.trim()).toBe('Copy');
			expect(live.textContent).toBe('');
		});

		it('the latest success shows Copied indefinitely', async () => {
			vi.useFakeTimers();
			try {
				writeText.mockResolvedValue(undefined);
				const { getByRole, container } = render(ChatEmptyState);
				const live = container.querySelector('[aria-live="polite"]')!;
				const btn = getByRole('button', COPY);
				await fireEvent.click(btn);
				await vi.advanceTimersByTimeAsync(0);
				expect(btn.textContent?.trim()).toBe('Copied');
				await vi.advanceTimersByTimeAsync(10 * 60 * 1000);
				flushSync();
				expect(btn.textContent?.trim()).toBe('Copied');
				expect(live.textContent).toBe('Copied');
				expect(vi.getTimerCount()).toBe(0);
			} finally {
				vi.useRealTimers();
			}
		});

		it('two failures in a row are each announced: the live region clears, then repeats the message', async () => {
			writeText.mockRejectedValue(new Error('denied'));
			const { getByRole, container } = render(ChatEmptyState);
			const live = container.querySelector('[aria-live="polite"]')!;
			const seen: string[] = [];
			const mo = new MutationObserver(() => seen.push(live.textContent ?? ''));
			mo.observe(live, { childList: true, characterData: true, subtree: true });
			const btn = getByRole('button', COPY);
			btn.click();
			await flush();
			// The second click's reset of status ('') and the failure text must reach the DOM
			// as separate changes; the click's reset provides the '' (announce() no longer
			// clears), and announce()'s tick keeps it from batching with the failure text.
			// The sync-throw path, where the tick matters, is pinned below.
			btn.click();
			await flush();
			mo.disconnect();
			expect(live.textContent).toBe(FAIL);
			expect(seen.filter((t) => t === FAIL).length).toBe(2);
			expect(seen).toContain('');
		});

		it('a synchronous writeText throw, clicked twice, re-announces the failure each time', async () => {
			// navigator.clipboard undefined: the throw is synchronous, so only announce()'s
			// tick separates the click's '' from the failure text.
			Object.defineProperty(navigator, 'clipboard', { value: undefined, configurable: true });
			const { getByRole, container } = render(ChatEmptyState);
			const live = container.querySelector('[aria-live="polite"]')!;
			const seen: string[] = [];
			const mo = new MutationObserver(() => seen.push(live.textContent ?? ''));
			mo.observe(live, { childList: true, characterData: true, subtree: true });
			const btn = getByRole('button', COPY);
			btn.click();
			await flush();
			btn.click();
			await flush();
			mo.disconnect();
			expect(live.textContent).toBe(FAIL);
			expect(seen.filter((t) => t === FAIL).length).toBe(2);
			expect(seen.filter((t) => t === '').length).toBe(1);
		});

		it('writes no state and calls no tick() when writeText settles after unmount', async () => {
			let resolve!: () => void;
			writeText.mockImplementation(() => new Promise<void>((r) => (resolve = r)));
			const { getByRole, unmount } = render(ChatEmptyState);
			await fireEvent.click(getByRole('button', COPY));
			tickSpy.mockClear(); // testing-library's own tick() inside fireEvent is not ours
			unmount();
			resolve();
			await flush();
			expect(tickSpy).not.toHaveBeenCalled();
		});
	});
});
