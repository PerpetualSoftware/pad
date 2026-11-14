import { afterEach, expect, it, vi } from 'vitest';
import { cleanup, render, screen } from '@testing-library/svelte';
import { createRawSnippet } from 'svelte';

const h = vi.hoisted(() => ({ oauthAvailable: false, commerceAllowed: false, user: { role: 'user' }, url: new URL('https://pad.test/settings/account') }));
vi.mock('$lib/stores/auth.svelte', () => ({ authStore: h }));
vi.mock('$app/state', () => ({ page: h }));
import Settings from './+layout.svelte';
const children = createRawSnippet(() => ({ render: () => '<div>Page content</div>' }));
afterEach(cleanup);

for (const oauthAvailable of [false, true]) for (const commerceAllowed of [false, true]) for (const admin of [false, true]) {
 it(`shows tabs for OAuth=${oauthAvailable}, commerce=${commerceAllowed}, admin=${admin}`, () => {
  Object.assign(h, { oauthAvailable, commerceAllowed, user: { role: admin ? 'admin' : 'user' }, url: new URL('https://pad.test/settings/account') });
  render(Settings, { children });
  const expected = [['Account', '/settings/account'], ...(oauthAvailable ? [['Connected apps', '/settings/connected-apps']] : []), ...(commerceAllowed ? [['Billing', '/settings/billing']] : []), ['Deleted workspaces', '/settings/deleted-workspaces'], ...(admin ? [['Admin admins', '/settings/admin']] : [])];
  expect(screen.getAllByRole('link')).toHaveLength(expected.length);
  for (const [name, href] of expected) expect(screen.getByRole('link', { name })).toHaveAttribute('href', href);
  expect(screen.getByRole('link', { name: 'Account' })).toHaveAttribute('aria-current', 'page');
  expect(document.querySelectorAll('[aria-current="page"]')).toHaveLength(1);
 });
}
it.each([
 ['/settings/account', 'Account'], ['/settings/connected-apps', 'Connected apps'], ['/settings/billing', 'Billing'], ['/settings/deleted-workspaces', 'Deleted workspaces'], ['/settings/admin/instance', 'Admin admins'], ['/settings/move-to-cloud', null]
])('selects the tab for %s', (path, label) => {
 Object.assign(h, { oauthAvailable: true, commerceAllowed: true, user: { role: 'admin' }, url: new URL('https://pad.test' + path) });
 render(Settings, { children });
 const active = document.querySelector('[aria-current="page"]');
 if (label) expect(active).toBe(screen.getByRole('link', { name: label }));
 else expect(active).toBeNull();
});
