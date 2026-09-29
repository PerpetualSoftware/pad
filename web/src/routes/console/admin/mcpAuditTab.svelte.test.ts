import { describe, it, expect, vi, afterEach } from 'vitest';
import { createRawSnippet, flushSync, mount, unmount } from 'svelte';

/**
 * PLAN-2310 DR-7 (TASK-3303): the admin MCP Audit tab is always visible to an
 * admin, MCP on or off, because its API answers on every install and reading
 * past use while MCP is off is how an admin audits it. It used to hide off
 * cloud. A non-admin gets no admin tabs at all.
 */
const auth = vi.hoisted(() => ({ role: 'admin' }));
vi.mock('$app/state', () => ({ page: { url: new URL('http://localhost/console/admin') } }));
vi.mock('$lib/stores/auth.svelte', () => ({
	authStore: {
		get user() {
			return { role: auth.role };
		}
	}
}));
vi.mock('$lib/stores/admin.svelte', () => ({
	adminStore: {
		loadStats: vi.fn(),
		loading: false,
		error: '',
		// A self-hosted install with MCP off.
		stats: { users: 1, users_by_plan: {}, workspaces: 1, cloud_mode: false, mcp_available: false, oauth_available: false }
	}
}));

const { default: AdminLayout } = await import('./+layout.svelte');

let app: Record<string, unknown> | null = null;
let host: HTMLElement;
function render() {
	host = document.createElement('div');
	document.body.appendChild(host);
	const children = createRawSnippet(() => ({ render: () => '<div></div>' }));
	app = mount(AdminLayout, { target: host, props: { children } }) as Record<string, unknown>;
	flushSync();
}
const tabLabels = () => [...host.querySelectorAll('a')].map((a) => a.textContent?.trim());

afterEach(() => {
	if (app) unmount(app as never);
	app = null;
	host.remove();
});

describe('admin MCP Audit tab', () => {
	it('is shown to an admin on a self-hosted install with MCP off', () => {
		auth.role = 'admin';
		render();
		expect(tabLabels()).toContain('MCP Audit');
		expect(host.querySelector('a[href="/console/admin/mcp-audit"]')).not.toBeNull();
	});

	it('is not shown to a non-admin', () => {
		auth.role = 'member';
		render();
		expect(tabLabels()).not.toContain('MCP Audit');
	});
});
