import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen, within } from '@testing-library/svelte';
import { page } from '$app/state';
import { goto } from '$app/navigation';
import { api } from '$lib/api/client';
import { uiStore } from '$lib/stores/ui.svelte';
import type { Workspace } from '$lib/types';

vi.mock('$lib/components/home/DesktopHome.svelte', () => import('../../test/StubComponent.svelte'));
vi.mock('$lib/stores/breakpoint.svelte', () => ({ viewport: { isMobile: true } }));
vi.mock('$lib/api/client', () => ({ api: { workspaces: { list: vi.fn() } } }));
vi.mock('$lib/stores/auth.svelte', () => ({
	authStore: { userId: 'owner', user: { username: 'river-pilot' } },
}));
vi.mock('$lib/stores/ui.svelte', () => ({
	uiStore: { openCreateWorkspace: vi.fn(), clearAddWorkspaceHighlight: vi.fn() },
}));
vi.mock('$app/navigation', () => ({ goto: vi.fn() }));
vi.mock('$lib/components/tutorials/TutorialSuggestion.svelte', () => import('../../test/StubComponent.svelte'));

import ConsolePage from './+page.svelte';

function deferredWorkspaces() {
	let resolve!: (workspaces: Workspace[]) => void;
	let reject!: (error: Error) => void;
	const promise = new Promise<Workspace[]>((res, rej) => { resolve = res; reject = rej; });
	return { promise, resolve, reject };
}

function expectNoWorkspaceSections() {
	expect(screen.queryByRole('heading', { name: 'Your Workspaces' })).not.toBeInTheDocument();
	expect(screen.queryByRole('heading', { name: 'Shared With Me' })).not.toBeInTheDocument();
	expect(screen.queryByText("You don't have any workspaces yet.")).not.toBeInTheDocument();
	expect(screen.queryByText('No workspaces have been shared with you yet.')).not.toBeInTheDocument();
}

beforeEach(() => {
	vi.resetAllMocks();
	page.url = new URL('http://localhost/');
});
afterEach(cleanup);

it('announces the initial load without workspace sections (BUG-4334)', () => {
	vi.mocked(api.workspaces.list).mockReturnValue(deferredWorkspaces().promise);
	render(ConsolePage);

	expect(screen.getByRole('status')).toHaveTextContent('Loading workspaces...');
	expectNoWorkspaceSections();
});

it('retries a failure without an empty-account flash or repeating openCreate (BUG-4334)', async () => {
	const retry = deferredWorkspaces();
	vi.mocked(api.workspaces.list)
		.mockRejectedValueOnce(new Error('Internal server details'))
		.mockReturnValueOnce(retry.promise);
	page.url = new URL('http://localhost/?openCreate=1');
	const { container } = render(ConsolePage);
	const addedText: string[] = [];
	const observer = new MutationObserver((records) => {
		for (const record of records) {
			for (const node of record.addedNodes) addedText.push(node.textContent ?? '');
		}
	});
	observer.observe(container, { childList: true, subtree: true });
	try {
		const alert = await screen.findByRole('alert');
		expect(within(alert).getByRole('heading', { name: 'Couldn’t load your workspaces.' })).toBeInTheDocument();
		expect(within(alert).getByText('Check your connection, then try again.')).toBeInTheDocument();
		expect(container).not.toHaveTextContent('Internal server details');
		expectNoWorkspaceSections();
		const button = within(alert).getByRole('button', { name: 'Retry' });
		expect(button).toBeEnabled();
		expect(button).toHaveClass('secondary');
		await fireEvent.click(button);

		expect(api.workspaces.list).toHaveBeenCalledTimes(2);
		expect(screen.getByRole('button', { name: 'Retry' })).toBeDisabled();
		expect(screen.getByRole('status')).toHaveTextContent('Loading workspaces...');
		expectNoWorkspaceSections();
		expect(uiStore.openCreateWorkspace).toHaveBeenCalledTimes(1);
		expect(goto).toHaveBeenCalledExactlyOnceWith('/', { replaceState: true, reset: false });

		retry.resolve([
			{ id: 'owned', slug: 'harbor', name: 'Harbor', owner_id: 'owner', created_at: '2026-01-01' },
			{ id: 'shared', slug: 'meadow', name: 'Meadow', owner_id: 'friend', owner_username: 'friend', created_at: '2026-01-01' },
		] as Workspace[]);
		expect(await screen.findByRole('heading', { name: 'Your Workspaces' })).toBeInTheDocument();
		expect(screen.getByRole('heading', { name: 'Shared With Me' })).toBeInTheDocument();
		expect(screen.getByRole('link', { name: /Harbor/ })).toBeInTheDocument();
		expect(screen.getByRole('link', { name: /Meadow/ })).toBeInTheDocument();
		expect(screen.queryByRole('alert')).not.toBeInTheDocument();
		expect(screen.queryByRole('status')).not.toBeInTheDocument();
		expect(addedText.join(' ')).not.toContain("You don't have any workspaces yet.");
		expect(addedText.join(' ')).not.toContain('No workspaces have been shared with you yet.');
	} finally {
		observer.disconnect();
	}
});

it('enables Retry again when a retry fails (BUG-4334)', async () => {
	const retry = deferredWorkspaces();
	vi.mocked(api.workspaces.list)
		.mockRejectedValueOnce(new Error('First failure'))
		.mockReturnValueOnce(retry.promise);
	render(ConsolePage);
	await fireEvent.click(await screen.findByRole('button', { name: 'Retry' }));
	expect(screen.getByRole('button', { name: 'Retry' })).toBeDisabled();

	retry.reject(new Error('Retry failure'));
	const alert = await screen.findByRole('alert');
	expect(within(alert).getByRole('button', { name: 'Retry' })).toBeEnabled();
	expect(alert).not.toHaveTextContent('Retry failure');
	expect(screen.queryByRole('status')).not.toBeInTheDocument();
	expectNoWorkspaceSections();
});

it('returns keyboard focus to Retry when a retry fails (BUG-4342)', async () => {
	const retry = deferredWorkspaces();
	vi.mocked(api.workspaces.list)
		.mockRejectedValueOnce(new Error('First failure'))
		.mockReturnValueOnce(retry.promise);
	render(ConsolePage);
	const first = await screen.findByRole('button', { name: 'Retry' });
	first.focus();
	await fireEvent.click(first);

	retry.reject(new Error('Retry failure'));
	const alert = await screen.findByRole('alert');
	expect(document.activeElement).toBe(within(alert).getByRole('button', { name: 'Retry' }));
});

it('does not take focus on the first failure, only after a retry (BUG-4342)', async () => {
	vi.mocked(api.workspaces.list).mockRejectedValueOnce(new Error('First failure'));
	render(ConsolePage);
	const alert = await screen.findByRole('alert');
	expect(document.activeElement).not.toBe(within(alert).getByRole('button', { name: 'Retry' }));
});

it('shows the error and Retry when the rejection has an empty message (BUG-4334)', async () => {
	vi.mocked(api.workspaces.list).mockRejectedValueOnce(new Error(''));
	render(ConsolePage);
	const alert = await screen.findByRole('alert');
	expect(within(alert).getByRole('button', { name: 'Retry' })).toBeEnabled();
	expectNoWorkspaceSections();
});

it('announces a failed retry from a fresh alert node, not a role added to the retrying one (BUG-4334)', async () => {
	const retry = deferredWorkspaces();
	vi.mocked(api.workspaces.list)
		.mockRejectedValueOnce(new Error('First failure'))
		.mockReturnValueOnce(retry.promise);
	render(ConsolePage);
	await fireEvent.click(await screen.findByRole('button', { name: 'Retry' }));
	const retrying = screen.getByRole('status').closest('.error-msg');
	expect(retrying).not.toBeNull();

	retry.reject(new Error('Retry failure'));
	const alert = await screen.findByRole('alert');
	expect(alert).not.toBe(retrying);
});
