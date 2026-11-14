import { it, expect, vi, afterEach } from 'vitest';
import { render, cleanup, screen } from '@testing-library/svelte';
import { createRawSnippet } from 'svelte';
vi.mock('$lib/stores/auth.svelte', () => ({ authStore: { load: async () => ({ authenticated: true }), onIdentityChange: () => () => {} } }));
vi.mock('$lib/stores/breakpoint.svelte', () => ({ viewport: { isMobile: false } }));
import Layout from './+layout.svelte';
afterEach(cleanup);
it('renders its content without a second nav or main on desktop', async () => {
	render(Layout, { children: createRawSnippet(() => ({ render: () => '<p>Account content</p>' })) });
	expect(await screen.findByText('Account content')).toBeTruthy();
	expect(document.querySelector('.console-nav')).toBeNull();
	expect(document.querySelector('main')).toBeNull();
});
