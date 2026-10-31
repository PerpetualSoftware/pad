// TASK-4186 PR B: the empty /messages help. Unknown is not none: only two good
// reads that found nothing may say "No one to message yet".
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render } from '@testing-library/svelte';
import { flushSync } from 'svelte';

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

		it('a second quick copy is not reset by the first timer', async () => {
			writeText.mockResolvedValue(undefined);
			const { getByRole, container } = render(ChatEmptyState);
			const btn = getByRole('button', { name: 'Copy Claude Code command' });
			await fireEvent.click(btn);
			vi.advanceTimersByTime(1000);
			await fireEvent.click(btn);
			vi.advanceTimersByTime(700); // 1700ms after the first copy, 700ms after the second
			flushSync();
			expect(btn.textContent?.trim()).toBe('Copied');
			vi.advanceTimersByTime(900);
			flushSync();
			expect(btn.textContent?.trim()).toBe('Copy');
		});
	});
});
