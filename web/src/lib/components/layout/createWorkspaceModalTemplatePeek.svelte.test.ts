import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, cleanup, fireEvent } from '@testing-library/svelte';
import { tick } from 'svelte';

/**
 * TASK-2254 (audit C30): looking at the template list chose nothing. It used
 * to pre-select `startup` on first expand, so a user who browsed and collapsed
 * the list created a startup workspace while the modal showed no selection.
 */

const api = vi.hoisted(() => ({
	auth: { session: vi.fn() },
	templates: { list: vi.fn() },
	workspaces: { create: vi.fn(), importBundle: vi.fn(), list: vi.fn(), get: vi.fn(), me: vi.fn() },
}));
vi.mock('$lib/api/client', () => ({ api, isPlanLimitError: () => false, planLimitMessage: () => '' }));
vi.mock('$app/navigation', () => ({ goto: vi.fn() }));
vi.mock('$lib/stores/toast.svelte', () => ({
	toastStore: { show: vi.fn(), dismiss: vi.fn(), get toasts() { return []; } },
}));

import CreateWorkspaceModal from './CreateWorkspaceModal.svelte';
import { authStore } from '$lib/stores/auth.svelte';
import { uiStore } from '$lib/stores/ui.svelte';

const TEMPLATES = [
	{ name: 'startup', category: 'software', icon: '🚀', collections: ['Tasks', 'Plans'] },
	{ name: 'scrum', category: 'software', icon: '🏃', collections: ['Backlog'] },
];

async function settle() {
	for (let i = 0; i < 24; i++) {
		await Promise.resolve();
		await tick();
	}
}

function btn(container: HTMLElement, label: RegExp): HTMLButtonElement {
	const found = [...container.querySelectorAll('button')].find((b) => label.test(b.textContent?.trim() ?? ''));
	if (!found) throw new Error(`no button matching ${label}`);
	return found as HTMLButtonElement;
}

async function createNamed(container: HTMLElement) {
	const name = container.querySelector('#ws-create-name') as HTMLInputElement;
	await fireEvent.input(name, { target: { value: 'Peek test' } });
	btn(container, /Create Workspace/).click();
	await settle();
	expect(api.workspaces.create).toHaveBeenCalledTimes(1);
	return JSON.stringify(api.workspaces.create.mock.calls[0]);
}

beforeEach(async () => {
	api.templates.list.mockResolvedValue(TEMPLATES);
	api.workspaces.list.mockResolvedValue([]);
	api.workspaces.me.mockResolvedValue(null);
	api.workspaces.create.mockReset();
	api.workspaces.create.mockResolvedValue({ id: 'w1', slug: 'peek', name: 'Peek test', owner_username: 'u' });
	api.workspaces.get.mockResolvedValue({ id: 'w1', slug: 'peek', name: 'Peek test', owner_username: 'u' });
	authStore.clear();
	api.auth.session.mockResolvedValue({ authenticated: true, user: { id: 'u1', email: 'u1@example.com' } });
	await authStore.load();
	uiStore.openCreateWorkspace();
});

afterEach(() => {
	cleanup();
	uiStore.closeCreateWorkspace();
	authStore.clear();
});

describe('template peek (TASK-2254)', () => {
	it('expanding and collapsing the list leaves Start blank chosen, and Create sends blank', async () => {
		const { container } = render(CreateWorkspaceModal, { props: {} });
		await settle();
		btn(container, /Or pick a template/).click();
		await settle();
		expect(container.querySelector('.template-card.selected'), 'nothing pre-selected').toBeNull();
		btn(container, /Or pick a template/).click();
		await settle();
		expect(btn(container, /Start blank/).getAttribute('aria-pressed')).toBe('true');
		const sent = await createNamed(container);
		expect(sent).toContain('"template":"blank"');
		expect(sent).not.toContain('startup');
	});

	it('a picked template stays named with the list collapsed, and × returns to blank', async () => {
		const { container } = render(CreateWorkspaceModal, { props: {} });
		await settle();
		btn(container, /Or pick a template/).click();
		await settle();
		btn(container, /^🏃\s*scrum/).click();
		await settle();
		btn(container, /Or pick a template|Template:/).click(); // collapse
		await settle();
		expect(btn(container, /Template: scrum/)).toBeTruthy();
		const clear = container.querySelector('button[aria-label="Clear template, start blank"]') as HTMLButtonElement;
		expect(clear).not.toBeNull();
		clear.click();
		await settle();
		expect(btn(container, /Start blank/).getAttribute('aria-pressed')).toBe('true');
		expect(container.querySelector('button[aria-label="Clear template, start blank"]')).toBeNull();
		expect(btn(container, /Or pick a template/)).toBeTruthy();
	});

	it('a picked template is what Create sends', async () => {
		const { container } = render(CreateWorkspaceModal, { props: {} });
		await settle();
		btn(container, /Or pick a template/).click();
		await settle();
		btn(container, /^🏃\s*scrum/).click();
		await settle();
		expect(await createNamed(container)).toContain('"template":"scrum"');
	});
});
