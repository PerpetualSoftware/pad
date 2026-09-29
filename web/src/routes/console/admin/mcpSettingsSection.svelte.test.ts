import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { flushSync, mount, unmount, tick } from 'svelte';

/**
 * PLAN-2310 DR-7 (TASK-3303): the admin Settings page's MCP section renders
 * each state GET /api/v1/admin/mcp can report. These assert the rendered page
 * (CONVE-19), with the admin store and API client stubbed.
 */
const getMCP = vi.hoisted(() => vi.fn());
const putMCP = vi.hoisted(() => vi.fn());

vi.mock('$lib/stores/admin.svelte', () => ({
	adminFetch: vi.fn(async () => null),
	adminPatch: vi.fn(),
	adminPost: vi.fn(),
	getCSRFToken: () => '',
	adminStore: { stats: null }
}));
vi.mock('$lib/api/client', () => ({
	api: {
		admin: {
			getDecisionSettings: vi.fn(() => new Promise(() => {})),
			getMCPSettings: () => getMCP(),
			updateMCPSettings: (enabled: boolean) => putMCP(enabled)
		}
	}
}));

const { default: SettingsPage } = await import('./settings/+page.svelte');

const https = {
	origin: 'https://pad.example.com',
	origin_var: 'PAD_URL',
	mcp_url: 'https://pad.example.com/mcp',
	auth_server_url: 'https://pad.example.com',
	https: true,
	auth_methods: ['oauth', 'pat'],
	problems: [] as string[],
	state: 'on',
	resume: { oauth_connections: 0, pats: 0 }
};
const settings = (over: Record<string, unknown>, rd: Record<string, unknown> = {}) => ({
	enabled: true,
	source: 'setting',
	locked: false,
	...over,
	readiness: { ...https, ...rd }
});

let host: HTMLElement;
let app: Record<string, unknown> | null = null;

async function mountWith(body: unknown) {
	getMCP.mockResolvedValue(body);
	app = mount(SettingsPage, { target: host, props: {} }) as Record<string, unknown>;
	flushSync();
	for (let i = 0; i < 4; i++) await Promise.resolve();
	await tick();
	flushSync();
}
const q = (id: string) => host.querySelector(`[data-testid="${id}"]`) as HTMLElement | null;
const text = (id: string) => q(id)?.textContent?.replace(/\s+/g, ' ').trim() ?? '';

beforeEach(() => {
	host = document.createElement('div');
	document.body.appendChild(host);
	getMCP.mockReset();
	putMCP.mockReset();
});
afterEach(() => {
	if (app) unmount(app as never);
	app = null;
	host.remove();
});

describe('admin MCP section', () => {
	it('on, https: OAuth and API tokens, toggle live', async () => {
		await mountWith(settings({}));
		expect((q('mcp-enabled') as HTMLInputElement).checked).toBe(true);
		expect((q('mcp-enabled') as HTMLInputElement).disabled).toBe(false);
		expect(q('mcp-state')?.dataset.state).toBe('on');
		expect(text('mcp-auth')).toBe('OAuth and API tokens');
		expect(text('mcp-readout')).toContain('https://pad.example.com/mcp');
		expect(q('mcp-lock-note')).toBeNull();
	});

	it('on, http: API tokens only', async () => {
		await mountWith(settings({}, { origin: 'http://10.0.0.5:7777', https: false, auth_methods: ['pat'] }));
		expect(text('mcp-auth')).toBe('API tokens only');
	});

	it('off with resume counts says nothing was revoked', async () => {
		await mountWith(settings({ enabled: false }, { state: 'off', resume: { oauth_connections: 3, pats: 1 } }));
		expect(q('mcp-state')?.dataset.state).toBe('off');
		expect(text('mcp-resume')).toContain('3 OAuth connections and 1 API token');
		expect(text('mcp-resume')).toContain('revoked nothing');
	});

	it('off with nothing to resume shows no resume line', async () => {
		await mountWith(settings({ enabled: false }, { state: 'off' }));
		expect(q('mcp-resume')).toBeNull();
	});

	it('blocked, no origin: says why, origin not set', async () => {
		const why = 'No public origin is configured. Set PAD_URL (or PUBLIC_URL) to the URL this server is reached at.';
		await mountWith(settings({}, { origin: '', origin_var: undefined, mcp_url: '', auth_server_url: '', https: false, auth_methods: [], state: 'blocked', blocked: why }));
		expect(q('mcp-state')?.dataset.state).toBe('blocked');
		expect(text('mcp-state')).toContain(why);
		expect(text('mcp-readout')).toContain('not set');
		expect(text('mcp-auth')).toBe('None');
	});

	it('blocked, unusable origin: the reason once, the origin "not usable"', async () => {
		const why = 'PAD_URL "pad.example.com/app" is not usable as the public origin: the scheme must be http or https';
		await mountWith(settings({}, { origin: '', mcp_url: '', auth_server_url: '', https: false, auth_methods: [], problems: [why], state: 'blocked', blocked: why }));
		expect(text('mcp-state')).toContain(why);
		expect(text('mcp-readout')).toContain('not usable');
		expect(host.querySelectorAll('[data-testid="mcp-problem"]').length).toBe(0);
	});

	it('locked by the environment: toggle disabled with its note', async () => {
		await mountWith(settings({ source: 'environment', locked: true }));
		expect((q('mcp-enabled') as HTMLInputElement).disabled).toBe(true);
		expect(text('mcp-lock-note')).toContain('set by environment');
	});

	it('Pad Cloud: toggle disabled, managed by the operator', async () => {
		await mountWith(settings({ source: 'cloud', locked: true }));
		expect((q('mcp-enabled') as HTMLInputElement).disabled).toBe(true);
		expect(text('mcp-lock-note')).toContain('managed by the operator');
	});

	it('the toggle writes at once and renders the answer', async () => {
		await mountWith(settings({ enabled: false }, { state: 'off' }));
		putMCP.mockResolvedValue(settings({}));
		const box = q('mcp-enabled') as HTMLInputElement;
		box.checked = true;
		box.dispatchEvent(new Event('change', { bubbles: true }));
		for (let i = 0; i < 4; i++) await Promise.resolve();
		await tick();
		flushSync();
		expect(putMCP).toHaveBeenCalledWith(true);
		expect(q('mcp-state')?.dataset.state).toBe('on');
	});
});
