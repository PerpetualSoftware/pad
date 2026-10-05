import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, fireEvent } from '@testing-library/svelte';
import { normalizePreview } from './appsShape';
import type { AppInstallPreview } from '$lib/types';

// BUG-3417: a Go server serializes an omitted slice as null. A real minimal
// manifest's preview came back with item_actions, artifacts, events and
// collections null, and the review threw on `.length`, hanging on
// "Fetching…". These feed the reviews exactly that shape.

class FakeApiError extends Error {
	code: string;
	constructor(code: string) {
		super(code);
		this.code = code;
	}
}

const listMock = vi.fn();
const previewMock = vi.fn();
const confirmMock = vi.fn();
const getMock = vi.fn();
const upgradePreviewMock = vi.fn();
const upgradeConfirmMock = vi.fn();

vi.mock('$lib/api/client', () => ({
	PadApiError: FakeApiError,
	api: {
		apps: {
			list: (...a: unknown[]) => listMock(...a),
			preview: (...a: unknown[]) => previewMock(...a),
			confirm: (...a: unknown[]) => confirmMock(...a),
			discardPending: vi.fn(),
			lifecycle: vi.fn(),
			issueCode: vi.fn(),
			get: (...a: unknown[]) => getMock(...a),
			upgradePreview: (...a: unknown[]) => upgradePreviewMock(...a),
			upgradeConfirm: (...a: unknown[]) => upgradeConfirmMock(...a)
		},
		items: { update: vi.fn() }
	}
}));
vi.mock('$lib/stores/auth.svelte', () => ({ authStore: { identityEpoch: 1 } }));
vi.mock('$app/state', () => ({ page: { params: { username: 'dave', workspace: 'ws-a' } } }));

const { default: AppsTab } = await import('./AppsTab.svelte');

/** The shape a minimal manifest's preview had on the wire: lists null. */
function nullPreview(): AppInstallPreview {
	return {
		pending_id: 'pend-null',
		expires_at: '2026-10-05T12:00:00Z',
		origin: 'https://minimal.example',
		manifest_url: 'https://minimal.example/.well-known/pad-app.json',
		manifest_sha256: 'sha-null',
		app_id: 'minimal',
		version: '1.0.0',
		title: 'Minimal App',
		publisher: 'Someone',
		reviewed_by_pad: false,
		notice: 'Not reviewed by Pad. You are installing code published at this origin.',
		service_access: 'read',
		delegated_access: 'none',
		reads_system_collections: "The app can read this workspace's Conventions and Playbooks.",
		collections: null,
		events: null,
		item_actions: null,
		artifacts: null,
		redirect_uris: null
	} as unknown as AppInstallPreview;
}

const install = {
	install_id: 'inst-1',
	app_name: 'Minimal App',
	origin: 'https://minimal.example',
	version: '1.0.0',
	state: 'active' as const,
	created_at: '2026-10-01T10:00:00Z',
	updated_at: '2026-10-01T10:00:00Z'
};

beforeEach(() => {
	vi.clearAllMocks();
	getMock.mockResolvedValue({ install_id: 'inst-1', state: 'active', artifacts: null });
});

describe('BUG-3417: null lists from the server', () => {
	it('normalizePreview makes every list an array, nested ones too', () => {
		const p = normalizePreview({
			...nullPreview(),
			events: [{ name: 'item.created', collections: null }],
			item_actions: [{ key: 'k', label: 'L', path: '/x', collections: null }],
			artifacts: [{ key: 'a', changes: null, normalized: { title: 't', content: 'c', fields: null } }],
			upgrade: { install_id: 'i', from_version: '0.9', from_manifest_sha256: 'o', diff: null, review_required: false, notice: '' }
		} as unknown as AppInstallPreview);
		expect(p.collections).toEqual([]);
		expect(p.redirect_uris).toEqual([]);
		expect(p.events[0].collections).toEqual([]);
		expect(p.item_actions[0].collections).toEqual([]);
		expect(p.artifacts[0].changes).toEqual([]);
		expect(p.artifacts[0].normalized.fields).toEqual({});
		expect(p.upgrade!.diff).toEqual([]);
	});

	it('the install review renders a null-list preview and installs it', async () => {
		listMock.mockResolvedValue({ available: true, cloud: false, installs: null });
		previewMock.mockResolvedValue(nullPreview());
		confirmMock.mockResolvedValue({ install_id: 'inst-1', install_code: 'CODE', expires_at: '2026-10-05T12:10:00Z', notice: 'n', items: null });
		render(AppsTab, { wsSlug: 'ws-a' });
		// A null installs list is an empty one, not a crash.
		expect(await screen.findByText('No apps installed.')).toBeTruthy();
		await fireEvent.click(screen.getByRole('button', { name: 'Install an app' }));
		await fireEvent.input(screen.getByLabelText('App URL'), { target: { value: 'https://minimal.example' } });
		await fireEvent.click(screen.getByRole('button', { name: 'Review' }));
		expect((await screen.findByTestId('app-not-reviewed')).textContent).toMatch(/Not reviewed by Pad/);
		expect(screen.getByText('Playbooks and conventions it adds (0)')).toBeTruthy();
		await fireEvent.click(screen.getByRole('button', { name: 'Install Minimal App' }));
		expect(confirmMock).toHaveBeenCalledWith('ws-a', 'pend-null', 'sha-null');
		expect((await screen.findByTestId('app-install-code')).textContent).toBe('CODE');
	});

	it('the upgrade review renders a null diff as nothing to change, and a null-artifact install view', async () => {
		listMock.mockResolvedValue({ available: true, cloud: false, installs: [install] });
		upgradePreviewMock.mockResolvedValue({
			...nullPreview(),
			upgrade: { install_id: 'inst-1', from_version: '1.0.0', from_manifest_sha256: 'old', diff: null, review_required: false, notice: '' }
		});
		render(AppsTab, { wsSlug: 'ws-a' });
		await fireEvent.click(await screen.findByRole('button', { name: /Minimal App/ }));
		expect(await screen.findByText('This app added none.')).toBeTruthy();
		await fireEvent.click(screen.getByRole('button', { name: 'Check for an update' }));
		expect(await screen.findByTestId('app-upgrade-nothing')).toBeTruthy();
	});

	it('an upgrade whose result lists no items says so', async () => {
		listMock.mockResolvedValue({ available: true, cloud: false, installs: [install] });
		upgradePreviewMock.mockResolvedValue({
			...nullPreview(),
			version: '2.0.0',
			upgrade: {
				install_id: 'inst-1',
				from_version: '1.0.0',
				from_manifest_sha256: 'old',
				diff: [{ kind: 'event', key: 'x', change: 'removed', class: 'auto' }],
				review_required: false,
				notice: ''
			}
		});
		upgradeConfirmMock.mockResolvedValue({ install_id: 'inst-1', version: '2.0.0', items: null });
		render(AppsTab, { wsSlug: 'ws-a' });
		await fireEvent.click(await screen.findByRole('button', { name: /Minimal App/ }));
		await fireEvent.click(screen.getByRole('button', { name: 'Check for an update' }));
		await fireEvent.click(await screen.findByRole('button', { name: 'Upgrade to v2.0.0' }));
		expect(await screen.findByText('No new drafts.')).toBeTruthy();
	});
});
