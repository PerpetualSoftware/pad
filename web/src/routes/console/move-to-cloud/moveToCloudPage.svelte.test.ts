// TASK-3612: the move-to-Cloud guide links to Pad Cloud's sign-up only from a
// self-hosted instance in a browser. Inside a mobile app's web view the link
// is absent (the app stores' anti-steering rules, TASK-3299), and on Cloud
// there is nothing to sign up for.
import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen } from '@testing-library/svelte';

const auth = vi.hoisted(() => ({
	authStore: { cloudMode: false, nativeShell: false, mcpPublicUrl: '' }
}));
vi.mock('$lib/stores/auth.svelte', () => auth);

const Page = (await import('./+page.svelte')).default;

const SIGNUP = 'https://app.getpad.dev';

function signupLinks(container: HTMLElement): Element[] {
	return [...container.querySelectorAll('a')].filter((a) => a.getAttribute('href') === SIGNUP);
}

describe('move-to-Cloud guide', () => {
	beforeEach(() => {
		auth.authStore.cloudMode = false;
		auth.authStore.nativeShell = false;
		auth.authStore.mcpPublicUrl = '';
	});

	it('links to the Cloud sign-up from a self-hosted instance in a browser (control)', () => {
		const { container } = render(Page);
		expect(signupLinks(container)).toHaveLength(1);
		expect(container.textContent).toContain('https://mcp.getpad.dev');
	});

	it('has no sign-up link inside a mobile app', () => {
		auth.authStore.nativeShell = true;
		const { container } = render(Page);
		// The step itself still renders, so the absence is not an empty page.
		expect(screen.getByRole('heading', { name: 'Sign in to Pad Cloud' })).toBeTruthy();
		expect(signupLinks(container)).toHaveLength(0);
	});

	it('has no sign-up link on Cloud, and names the session MCP address', () => {
		auth.authStore.cloudMode = true;
		auth.authStore.mcpPublicUrl = 'https://mcp.example.test';
		const { container } = render(Page);
		expect(screen.getByText("You're on Pad Cloud already.")).toBeTruthy();
		expect(signupLinks(container)).toHaveLength(0);
		expect(container.textContent).toContain('https://mcp.example.test');
	});
});
