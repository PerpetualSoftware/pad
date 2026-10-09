// TASK-2253: the CLI sign-in approval page offers Deny, says who asked, and
// tells the approver what to do if it was not them. The agent string is the
// requester's own text, so it renders as text and is labelled as theirs.
import { describe, it, expect, vi, afterEach, beforeEach } from 'vitest';
import { cleanup, fireEvent, render, screen } from '@testing-library/svelte';
import { page } from '$app/state';

const state = vi.hoisted(() => ({
	session: {} as Record<string, unknown> | Error,
	approve: null as Error | null,
	deny: null as Error | null,
	denied: 0
}));

vi.mock('$lib/api/client', () => {
	class PadApiError extends Error {
		code: string;
		constructor(code: string, message: string) {
			super(message);
			this.code = code;
		}
	}
	return {
		PadApiError,
		api: {
			auth: {
				session: async () => ({ authenticated: true }),
				me: async () => ({ id: 'u1', name: 'Dana', username: 'dana', email: 'dana@example.com' }),
				logout: async () => {},
				cli: {
					getSession: async () => {
						if (state.session instanceof Error) throw state.session;
						return state.session;
					},
					approveSession: async () => {
						if (state.approve) throw state.approve;
						return { approved: true };
					},
					denySession: async () => {
						state.denied++;
						if (state.deny) throw state.deny;
						return { denied: true };
					}
				}
			}
		}
	};
});
vi.mock('$app/navigation', () => ({ goto: vi.fn(async () => {}) }));

import { PadApiError } from '$lib/api/client';
import CLIAuthPage from './+page.svelte';

async function settle() {
	for (let i = 0; i < 20; i++) await Promise.resolve();
}

function apiError(code: string, message: string): Error {
	return new (PadApiError as unknown as new (code: string, message: string) => Error)(code, message);
}

beforeEach(() => {
	page.params.code = 'abc123';
	state.session = {
		status: 'pending',
		created_at: new Date(Date.now() - 2 * 60_000).toISOString(),
		requester_ip: '203.0.113.4',
		requester_user_agent: '<b>Pad official</b> pad-cli/1.0'
	};
	state.approve = null;
	state.deny = null;
	state.denied = 0;
});
afterEach(() => cleanup());

describe('CLI sign-in approval page (TASK-2253)', () => {
	it('says who asked, labels the agent as their claim, and warns before the buttons', async () => {
		const { container } = render(CLIAuthPage);
		await settle();

		expect(screen.getByText('Requested 2 minutes ago from 203.0.113.4.')).toBeTruthy();
		// Text, not markup: the tags show as characters and build no element.
		const agent = screen.getByText('<b>Pad official</b> pad-cli/1.0');
		expect(agent.querySelector('b')).toBeNull();
		expect(container.querySelector('.request-context b')).toBeNull();
		expect(screen.getByText('reported by the requester')).toBeTruthy();

		const warning = container.querySelector('.warning')!;
		expect(warning.textContent).toBe('Only approve if you just ran pad auth login yourself.');
		const deny = screen.getByRole('button', { name: 'Deny' });
		const approve = screen.getByRole('button', { name: 'Approve' });
		// The warning comes before the buttons in reading order.
		expect(warning.compareDocumentPosition(deny) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
		expect(warning.compareDocumentPosition(approve) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
	});

	it('Deny refuses the request and says nothing was approved', async () => {
		render(CLIAuthPage);
		await settle();
		await fireEvent.click(screen.getByRole('button', { name: 'Deny' }));
		await settle();
		expect(state.denied).toBe(1);
		expect(screen.getByText('Request denied')).toBeTruthy();
		expect(screen.getByText(/Nothing was approved/)).toBeTruthy();
		expect(screen.queryByRole('button', { name: 'Approve' })).toBeNull();
	});

	it('a deny that lost to an approval says it was approved', async () => {
		state.deny = apiError('already_approved', 'This CLI session has already been approved');
		render(CLIAuthPage);
		await settle();
		await fireEvent.click(screen.getByRole('button', { name: 'Deny' }));
		await settle();
		expect(screen.getByText('Already approved')).toBeTruthy();
	});

	it('opening a link that was already denied shows the denial, not a broken link', async () => {
		state.session = apiError('cli_auth_denied', 'This sign-in was denied in the browser.');
		render(CLIAuthPage);
		await settle();
		expect(screen.getByText('Request denied')).toBeTruthy();
		expect(screen.queryByText(/invalid or has expired/)).toBeNull();
	});

	it('after approving, says what to do if it was not you', async () => {
		render(CLIAuthPage);
		await settle();
		await fireEvent.click(screen.getByRole('button', { name: 'Approve' }));
		await settle();
		expect(screen.getByText('Authorized')).toBeTruthy();
		const link = screen.getByRole('link', { name: 'Change your password' });
		expect(link.getAttribute('href')).toBe('/console/settings');
	});

	it('shows no context block when the server reported none (a session from before the migration)', async () => {
		state.session = { status: 'pending' };
		const { container } = render(CLIAuthPage);
		await settle();
		expect(container.querySelector('.request-context')).toBeNull();
		expect(screen.getByRole('button', { name: 'Deny' })).toBeTruthy();
	});
});
