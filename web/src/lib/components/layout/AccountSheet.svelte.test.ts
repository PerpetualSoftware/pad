// Phone account actions and their visibility gates.
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, cleanup } from '@testing-library/svelte';
import { tick, flushSync } from 'svelte';

const h = vi.hoisted(() => ({ chatOn: true, path: '/', commerce: false, role: 'member', oauth: false }));
vi.mock('$app/navigation', () => ({ goto: vi.fn(), afterNavigate: vi.fn() }));
vi.mock('$app/state', () => ({
	page: {
		get url() {
			return new URL(`http://localhost${h.path}`);
		},
		state: {}
	}
}));
vi.mock('$lib/api/client', () => ({ api: { auth: { logout: vi.fn() } } }));
vi.mock('$lib/stores/workspace.svelte', () => ({ workspaceStore: { workspaces: [], current: { slug: 'ws', owner_username: 'u', name: 'WS' } } }));
// The real store (other modules register identity listeners on it), with the
// fields these components read pinned.
vi.mock('$lib/stores/auth.svelte', async (orig) => {
	const real = await orig<typeof import('$lib/stores/auth.svelte')>();
	const fake = {
		authenticated: true,
		user: { name: 'Ada', email: 'ada@example.com', get role() { return h.role; } },
		get commerceAllowed() { return h.commerce; },
		get oauthAvailable() { return h.oauth; },
		cloudMode: false,
		mcpPublicUrl: '',
		mcpAuth: ''
	};
	return {
		...real,
		authStore: new Proxy(real.authStore, {
			get: (t, k) => (k in fake ? fake[k as keyof typeof fake] : Reflect.get(t, k))
		})
	};
});

vi.mock('$lib/stores/chatSetting.svelte', () => ({
	chatSetting: {
		get enabled() {
			return h.chatOn;
		}
	}
}));

vi.mock('$lib/stores/ui.svelte', () => ({ uiStore: { isMobile: true, closeSearch: vi.fn() } }));
import MobileContextBar from './MobileContextBar.svelte';
import AccountSheet from './AccountSheet.svelte';
import { chatStore } from '$lib/stores/chat.svelte';

beforeEach(() => {
	cleanup();
	h.chatOn = true;
	h.path = '/'; h.commerce = false; h.role = 'member'; h.oauth = false;
	chatStore.unreadTotal = 4;
});
afterEach(() => {
	cleanup();
	chatStore.unreadTotal = 0;
});


describe('Account sheet', () => {
 it('shows profile, theme, Settings shortcuts, Connect, Resources and Sign out without moved destinations', async () => {
  const { getByText, getByRole, queryByRole } = render(AccountSheet, { open: true, onclose: vi.fn() });
  await tick();
  expect(getByText('Ada')).toBeTruthy(); expect(getByText('ada@example.com')).toBeTruthy();
  expect(getByRole('button', { name: 'Dark mode' })).toBeTruthy();
  expect(getByRole('link', { name: 'Settings' }).getAttribute('href')).toBe('/settings');
  expect(getByRole('link', { name: 'Account' }).getAttribute('href')).toBe('/settings/account');
  expect(getByRole('link', { name: 'Deleted workspaces' })).toBeTruthy();
  expect(getByRole('button', { name: 'Connect a project…' })).toBeTruthy();
  expect(getByText('Resources')).toBeTruthy();
  expect(getByRole('button', { name: 'Sign out' })).toBeTruthy();
  for (const name of ['Agenda', 'Chat', 'Workspaces', 'Billing', 'Admin', 'Connected apps'])
   expect(queryByRole('link', { name, exact: true })).toBeNull();
 });
 it('gates Billing, Admin and Connected apps like Settings', async () => {
  h.commerce = true; h.role = 'admin'; h.oauth = true;
  const { getByRole } = render(AccountSheet, { open: true, onclose: vi.fn() }); await tick();
  for (const [name, path] of [['Billing','billing'], ['Admin','admin'], ['Connected apps','connected-apps']])
   expect(getByRole('link', { name }).getAttribute('href')).toBe('/settings/' + path);
 });
 it('closes on a settings shortcut and toggles the persisted theme', async () => {
  const onclose = vi.fn();
  const { getByRole } = render(AccountSheet, { open: true, onclose }); await tick();
  const theme = document.documentElement.getAttribute('data-theme');
  getByRole('button', { name: 'Dark mode' }).click(); flushSync();
  expect(document.documentElement.getAttribute('data-theme')).not.toBe(theme);
  expect(localStorage.getItem('pad-theme')).toBe(document.documentElement.getAttribute('data-theme'));
  const a = getByRole('link', { name: 'Account' }); a.addEventListener('click', e => e.preventDefault()); a.click();
  expect(onclose).toHaveBeenCalledOnce();
 });
});

if (!('animate' in Element.prototype)) {
	(Element.prototype as unknown as { animate: unknown }).animate = function () {
		const animation = {
			onfinish: null as null | (() => void),
			cancel() {},
			finish() {},
			currentTime: 0,
			addEventListener() {},
			finished: Promise.resolve()
		};
		queueMicrotask(() => animation.onfinish?.());
		return animation;
	};
}

it('the workspace top-bar avatar opens Account', async () => {
 h.path = '/u/ws/tasks';
 const { getByRole } = render(MobileContextBar); await tick();
 getByRole('button', { name: 'Account', exact: true }).click(); await tick(); flushSync();
 expect(getByRole('dialog', { name: 'Account' })).toBeTruthy();
 expect(getByRole('link', { name: 'Settings' }).getAttribute('href')).toBe('/settings');
});
