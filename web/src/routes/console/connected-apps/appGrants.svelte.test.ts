import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { flushSync, mount, unmount, tick } from 'svelte';

/**
 * TASK-3399 (SPEC-6 U5b-1): the Connected apps page lists the delegated grants
 * a person gave installed apps on their own, read-only plus revoke (lead
 * ruling Q1). The API client is stubbed.
 */
const list = vi.hoisted(() => vi.fn());
const revoke = vi.hoisted(() => vi.fn());

vi.mock('$lib/api/client', () => ({
	api: {
		connectedApps: { list: () => list(), revoke: (id: string) => revoke(id) },
		workspaces: { list: vi.fn(async () => []) }
	}
}));

const { default: Page } = await import('./+page.svelte');

const grant = {
	id: 'req-grant-1',
	app_name: 'Support Portal',
	origin: 'https://portal.example',
	workspace: { slug: 'acme', name: 'Acme' },
	access: 'write',
	granted_at: new Date().toISOString()
};

let host: HTMLElement;
let app: Record<string, unknown> | null = null;

async function settle() {
	flushSync();
	for (let i = 0; i < 4; i++) await Promise.resolve();
	await tick();
	flushSync();
}

async function mountWith(body: unknown) {
	list.mockResolvedValue(body);
	app = mount(Page, { target: host, props: {} }) as Record<string, unknown>;
	await settle();
}

beforeEach(() => {
	host = document.createElement('div');
	document.body.appendChild(host);
	list.mockReset();
	revoke.mockReset();
});

afterEach(() => {
	if (app) unmount(app);
	app = null;
	host.remove();
});

describe('Connected apps: installed apps acting as you', () => {
	it('lists an app grant on its own, with no MCP empty state and no edit controls', async () => {
		await mountWith({ items: [], app_grants: [grant] });
		const text = host.textContent ?? '';
		expect(text).toContain('Installed apps acting as you');
		expect(text).toContain('Support Portal');
		expect(text).toContain('Acme');
		expect(text).toContain('Read and write');
		expect(text).not.toContain('No connected apps yet.');
		const section = host.querySelector('[aria-labelledby="app-grants-title"]') ?? host;
		const buttons = [...section.querySelectorAll('button')].map((b) => b.textContent?.trim());
		expect(buttons).toEqual(['Revoke']);
	});

	it('shows the empty state only when there is nothing at all', async () => {
		await mountWith({ items: [], app_grants: [] });
		expect(host.textContent).toContain('No connected apps yet.');
		expect(host.textContent).not.toContain('Installed apps acting as you');
	});

	it('revokes a grant through the shared confirm and reloads', async () => {
		await mountWith({ items: [], app_grants: [{ ...grant, access: 'read' }] });
		expect(host.textContent).toContain('Read only');
		const btn = [...host.querySelectorAll('button')].find((b) => b.textContent?.trim() === 'Revoke');
		btn!.click();
		await settle();
		expect(document.body.textContent).toContain('Revoke Support Portal?');
		revoke.mockResolvedValue(undefined);
		list.mockResolvedValue({ items: [], app_grants: [] });
		const confirm = [...document.body.querySelectorAll('.modal-actions button')].find(
			(b) => b.textContent?.trim() === 'Revoke'
		) as HTMLButtonElement;
		confirm.click();
		await settle();
		expect(revoke).toHaveBeenCalledWith('req-grant-1');
		expect(list).toHaveBeenCalledTimes(2);
	});
});
