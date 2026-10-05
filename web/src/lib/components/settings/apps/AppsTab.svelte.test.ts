import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, fireEvent, waitFor } from '@testing-library/svelte';
import type { AppInstallList, AppInstallPreview, AppInstallStateResult } from '$lib/types';

// SPEC-6 U9a (TASK-3413): Settings → Apps. The list, the apps-off state, the
// install review and handoff, and the lifecycle doors with their copy.

class FakeApiError extends Error {
	code: string;
	details?: Record<string, unknown>;
	constructor(code: string, message = code, details?: Record<string, unknown>) {
		super(message);
		this.code = code;
		this.details = details;
	}
}

const listMock = vi.fn<(ws: string) => Promise<AppInstallList>>();
const previewMock = vi.fn<(ws: string, url: string) => Promise<AppInstallPreview>>();
const confirmMock = vi.fn();
const discardMock = vi.fn();
const lifecycleMock = vi.fn<(ws: string, id: string, action: string) => Promise<AppInstallStateResult>>();
const issueCodeMock = vi.fn();

vi.mock('$lib/api/client', () => ({
	PadApiError: FakeApiError,
	api: {
		apps: {
			list: (ws: string) => listMock(ws),
			preview: (ws: string, url: string) => previewMock(ws, url),
			confirm: (...a: unknown[]) => confirmMock(...a),
			discardPending: (...a: unknown[]) => discardMock(...a),
			lifecycle: (ws: string, id: string, action: string) => lifecycleMock(ws, id, action),
			issueCode: (...a: unknown[]) => issueCodeMock(...a)
		}
	}
}));

vi.mock('$lib/stores/auth.svelte', () => ({ authStore: { identityEpoch: 1 } }));

const { default: AppsTab } = await import('./AppsTab.svelte');

const portal = {
	install_id: 'inst-1',
	app_name: 'Support Portal',
	origin: 'https://portal.example',
	version: '1.2.0',
	state: 'active' as const,
	created_at: '2026-10-01T10:00:00Z',
	updated_at: '2026-10-01T10:00:00Z',
	webhook: { url: 'https://portal.example/hooks', status: 'active', undelivered_dropped: 3 }
};

function previewFixture(): AppInstallPreview {
	return {
		pending_id: 'pend-1',
		expires_at: '2026-10-05T12:00:00Z',
		origin: 'https://portal.example',
		manifest_url: 'https://portal.example/.well-known/pad-app.json',
		manifest_sha256: 'sha-reviewed',
		app_id: 'portal',
		version: '1.2.0',
		title: 'Support Portal',
		publisher: 'Portal Co',
		reviewed_by_pad: false,
		notice: 'Not reviewed by Pad. You are installing code published at this origin.',
		service_access: 'write',
		delegated_access: 'write',
		reads_system_collections: "The app can read this workspace's Conventions and Playbooks.",
		collections: [{ key: 'tickets', slug: 'tickets', name: 'Tickets', schema: {}, adopt: false }],
		events: [{ name: 'item.created', collections: ['tickets'] }],
		webhook_url: 'https://portal.example/hooks',
		item_actions: [],
		artifacts: [
			{
				key: 'triage',
				url: 'https://portal.example/triage.md',
				kind: 'playbook',
				destination_collection: 'playbooks',
				raw_sha256: 'r',
				raw: 'RAW BODY',
				normalized: { title: 'Triage', content: 'STORED BODY', fields: { status: 'draft' } },
				normalized_sha256: 'n',
				changes: ['field "trigger" was changed: not an option']
			}
		],
		redirect_uris: []
	};
}

beforeEach(() => {
	vi.clearAllMocks();
});

describe('Settings → Apps', () => {
	it('says apps are off and to ask the admin, instead of an empty list', async () => {
		listMock.mockResolvedValue({ available: false, cloud: false, installs: [] });
		render(AppsTab, { wsSlug: 'ws-a' });
		const off = await screen.findByTestId('apps-unavailable');
		expect(off.textContent).toMatch(/Ask your admin/);
		expect(screen.queryByRole('button', { name: 'Install an app' })).toBeNull();
	});

	it('lists live installs and keeps uninstalled ones apart', async () => {
		listMock.mockResolvedValue({
			available: true,
			cloud: false,
			installs: [portal, { ...portal, install_id: 'inst-0', app_name: 'Old Bot', state: 'uninstalled', webhook: undefined }]
		});
		render(AppsTab, { wsSlug: 'ws-a' });
		expect(await screen.findByRole('button', { name: /Support Portal/ })).toBeTruthy();
		expect(screen.getByText('3 dropped')).toBeTruthy();
		expect(screen.queryByRole('button', { name: /Old Bot/ })).toBeNull();
		expect(screen.getByText('Uninstalled (1)')).toBeTruthy();
	});

	it('shows the review: not-reviewed notice, read statement, changes, raw and stored; installs the reviewed hash and hands off the code', async () => {
		listMock.mockResolvedValue({ available: true, cloud: false, installs: [] });
		previewMock.mockResolvedValue(previewFixture());
		confirmMock.mockResolvedValue({
			install_id: 'inst-1',
			install_code: 'CODE-123',
			expires_at: '2026-10-05T12:10:00Z',
			notice: 'Give this install code to the app.',
			items: []
		});
		render(AppsTab, { wsSlug: 'ws-a' });
		await fireEvent.click(await screen.findByRole('button', { name: 'Install an app' }));
		await fireEvent.input(screen.getByLabelText('App URL'), { target: { value: 'https://portal.example' } });
		await fireEvent.click(screen.getByRole('button', { name: 'Review' }));

		expect((await screen.findByTestId('app-not-reviewed')).textContent).toMatch(/Not reviewed by Pad/);
		expect(screen.getByTestId('app-reads-system').textContent).toMatch(/Conventions and Playbooks/);
		expect(screen.getByText(/"trigger" was changed/)).toBeTruthy();
		expect(screen.getByText('RAW BODY')).toBeTruthy();
		expect(screen.getByText('STORED BODY')).toBeTruthy();
		expect(previewMock).toHaveBeenCalledWith('ws-a', 'https://portal.example');

		await fireEvent.click(screen.getByRole('button', { name: 'Install Support Portal' }));
		expect(confirmMock).toHaveBeenCalledWith('ws-a', 'pend-1', 'sha-reviewed');
		expect((await screen.findByTestId('app-install-code')).textContent).toBe('CODE-123');
	});

	it('a refused preview names the manifest path', async () => {
		listMock.mockResolvedValue({ available: true, cloud: false, installs: [] });
		previewMock.mockRejectedValue(new FakeApiError('invalid_manifest', 'bad slug', { path: 'companion_pack.collections[0].slug' }));
		render(AppsTab, { wsSlug: 'ws-a' });
		await fireEvent.click(await screen.findByRole('button', { name: 'Install an app' }));
		await fireEvent.input(screen.getByLabelText('App URL'), { target: { value: 'https://x.example' } });
		await fireEvent.click(screen.getByRole('button', { name: 'Review' }));
		const alert = await screen.findByRole('alert');
		expect(alert.textContent).toMatch(/bad slug/);
		expect(alert.textContent).toMatch(/companion_pack\.collections\[0\]\.slug/);
	});

	it('cancelling a review discards the staged install', async () => {
		listMock.mockResolvedValue({ available: true, cloud: false, installs: [] });
		previewMock.mockResolvedValue(previewFixture());
		discardMock.mockResolvedValue(undefined);
		render(AppsTab, { wsSlug: 'ws-a' });
		await fireEvent.click(await screen.findByRole('button', { name: 'Install an app' }));
		await fireEvent.input(screen.getByLabelText('App URL'), { target: { value: 'https://portal.example' } });
		await fireEvent.click(screen.getByRole('button', { name: 'Review' }));
		await screen.findByTestId('app-not-reviewed');
		await fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
		expect(discardMock).toHaveBeenCalledWith('ws-a', 'pend-1');
	});

	it('the install view shows dropped deliveries, and disable states its consequence before acting', async () => {
		listMock.mockResolvedValue({ available: true, cloud: false, installs: [portal] });
		lifecycleMock.mockResolvedValue({ install_id: 'inst-1', state: 'inactive' });
		render(AppsTab, { wsSlug: 'ws-a' });
		await fireEvent.click(await screen.findByRole('button', { name: /Support Portal/ }));
		expect(screen.getByTestId('app-undelivered-dropped').textContent).toMatch(/3\s+deliveries dropped undelivered after 24 hours/);

		await fireEvent.click(screen.getByRole('button', { name: 'Disable' }));
		expect(lifecycleMock).not.toHaveBeenCalled();
		expect(screen.getByRole('group', { name: 'Disable this app' }).textContent).toMatch(/signed out/);
		await fireEvent.click(screen.getByRole('button', { name: 'Disable' }));
		expect(lifecycleMock).toHaveBeenCalledWith('ws-a', 'inst-1', 'disable');
		await waitFor(() => expect(listMock).toHaveBeenCalledTimes(2));
	});

	it('rotate hands over the new code; deliveries in flight say to repeat', async () => {
		listMock.mockResolvedValue({ available: true, cloud: false, installs: [portal] });
		lifecycleMock.mockRejectedValueOnce(new FakeApiError('deliveries_in_flight'));
		lifecycleMock.mockResolvedValueOnce({
			install_id: 'inst-1',
			state: 'active',
			install_code: 'NEW-CODE',
			expires_at: '2026-10-05T12:10:00Z',
			notice: 'Give this install code to the app.'
		});
		render(AppsTab, { wsSlug: 'ws-a' });
		await fireEvent.click(await screen.findByRole('button', { name: /Support Portal/ }));
		await fireEvent.click(screen.getByRole('button', { name: 'Rotate' }));
		await fireEvent.click(screen.getByRole('button', { name: 'Rotate' }));
		expect((await screen.findByRole('alert')).textContent).toMatch(/press the button again/);
		await fireEvent.click(screen.getByRole('button', { name: 'Rotate' }));
		expect((await screen.findByTestId('app-install-code')).textContent).toBe('NEW-CODE');
		expect(lifecycleMock).toHaveBeenCalledTimes(2);
	});

	it('an inactive install offers re-enable and uninstall only', async () => {
		listMock.mockResolvedValue({ available: true, cloud: false, installs: [{ ...portal, state: 'inactive' }] });
		render(AppsTab, { wsSlug: 'ws-a' });
		await fireEvent.click(await screen.findByRole('button', { name: /Support Portal/ }));
		expect(screen.getByRole('button', { name: 'Re-enable' })).toBeTruthy();
		expect(screen.getByRole('button', { name: 'Uninstall' })).toBeTruthy();
		expect(screen.queryByRole('button', { name: 'Disable' })).toBeNull();
		expect(screen.queryByRole('button', { name: 'Issue code' })).toBeNull();
	});
});
