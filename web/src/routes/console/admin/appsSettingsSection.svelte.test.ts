import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { flushSync, mount, unmount, tick } from 'svelte';

/**
 * TASK-3980 item 1: the admin Settings page's Apps section, the switch the
 * Apps tab's off-state points at. It renders each state GET
 * /api/v1/admin/apps can report and writes only `enabled` on Save. These
 * assert the rendered page (CONVE-19), with the admin store and API client
 * stubbed.
 */
const getApps = vi.hoisted(() => vi.fn());
const putApps = vi.hoisted(() => vi.fn());

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
			getMCPSettings: vi.fn(() => new Promise(() => {})),
			getAppsSettings: () => getApps(),
			updateAppsSettings: (enabled: boolean) => putApps(enabled)
		}
	}
}));

const { default: SettingsPage } = await import('./settings/+page.svelte');

const settings = (over: Record<string, unknown> = {}) => ({
	enabled: true,
	available: true,
	https_issuer: true,
	cloud: false,
	private_origins: [],
	...over
});

let host: HTMLElement;
let app: Record<string, unknown> | null = null;

async function settle() {
	for (let i = 0; i < 4; i++) await Promise.resolve();
	await tick();
	flushSync();
}
async function mountWith(body: unknown) {
	getApps.mockResolvedValue(body);
	app = mount(SettingsPage, { target: host, props: {} }) as Record<string, unknown>;
	flushSync();
	await settle();
}
const q = (id: string) => host.querySelector(`[data-testid="${id}"]`) as HTMLElement | null;

beforeEach(() => {
	host = document.createElement('div');
	document.body.appendChild(host);
	getApps.mockReset();
	putApps.mockReset();
});
afterEach(() => {
	if (app) unmount(app as never);
	app = null;
	host.remove();
});

describe('admin Apps section', () => {
	it('on and available: toggle live, state on', async () => {
		await mountWith(settings());
		expect((q('apps-enabled') as HTMLInputElement).checked).toBe(true);
		expect((q('apps-enabled') as HTMLInputElement).disabled).toBe(false);
		expect(q('apps-state')?.dataset.state).toBe('on');
	});

	it('enabled without an https issuer: blocked, and says why', async () => {
		await mountWith(settings({ available: false, https_issuer: false }));
		expect(q('apps-state')?.dataset.state).toBe('blocked');
		expect(q('apps-state')?.textContent).toContain('https');
	});

	it('off', async () => {
		await mountWith(settings({ enabled: false, available: false }));
		expect((q('apps-enabled') as HTMLInputElement).checked).toBe(false);
		expect(q('apps-state')?.dataset.state).toBe('off');
	});

	it('Pad Cloud: toggle disabled, no Save', async () => {
		await mountWith(settings({ cloud: true }));
		expect((q('apps-enabled') as HTMLInputElement).disabled).toBe(true);
		expect(q('apps-save')).toBeNull();
		expect(q('apps-section')?.textContent).toContain('managed by the operator');
	});

	it('the checkbox edits a draft; only Save writes, then renders the answer', async () => {
		await mountWith(settings({ enabled: false, available: false }));
		putApps.mockResolvedValue(settings());
		const save = q('apps-save') as HTMLButtonElement;
		expect(save.disabled, 'nothing to save before a change').toBe(true);
		const box = q('apps-enabled') as HTMLInputElement;
		box.checked = true;
		box.dispatchEvent(new Event('change', { bubbles: true }));
		flushSync();
		expect(putApps, 'a click on the checkbox alone does not write').not.toHaveBeenCalled();
		expect(save.disabled).toBe(false);
		save.click();
		await settle();
		expect(putApps).toHaveBeenCalledWith(true);
		expect(q('apps-state')?.dataset.state).toBe('on');
	});
});
