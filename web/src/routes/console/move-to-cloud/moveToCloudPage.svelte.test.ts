// TASK-3612: the move-to-Cloud guide links to Pad Cloud's sign-up only from a
// self-hosted instance in a browser. Inside a mobile app's web view the whole
// guide is replaced by one line (the app stores' anti-steering rules,
// TASK-3299), and on Cloud there is nothing to sign up for.
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
		expect(screen.queryByText('Open Pad in a browser to move a workspace.')).toBeNull();
		expect(container.textContent).toContain('pad workspace link <slug> --url https://app.getpad.dev');
		expect(container.textContent).toContain('https://mcp.getpad.dev');
	});

	// Lead ruling (day 92): the whole guide steers to Pad Cloud, so inside a
	// mobile app the route shows one line and nothing else.
	it('shows only the open-in-a-browser note inside a mobile app', () => {
		auth.authStore.nativeShell = true;
		const { container } = render(Page);
		expect(screen.getByText('Open Pad in a browser to move a workspace.')).toBeTruthy();
		expect(container.querySelectorAll('a')).toHaveLength(0);
		expect(container.querySelectorAll('h1, h2, ol, pre')).toHaveLength(0);
		expect(container.textContent).not.toMatch(/Cloud|getpad\.dev/);
	});

	it('shows only the note inside a mobile app on Cloud as well', () => {
		auth.authStore.nativeShell = true;
		auth.authStore.cloudMode = true;
		auth.authStore.mcpPublicUrl = 'https://mcp.example.test';
		const { container } = render(Page);
		expect(screen.getByText('Open Pad in a browser to move a workspace.')).toBeTruthy();
		expect(container.textContent).not.toMatch(/Cloud|getpad\.dev|mcp\.example\.test/);
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
