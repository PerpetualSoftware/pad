import { afterEach, expect, it, vi } from 'vitest';
import { cleanup, render, screen } from '@testing-library/svelte';
import { createRawSnippet } from 'svelte';

const h = vi.hoisted(() => ({ user: { role: 'admin' }, stats: { cloud_mode: true, users: 2, workspaces: 3, users_by_plan: {} }, loadStats: vi.fn(), url: new URL('https://pad.test/settings/admin/instance') }));
vi.mock('$lib/stores/auth.svelte', () => ({ authStore: h }));
vi.mock('$lib/stores/admin.svelte', () => ({ adminStore: h }));
vi.mock('$app/state', () => ({ page: h }));
import Admin from './+layout.svelte';
const children = createRawSnippet(() => ({ render: () => '<div>Admin content</div>' }));
afterEach(cleanup);
const items = [['Users', ''], ['Invitations', '/invitations'], ['Audit log', '/audit-log'], ['MCP audit', '/mcp-audit'], ['Billing', '/billing'], ['Instance', '/instance']];
it.each(items)('renders six segments and marks %s', (name, suffix) => {
 h.stats.cloud_mode = true; h.url = new URL('https://pad.test/settings/admin' + suffix);
 render(Admin, { children });
 expect(screen.getAllByRole('link')).toHaveLength(6);
 for (const [label, route] of items) expect(screen.getByRole('link', { name: label })).toHaveAttribute('href', '/settings/admin' + route);
 expect(screen.getByRole('link', { name })).toHaveAttribute('aria-current', 'page');
 expect(document.querySelectorAll('[aria-current="page"]')).toHaveLength(1);
});
it('keeps Billing hidden on self-host', () => {
 h.stats.cloud_mode = false;
 render(Admin, { children });
 expect(screen.queryByRole('link', { name: 'Billing' })).toBeNull();
});
