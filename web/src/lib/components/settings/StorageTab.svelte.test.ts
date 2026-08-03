import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';
import type {
	AttachmentListItem,
	AttachmentListResponse,
	WorkspaceStorageInfo,
} from '$lib/types';

// TASK-2418. The Storage tab lives at `/{user}/{ws}/settings` — ONE SvelteKit
// route — so switching workspaces changes `wsSlug` under a MOUNTED component.
// These tests drive that switch and pin the reactive reload: fresh rows, fresh
// usage, no stale scope, and a loading state that can't strand when the switch
// happens mid-flight.

const listMock =
	vi.fn<(ws: string, filters: Record<string, unknown>) => Promise<AttachmentListResponse>>();
const usageMock = vi.fn<(ws: string) => Promise<WorkspaceStorageInfo>>();
const toastMock = vi.fn<(message: string, kind?: string) => void>();

class FakeApiError extends Error {
	code: string;
	constructor(code: string) {
		super(code);
		this.code = code;
	}
}

vi.mock('$lib/api/client', () => ({
	PadApiError: FakeApiError,
	api: {
		attachments: {
			list: (ws: string, filters: Record<string, unknown>) => listMock(ws, filters),
			storageUsage: (ws: string) => usageMock(ws),
			downloadUrl: (ws: string, id: string, variant?: string) =>
				`/api/v1/workspaces/${ws}/attachments/${id}${variant ? `?variant=${variant}` : ''}`,
			delete: vi.fn(),
		},
	},
}));

vi.mock('$app/state', () => ({
	page: { params: { username: 'dave', workspace: 'ws-a' }, url: new URL('http://x/') },
}));

vi.mock('$lib/attachments/events', () => ({
	announceAttachmentDeleted: vi.fn(),
}));

vi.mock('$lib/components/editor/attachment-metadata', () => ({
	invalidateAttachmentMetadata: vi.fn(),
}));

vi.mock('$lib/stores/toast.svelte', () => ({
	toastStore: { show: (message: string, kind?: string) => toastMock(message, kind) },
}));

const { default: StorageTab } = await import('./StorageTab.svelte');

function att(overrides: Partial<AttachmentListItem> & { id: string }): AttachmentListItem {
	return {
		workspace_id: 'ws-1',
		uploaded_by: 'u-1',
		storage_key: `key/${overrides.id}`,
		content_hash: `hash-${overrides.id}`,
		mime_type: 'application/pdf',
		size_bytes: 2048,
		filename: `${overrides.id}.pdf`,
		created_at: '2026-08-01T00:00:00Z',
		...overrides,
	};
}

function response(attachments: AttachmentListItem[]): AttachmentListResponse {
	return { attachments, total: attachments.length, limit: 50, offset: 0 };
}

function usage(used: number): WorkspaceStorageInfo {
	return { used_bytes: used, limit_bytes: 1000, override_active: false } as WorkspaceStorageInfo;
}

/** A promise plus its resolver, so a test can control when a fetch lands. */
function deferred<T>() {
	let resolve!: (value: T) => void;
	let reject!: (err: unknown) => void;
	const promise = new Promise<T>((res, rej) => {
		resolve = res;
		reject = rej;
	});
	return { promise, resolve, reject };
}

// Reactive props object so a test can flip `wsSlug` the way the settings route
// does when the user switches workspaces while this tab is open.
const props = $state<{ wsSlug: string; collections: []; initialItemId: string }>({
	wsSlug: 'ws-a',
	collections: [],
	initialItemId: '',
});

describe('StorageTab workspace switching', () => {
	let target: HTMLElement;
	let instance: ReturnType<typeof mount> | undefined;

	beforeEach(() => {
		listMock.mockReset();
		listMock.mockResolvedValue(response([]));
		usageMock.mockReset();
		usageMock.mockResolvedValue(usage(1));
		toastMock.mockReset();
		props.wsSlug = 'ws-a';
		props.initialItemId = '';
		target = document.body.appendChild(document.createElement('div'));
	});

	afterEach(() => {
		if (instance) unmount(instance);
		instance = undefined;
		target.remove();
	});

	function mountTab() {
		instance = mount(StorageTab, { target, props });
		flushSync();
	}

	async function settle() {
		for (let i = 0; i < 6; i++) await Promise.resolve();
		flushSync();
	}

	function rows(): HTMLElement[] {
		return Array.from(target.querySelectorAll<HTMLElement>('.att-row'));
	}

	function text(): string {
		return target.textContent ?? '';
	}

	it('loads exactly once on mount', async () => {
		mountTab();
		await settle();

		expect(listMock).toHaveBeenCalledTimes(1);
		expect(listMock).toHaveBeenCalledWith('ws-a', expect.anything());
		expect(usageMock).toHaveBeenCalledTimes(1);
		expect(usageMock).toHaveBeenCalledWith('ws-a');
	});

	it('reloads the list and the usage figure when the workspace changes', async () => {
		listMock.mockResolvedValueOnce(response([att({ id: 'a1', filename: 'from-a.pdf' })]));
		usageMock.mockResolvedValueOnce(usage(111));
		mountTab();
		await settle();
		expect(rows()).toHaveLength(1);
		expect(text()).toContain('from-a.pdf');
		expect(text()).toContain('111 B');

		listMock.mockResolvedValueOnce(response([att({ id: 'b1', filename: 'from-b.pdf' })]));
		usageMock.mockResolvedValueOnce(usage(222));
		props.wsSlug = 'ws-b';
		flushSync();
		await settle();

		expect(listMock).toHaveBeenCalledTimes(2);
		expect(listMock).toHaveBeenLastCalledWith('ws-b', expect.anything());
		expect(usageMock).toHaveBeenLastCalledWith('ws-b');
		expect(rows()).toHaveLength(1);
		expect(text()).toContain('from-b.pdf');
		expect(text()).not.toContain('from-a.pdf');
		expect(text()).toContain('222 B');
	});

	it('drops the old workspace item scope when the workspace changes', async () => {
		props.initialItemId = 'item-in-a';
		mountTab();
		await settle();
		expect(listMock).toHaveBeenLastCalledWith(
			'ws-a',
			expect.objectContaining({ item_id: 'item-in-a' })
		);
		expect(target.querySelector('.item-scope')).not.toBeNull();

		// A workspace switch without a deep link: the scope named an item in the
		// workspace the user just left, so it must not narrow the new list.
		props.wsSlug = 'ws-b';
		props.initialItemId = '';
		flushSync();
		await settle();

		expect(listMock).toHaveBeenCalledTimes(2);
		expect(listMock).toHaveBeenLastCalledWith('ws-b', expect.not.objectContaining({ item_id: 'item-in-a' }));
		expect(target.querySelector('.item-scope')).toBeNull();
	});

	it('does not strand the loading state when the switch happens mid-flight', async () => {
		const slowA = deferred<AttachmentListResponse>();
		listMock.mockReturnValueOnce(slowA.promise);
		mountTab();
		await settle();
		expect(text()).toContain('Loading');

		listMock.mockResolvedValueOnce(response([att({ id: 'b1', filename: 'from-b.pdf' })]));
		props.wsSlug = 'ws-b';
		flushSync();
		await settle();

		// B landed; the tab is showing B even though A is still outstanding.
		expect(text()).toContain('from-b.pdf');
		expect(text()).not.toContain('Loading attachments');

		// A's superseded response resolves late and must change nothing — in
		// particular it must not re-raise the loading gate or paint its rows.
		slowA.resolve(response([att({ id: 'a1', filename: 'from-a.pdf' })]));
		await settle();
		expect(text()).toContain('from-b.pdf');
		expect(text()).not.toContain('from-a.pdf');
		expect(text()).not.toContain('Loading attachments');
	});

	it('ignores the first load of an A→B→A round trip, where the workspace matches again', async () => {
		// The workspace fence can't see this one: by the time A's FIRST response
		// lands, `wsSlug` is back at 'ws-a'. Only the generation distinguishes it
		// from the load that is actually current (Codex round 1).
		const staleA = deferred<AttachmentListResponse>();
		const staleUsageA = deferred<WorkspaceStorageInfo>();
		listMock.mockReturnValueOnce(staleA.promise);
		usageMock.mockReturnValueOnce(staleUsageA.promise);
		mountTab();
		await settle();

		props.wsSlug = 'ws-b';
		flushSync();
		await settle();

		const freshA = deferred<AttachmentListResponse>();
		listMock.mockReturnValueOnce(freshA.promise);
		usageMock.mockResolvedValueOnce(usage(333));
		props.wsSlug = 'ws-a';
		flushSync();
		await settle();

		// The first A load resolves late. It must not lower the whole-tab gate
		// (the current A load is still pending) nor paint its rows or usage.
		staleA.resolve(response([att({ id: 'stale', filename: 'stale-a.pdf' })]));
		staleUsageA.resolve(usage(999));
		await settle();
		expect(text()).toContain('Loading storage…');
		expect(text()).not.toContain('stale-a.pdf');
		expect(text()).not.toContain('999 B');

		// The current one does.
		freshA.resolve(response([att({ id: 'fresh', filename: 'fresh-a.pdf' })]));
		await settle();
		expect(text()).not.toContain('Loading storage…');
		expect(text()).toContain('fresh-a.pdf');
		expect(text()).not.toContain('stale-a.pdf');
	});

	it('ignores a superseded usage response and its error toast', async () => {
		const slowA = deferred<WorkspaceStorageInfo>();
		usageMock.mockReturnValueOnce(slowA.promise);
		mountTab();
		await settle();

		usageMock.mockResolvedValueOnce(usage(222));
		props.wsSlug = 'ws-b';
		flushSync();
		await settle();
		expect(text()).toContain('222 B');

		slowA.reject(new Error('workspace a is gone'));
		await settle();
		expect(text()).toContain('222 B');
		expect(toastMock).not.toHaveBeenCalled();
	});
});
