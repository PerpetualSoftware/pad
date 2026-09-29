// PLAN-2310 DR-8: the connect modal serves both MCP auth paths. The session's
// mcp_auth decides what is visible: ['oauth', 'pat'] keeps the sign-in path
// and offers a personal API token as an alternative; ['pat'] (an http
// self-host) makes the token the only path and hides the claim-code tab and
// the Connected apps link, which need OAuth; an empty URL leaves only the
// CLI. Each case asserts what is visible AND what is not, so a branch that
// fires for both (or neither) fails.
import { afterEach, describe, expect, it, vi } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';

vi.mock('$lib/api/client', () => ({
	api: { workspaces: { claimCode: vi.fn(() => new Promise(() => {})) } },
	PadApiError: class extends Error {}
}));

const { default: ConnectWorkspaceModal } = await import('./ConnectWorkspaceModal.svelte');

let root: HTMLElement | null = null;
let instance: ReturnType<typeof mount> | null = null;

afterEach(() => {
	if (instance) unmount(instance);
	root?.remove();
	root = null;
	instance = null;
});

function render(mcpPublicUrl: string, mcpAuth: string[] | undefined) {
	root = document.body.appendChild(document.createElement('div'));
	instance = mount(ConnectWorkspaceModal, {
		target: root,
		props: { open: true, workspaceSlug: 'acme', serverUrl: 'http://pad.lan:7777', mcpPublicUrl, mcpAuth }
	});
	flushSync();
}

function tabLabels(): string[] {
	return [...document.querySelectorAll('.primary-tab .primary-tab-label')].map((e) => e.textContent ?? '');
}

function text(): string {
	return document.body.textContent ?? '';
}

function patConfig(): string {
	return document.querySelector('[data-testid="connect-pat-config"]')?.textContent ?? '';
}

function clickClient(label: string) {
	const btn = [...document.querySelectorAll<HTMLButtonElement>('button.tab-btn')].find(
		(b) => b.textContent?.trim() === label
	);
	if (!btn) throw new Error(`no client tab ${label}`);
	btn.click();
	flushSync();
}

const HTTPS_URL = 'https://pad.example.com/mcp';
const HTTP_URL = 'http://pad.lan:7777/mcp';

describe('ConnectWorkspaceModal auth paths', () => {
	it("['oauth','pat']: sign-in path, claim tab, and the token path as an alternative", () => {
		render(HTTPS_URL, ['oauth', 'pat']);
		expect(tabLabels()).toEqual(['Connect an agent', 'CLI', 'Connect code']);
		expect(text()).toContain(HTTPS_URL);
		expect(text()).toMatch(/Sign in with your Pad\s+account when prompted/);
		const details = document.querySelector('details.pat-details');
		expect(details).not.toBeNull();
		expect(details?.querySelector('summary')?.textContent).toMatch(/personal API token/);
		// Unnumbered inside the disclosure: it does not continue the OAuth steps.
		expect(details?.textContent).not.toMatch(/Step \d/);
		expect(patConfig()).toContain('Authorization: Bearer YOUR_PAD_TOKEN');
		expect(document.querySelector('a[href="/console/connected-apps"]')).not.toBeNull();
	});

	it("['pat']: the token path is the only one; no claim tab, no sign-in copy, no Connected apps", () => {
		render(HTTP_URL, ['pat']);
		expect(tabLabels()).toEqual(['Connect an agent', 'CLI']);
		expect(text()).toContain(HTTP_URL);
		expect(text()).not.toMatch(/Sign in with your Pad/);
		expect(text()).not.toMatch(/Connect code/);
		expect(document.querySelector('details.pat-details')).toBeNull();
		expect(text()).toMatch(/Step 2 — Create a personal API token/);
		expect(text()).toMatch(/Step 3 — Add Pad to your client/);
		const link = document.querySelector<HTMLAnchorElement>('[data-testid="connect-pat-settings-link"]');
		expect(link?.getAttribute('href')).toBe('/console/settings#api-tokens');
		expect(document.querySelector('a[href="/console/connected-apps"]')).toBeNull();
		// ChatGPT's connectors are OAuth-only, so it is not offered here.
		expect([...document.querySelectorAll('button.tab-btn')].map((b) => b.textContent?.trim())).not.toContain(
			'ChatGPT'
		);
	});

	it('mcp_public_url empty: only the CLI tab, and it is active', () => {
		render('', []);
		expect(tabLabels()).toEqual(['CLI']);
		expect(document.querySelector('.primary-tab.active')?.textContent).toMatch(/CLI/);
		expect(patConfig()).toBe('');
		expect(document.querySelector('a[href="/console/connected-apps"]')).toBeNull();
	});

	it('a server that predates mcp_auth: the OAuth rendering it always had, no token path', () => {
		render(HTTPS_URL, undefined);
		expect(tabLabels()).toEqual(['Connect an agent', 'CLI', 'Connect code']);
		expect(text()).toMatch(/Sign in with your Pad/);
		expect(document.querySelector('details.pat-details')).toBeNull();
		expect(patConfig()).toBe('');
	});

	it('every client config carries the URL and the bearer header, and the JSON ones parse', () => {
		render(HTTP_URL, ['pat']);
		expect(patConfig()).toBe(
			`claude mcp add --transport http pad ${HTTP_URL} --header "Authorization: Bearer YOUR_PAD_TOKEN"`
		);

		clickClient('Cursor');
		expect(JSON.parse(patConfig())).toEqual({
			mcpServers: { pad: { url: HTTP_URL, headers: { Authorization: 'Bearer YOUR_PAD_TOKEN' } } }
		});

		clickClient('Windsurf');
		expect(JSON.parse(patConfig())).toEqual({
			mcpServers: { pad: { serverUrl: HTTP_URL, headers: { Authorization: 'Bearer YOUR_PAD_TOKEN' } } }
		});

		clickClient('VS Code');
		expect(JSON.parse(patConfig())).toEqual({
			servers: { pad: { type: 'http', url: HTTP_URL, headers: { Authorization: 'Bearer YOUR_PAD_TOKEN' } } }
		});

		clickClient('Claude Desktop');
		const desktop = JSON.parse(patConfig());
		expect(desktop.mcpServers.pad.args).toEqual([
			'mcp-remote',
			HTTP_URL,
			'--header',
			'Authorization:${PAD_AUTH}',
			'--allow-http'
		]);
		expect(desktop.mcpServers.pad.env).toEqual({ PAD_AUTH: 'Bearer YOUR_PAD_TOKEN' });
	});

	it('Claude Desktop over https needs no --allow-http', () => {
		render(HTTPS_URL, ['oauth', 'pat']);
		clickClient('Claude Desktop');
		expect(JSON.parse(patConfig()).mcpServers.pad.args).not.toContain('--allow-http');
	});
});
