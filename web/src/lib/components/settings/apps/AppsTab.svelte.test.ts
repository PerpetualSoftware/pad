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
const getMock = vi.fn();
const upgradePreviewMock = vi.fn();
const upgradeConfirmMock = vi.fn();
const itemUpdateMock = vi.fn();

vi.mock('$lib/api/client', () => ({
	PadApiError: FakeApiError,
	api: {
		apps: {
			list: (ws: string) => listMock(ws),
			preview: (ws: string, url: string) => previewMock(ws, url),
			confirm: (...a: unknown[]) => confirmMock(...a),
			discardPending: (...a: unknown[]) => discardMock(...a),
			lifecycle: (ws: string, id: string, action: string) => lifecycleMock(ws, id, action),
			issueCode: (...a: unknown[]) => issueCodeMock(...a),
			get: (...a: unknown[]) => getMock(...a),
			upgradePreview: (...a: unknown[]) => upgradePreviewMock(...a),
			upgradeConfirm: (...a: unknown[]) => upgradeConfirmMock(...a)
		},
		items: { update: (...a: unknown[]) => itemUpdateMock(...a) }
	}
}));

vi.mock('$app/state', () => ({ page: { params: { username: 'dave', workspace: 'ws-a' } } }));

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
	getMock.mockResolvedValue({ install_id: 'inst-1', state: 'active', artifacts: [] });
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

describe('Settings → Apps focus (codex r2)', () => {
	it('after install, focus lands on the code panel heading', async () => {
		listMock.mockResolvedValue({ available: true, cloud: false, installs: [] });
		previewMock.mockResolvedValue(previewFixture());
		confirmMock.mockResolvedValue({ install_id: 'inst-1', install_code: 'C', expires_at: '2026-10-05T12:10:00Z', notice: 'n', items: [] });
		render(AppsTab, { wsSlug: 'ws-a' });
		await fireEvent.click(await screen.findByRole('button', { name: 'Install an app' }));
		expect(document.activeElement).toBe(screen.getByLabelText('App URL'));
		await fireEvent.input(screen.getByLabelText('App URL'), { target: { value: 'https://portal.example' } });
		await fireEvent.click(screen.getByRole('button', { name: 'Review' }));
		await fireEvent.click(await screen.findByRole('button', { name: 'Install Support Portal' }));
		await waitFor(() => expect(document.activeElement?.textContent).toBe('Give this code to Support Portal'));
	});

	it('a completed action focuses the install heading; cancel returns to its button; back returns to the card', async () => {
		listMock.mockResolvedValue({ available: true, cloud: false, installs: [portal] });
		lifecycleMock.mockResolvedValue({ install_id: 'inst-1', state: 'active' });
		render(AppsTab, { wsSlug: 'ws-a' });
		await fireEvent.click(await screen.findByRole('button', { name: /Support Portal/ }));
		const heading = screen.getByRole('heading', { name: 'Support Portal' });
		expect(document.activeElement).toBe(heading);

		await fireEvent.click(screen.getByRole('button', { name: 'Uninstall' }));
		await fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
		await waitFor(() => expect(document.activeElement).toBe(screen.getByRole('button', { name: 'Uninstall' })));

		await fireEvent.click(screen.getByRole('button', { name: 'Disable' }));
		await fireEvent.click(screen.getByRole('button', { name: 'Disable' }));
		await waitFor(() => expect(document.activeElement).toBe(screen.getByRole('heading', { name: 'Support Portal' })));

		await fireEvent.click(screen.getByRole('button', { name: /All apps/ }));
		await waitFor(() => expect(document.activeElement).toBe(screen.getByRole('button', { name: /Support Portal/ })));
	});
});

it('an install_state refusal closes the card and focuses the install heading', async () => {
	listMock.mockResolvedValue({ available: true, cloud: false, installs: [portal] });
	lifecycleMock.mockRejectedValue(new FakeApiError('install_state'));
	render(AppsTab, { wsSlug: 'ws-a' });
	await fireEvent.click(await screen.findByRole('button', { name: /Support Portal/ }));
	await fireEvent.click(screen.getByRole('button', { name: 'Disable' }));
	// A real click focuses the button; fireEvent.click does not, and focus
	// would otherwise still sit on the heading from mount.
	const confirmBtn = screen.getByRole('button', { name: 'Disable' });
	confirmBtn.focus();
	expect(document.activeElement).toBe(confirmBtn);
	await fireEvent.click(confirmBtn);
	expect((await screen.findByRole('alert')).textContent).toMatch(/changed state/);
	await waitFor(() => expect(document.activeElement).toBe(screen.getByRole('heading', { name: 'Support Portal' })));
	expect(screen.queryByRole('group', { name: 'Disable this app' })).toBeNull();
});

describe('Settings → Apps: upgrade and drafts (U9b)', () => {
	function upgradePreview(diff: unknown[]): AppInstallPreview {
		return {
			...previewFixture(),
			pending_id: 'pend-up',
			manifest_sha256: 'sha-up',
			version: '2.0.0',
			upgrade: { install_id: 'inst-1', from_version: '1.2.0', from_manifest_sha256: 'old', diff: diff as never, review_required: true, notice: 'Review what changes.' }
		};
	}

	async function openInstall() {
		listMock.mockResolvedValue({ available: true, cloud: false, installs: [portal] });
		render(AppsTab, { wsSlug: 'ws-a' });
		await fireEvent.click(await screen.findByRole('button', { name: /Support Portal/ }));
	}

	it('groups the diff, names released collections, shows new drafts, and upgrades with the reviewed hash', async () => {
		upgradePreviewMock.mockResolvedValue(
			upgradePreview([
				{ kind: 'artifact', key: 'triage', change: 'changed', class: 'review' },
				{ kind: 'access', key: 'service', change: 'widened', class: 'review' },
				{ kind: 'collection', key: 'old-tickets', change: 'released', class: 'auto' },
				{ kind: 'event', key: 'item.deleted', change: 'removed', class: 'auto' }
			])
		);
		upgradeConfirmMock.mockResolvedValue({ install_id: 'inst-1', version: '2.0.0', items: [{ key: 'triage', ref: 'PLAYB-9', status: 'draft' }] });
		await openInstall();
		await fireEvent.click(screen.getByRole('button', { name: 'Check for an update' }));

		const review = await screen.findByTestId('app-upgrade-review');
		expect(review.textContent).toMatch(/Playbook or convention “triage” changed/);
		expect(review.textContent).toMatch(/widened: more access/);
		const auto = screen.getByTestId('app-upgrade-auto');
		expect(auto.textContent).toMatch(/“old-tickets” released to the workspace \(data kept\)/);
		// Each line sits in exactly one group.
		expect(review.querySelectorAll('li')).toHaveLength(2);
		expect(review.textContent).not.toMatch(/old-tickets|item\.deleted/);
		expect(auto.querySelectorAll('li')).toHaveLength(2);
		expect(auto.textContent).not.toMatch(/triage|widened/);
		expect(screen.getByText('arrives as a new draft')).toBeTruthy();

		await fireEvent.click(screen.getByRole('button', { name: 'Upgrade to v2.0.0' }));
		expect(upgradeConfirmMock).toHaveBeenCalledWith('ws-a', 'inst-1', 'pend-up', 'sha-up');
		expect(await screen.findByText('PLAYB-9')).toBeTruthy();
		// The drafts panel re-reads after the upgrade.
		await waitFor(() => expect(getMock).toHaveBeenCalledTimes(2));
	});

	it('a stale review says so and previews again on request', async () => {
		upgradePreviewMock.mockResolvedValue(upgradePreview([{ kind: 'event', key: 'x', change: 'added', class: 'review' }]));
		upgradeConfirmMock.mockRejectedValue(new FakeApiError('install_review_stale', 'The workspace changed since you reviewed this upgrade'));
		await openInstall();
		await fireEvent.click(screen.getByRole('button', { name: 'Check for an update' }));
		await fireEvent.click(await screen.findByRole('button', { name: 'Upgrade to v2.0.0' }));
		expect((await screen.findByRole('alert')).textContent).toMatch(/changed since you reviewed/);
		expect(screen.queryByRole('button', { name: 'Upgrade to v2.0.0' })).toBeNull();
		await fireEvent.click(screen.getByRole('button', { name: 'Preview again' }));
		expect(upgradePreviewMock).toHaveBeenCalledTimes(2);
	});

	it('nothing to change offers no upgrade, and closing discards the staged one', async () => {
		upgradePreviewMock.mockResolvedValue(upgradePreview([]));
		discardMock.mockResolvedValue(undefined);
		await openInstall();
		await fireEvent.click(screen.getByRole('button', { name: 'Check for an update' }));
		expect(await screen.findByTestId('app-upgrade-nothing')).toBeTruthy();
		expect(screen.queryByRole('button', { name: /Upgrade to/ })).toBeNull();
		await fireEvent.click(screen.getByRole('button', { name: 'Close' }));
		expect(discardMock).toHaveBeenCalledWith('ws-a', 'pend-up');
		expect(screen.getByRole('button', { name: 'Check for an update' })).toBeTruthy();
	});

	it('no upgrade while an install is between phases', async () => {
		listMock.mockResolvedValue({ available: true, cloud: false, installs: [{ ...portal, state: 'disabling' }] });
		render(AppsTab, { wsSlug: 'ws-a' });
		await fireEvent.click(await screen.findByRole('button', { name: /Support Portal/ }));
		expect(screen.queryByRole('button', { name: 'Check for an update' })).toBeNull();
	});

	it('lists the app’s drafts with links, and Activate sets status active on that item only', async () => {
		getMock.mockResolvedValue({
			install_id: 'inst-1',
			state: 'active',
			artifacts: [
				{ item_id: 'i1', ref: 'PLAYB-3', slug: 'triage', title: 'Triage', collection_slug: 'playbooks', status: 'draft', version: '1.2.0' },
				{ item_id: 'i2', ref: 'CONVE-4', slug: 'tone', title: 'Tone', collection_slug: 'conventions', status: 'active', version: '1.2.0' }
			]
		});
		itemUpdateMock.mockResolvedValue({});
		await openInstall();
		const rows = await screen.findAllByTestId('app-artifact-row');
		expect(rows).toHaveLength(2);
		expect(screen.getByRole('link', { name: 'Triage' }).getAttribute('href')).toBe('/dave/ws-a/playbooks/PLAYB-3');
		expect(screen.queryByRole('button', { name: 'Activate Tone' })).toBeNull();
		await fireEvent.click(screen.getByRole('button', { name: 'Activate Triage' }));
		expect(itemUpdateMock).toHaveBeenCalledWith('ws-a', 'i1', { fields_patch: { status: 'active' } });
		await waitFor(() => expect(getMock).toHaveBeenCalledTimes(2));
	});
});
