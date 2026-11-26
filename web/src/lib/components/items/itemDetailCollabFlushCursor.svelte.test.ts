/**
 * BUG-3558: the server now refuses any `?source=collab-snapshot` PATCH that
 * omits `op_log_cursor` with 409 `stale_collab_snapshot`. Every flush trigger
 * in ItemDetail — the 5s idle debounce, pagehide / visibilitychange (BUG-3030),
 * the rich-to-raw toggle, and editor teardown (unmount and item switch) — must
 * call `api.items.flushCollabContent` with a numeric `opLogCursor`.
 */
import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest';
import { cleanup, fireEvent, render, waitFor } from '@testing-library/svelte';
import { flushSync, tick } from 'svelte';
import { bindReactiveEpoch, resetEpoch } from '../../../test/identityEpochMock.svelte';

vi.mock('$lib/components/editor/Editor.svelte', () => import('../../../test/StubComponent.svelte'));
vi.mock('$lib/components/editor/EditorBubbleMenu.svelte', () => import('../../../test/StubComponent.svelte'));
vi.mock('$lib/components/editor/EditorLinkPopover.svelte', () => import('../../../test/StubComponent.svelte'));
vi.mock('$lib/components/editor/RawMarkdownEditor.svelte', () => import('../../../test/StubComponent.svelte'));
vi.mock('$lib/components/fields/FieldEditor.svelte', () => import('../../../test/StubComponent.svelte'));
vi.mock('$lib/components/fields/TagInput.svelte', () => import('../../../test/StubComponent.svelte'));
vi.mock('$lib/components/timeline/ItemTimeline.svelte', () => import('../../../test/StubComponent.svelte'));
vi.mock('$lib/components/timeline/TimelineEntryList.svelte', () => import('../../../test/StubComponent.svelte'));
vi.mock('$lib/components/ChildItems.svelte', () => import('../../../test/StubComponent.svelte'));
vi.mock('$lib/components/BacklinksPanel.svelte', () => import('../../../test/StubComponent.svelte'));
vi.mock('$lib/components/items/ItemCommitmentPanel.svelte', () => import('../../../test/StubComponent.svelte'));
vi.mock('$lib/components/items/ItemCommitmentLinks.svelte', () => import('../../../test/StubComponent.svelte'));
vi.mock('$lib/components/items/DecisionChips.svelte', () => import('../../../test/StubComponent.svelte'));
vi.mock('$lib/components/RelationBacklinksPanel.svelte', () => import('../../../test/StubComponent.svelte'));
vi.mock('./ItemPicker.svelte', () => import('../../../test/StubComponent.svelte'));
vi.mock('$lib/components/common/QuickActionsMenu.svelte', () => import('../../../test/StubComponent.svelte'));
vi.mock('$lib/components/common/BottomSheet.svelte', () => import('../../../test/StubComponent.svelte'));
vi.mock('$lib/components/collections/EditCollectionModal.svelte', () => import('../../../test/StubComponent.svelte'));
vi.mock('$lib/components/ShareDialog.svelte', () => import('../../../test/StubComponent.svelte'));
vi.mock('$lib/components/items/CopyItemDialog.svelte', () => import('../../../test/StubComponent.svelte'));
vi.mock('$lib/components/items/PushToAgentDialog.svelte', () => import('../../../test/StubComponent.svelte'));
vi.mock('$lib/components/items/ItemAttachmentStrip.svelte', () => import('../../../test/StubComponent.svelte'));
vi.mock('$lib/components/attachments/AttachmentSurfaceHost.svelte', () => import('../../../test/StubComponent.svelte'));

const collab = vi.hoisted(() => ({ lastOpLogID: 42 }));
vi.mock('$lib/collab/wsProvider.svelte', () => ({
	CollabProvider: class {
		state = 'connected';
		synced = true;
		lastOpLogID = collab.lastOpLogID;
		itemID: string;
		awareness = { setLocalStateField() {}, on() {}, off() {}, getStates: () => new Map() };
		constructor(itemId: string) {
			this.itemID = itemId;
		}
		destroy() {}
	},
}));

const COLL = vi.hoisted(() => ({
	id: 'c1',
	slug: 'tasks',
	name: 'Tasks',
	prefix: 'TASK',
	schema: '{"fields":[]}',
	settings: '{}',
}));
function itemFor(slug: string) {
	return {
		id: slug,
		slug,
		title: `Item ${slug}`,
		item_number: 1,
		collection_slug: 'tasks',
		collection_id: 'c1',
		fields: '{}',
		tags: '[]',
		content: `original body of ${slug}`,
		seq: 1,
		created_at: '2026-01-01T00:00:00Z',
		updated_at: '2026-01-01T00:00:00Z',
	};
}

vi.mock('$lib/api/client', () => ({
	api: {
		items: {
			get: vi.fn(async (_ws: string, slug: string) => itemFor(slug)),
			update: vi.fn(async (_ws: string, id: string) => itemFor(id)),
			flushCollabContent: vi.fn(async (_ws: string, id: string) => itemFor(id)),
			progress: vi.fn(async () => ({ total: 0, done: 0, percentage: 0 })),
		},
		collections: { get: vi.fn(async () => COLL), list: vi.fn(async () => [COLL]) },
		links: { list: vi.fn(async () => []) },
		members: { list: vi.fn(async () => ({ members: [] })) },
		agentRoles: { list: vi.fn(async () => []) },
		tags: { list: vi.fn(async () => []) },
	},
	PadApiError: class PadApiError extends Error {
		code = '';
	},
	isUpdateConflictError: () => false,
}));

vi.mock('$lib/services/sse.svelte', () => ({
	sseService: {
		onItemEvent: () => () => {},
		connect: vi.fn(),
		disconnect: vi.fn(),
		get connected() {
			return true;
		},
		get state() {
			return 'open';
		},
	},
}));
vi.mock('$lib/services/sync.svelte', () => ({
	syncService: {
		onSync: () => () => {},
		markSynced: vi.fn(),
	},
}));
vi.mock('$lib/stores/localIndex.svelte', () => ({
	localIndex: {
		bootstrap: vi.fn(async () => {}),
		getAll: () => [],
		retagCollection: vi.fn(),
		bootstrapStateFor: () => 'cold',
	},
}));
vi.mock('$lib/stores/workspace.svelte', () => ({
	workspaceStore: {
		canEditItem: () => true,
		get isOwner() {
			return true;
		},
		setCurrent: vi.fn(async () => {}),
	},
}));

const auth = vi.hoisted(() => {
	const hook = { read: null as null | (() => number), write: null as null | ((n: number) => void) };
	let fallback = 0;
	const getEpoch = () => (hook.read ? hook.read() : fallback);
	return {
		__hook: hook,
		get identityEpoch() {
			return getEpoch();
		},
		get userId() {
			return 'u1';
		},
		get user() {
			return { id: 'u1', name: 'A' };
		},
		get authenticated() {
			return true;
		},
		identityFence() {
			const captured = getEpoch();
			return () => getEpoch() === captured;
		},
		onIdentityChange: () => () => {},
	};
});
vi.mock('$lib/stores/auth.svelte', () => ({ authStore: auth }));

import { api } from '$lib/api/client';
import ItemDetail from './ItemDetail.svelte';

type Props = Record<string, unknown>;
const stubs = () => (globalThis as { __stubProps?: Props[] }).__stubProps ?? [];

function mount(ref = 'i1') {
	return render(ItemDetail, { props: { username: 'u', wsSlug: 'ws', collSlug: 'tasks', ref } });
}

async function settle() {
	flushSync();
	await tick();
	await new Promise((r) => setTimeout(r, 0));
	await tick();
}

async function mountWithRichEditor(draft: string) {
	const r = mount('i1');
	await waitFor(() => expect(r.container.textContent).toContain('Item i1'));
	const editorProps = await waitFor(() => {
		const p = stubs().find((s) => typeof s.onEditor === 'function');
		if (!p) throw new Error('rich editor did not mount');
		return p;
	});
	let current = draft;
	(editorProps.onEditor as (e: unknown) => void)({
		isDestroyed: false,
		isEditable: true,
		storage: { markdown: { getMarkdown: () => current } },
		commands: { setContent() {}, focus() {} },
		on() {},
		off() {},
	});
	await settle();
	return {
		r,
		editorProps,
		setDraft(next: string) {
			current = next;
		},
	};
}

function flushCalls() {
	return vi.mocked(api.items.flushCollabContent).mock.calls;
}

function expectEveryFlushHasNumericCursor(expected: number) {
	const calls = flushCalls();
	expect(calls.length, 'expected at least one flushCollabContent call').toBeGreaterThan(0);
	for (const call of calls) {
		const opts = call[3] as { opLogCursor?: unknown } | undefined;
		expect(typeof opts?.opLogCursor, `flushCollabContent opts=${JSON.stringify(opts)}`).toBe('number');
		expect(opts?.opLogCursor).toBe(expected);
	}
}

beforeEach(() => {
	(globalThis as { __stubProps?: Props[] }).__stubProps = [];
	resetEpoch();
	bindReactiveEpoch(auth.__hook);
	collab.lastOpLogID = 42;
	vi.mocked(api.items.get).mockClear();
	vi.mocked(api.items.update).mockClear();
	vi.mocked(api.items.flushCollabContent).mockClear();
});

afterEach(() => {
	vi.useRealTimers();
	cleanup();
});

describe('BUG-3558: every collab flush trigger sends a numeric opLogCursor', () => {
	it('5s idle debounce sends a numeric opLogCursor', async () => {
		const { editorProps } = await mountWithRichEditor('original body of i1');
		vi.useFakeTimers();
		(editorProps.onUpdate as (md: string) => void)('debounced edit');
		expect(flushCalls().length).toBe(0);
		await vi.advanceTimersByTimeAsync(5_000);
		expect(flushCalls().length).toBe(1);
		expectEveryFlushHasNumericCursor(42);
	});

	it('pagehide teardown handler sends a numeric opLogCursor', async () => {
		await mountWithRichEditor('edited before pagehide');
		window.dispatchEvent(new Event('pagehide'));
		expectEveryFlushHasNumericCursor(42);
		expect(flushCalls()[0][3]).toMatchObject({ keepalive: true, opLogCursor: 42 });
	});

	it('visibilitychange->hidden teardown handler sends a numeric opLogCursor', async () => {
		await mountWithRichEditor('edited before hide');
		const desc = Object.getOwnPropertyDescriptor(document, 'visibilityState');
		Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'hidden' });
		try {
			document.dispatchEvent(new Event('visibilitychange'));
		} finally {
			if (desc) Object.defineProperty(document, 'visibilityState', desc);
			else delete (document as { visibilityState?: unknown }).visibilityState;
		}
		expectEveryFlushHasNumericCursor(42);
		expect(flushCalls()[0][3]).toMatchObject({ keepalive: true, opLogCursor: 42 });
	});

	it('raw-mode toggle sends a numeric opLogCursor', async () => {
		const { r } = await mountWithRichEditor('edited before raw toggle');
		await fireEvent.click(r.getByTitle('Raw markdown editor'));
		await waitFor(() => expect(flushCalls().length).toBeGreaterThan(0));
		expectEveryFlushHasNumericCursor(42);
		expect(flushCalls()[0][3]).toMatchObject({ keepalive: false, opLogCursor: 42 });
	});

	it('editor teardown (unmount) sends a numeric opLogCursor', async () => {
		const { r } = await mountWithRichEditor('edited before unmount');
		r.unmount();
		expectEveryFlushHasNumericCursor(42);
		expect(flushCalls()[0][3]).toMatchObject({ keepalive: true, opLogCursor: 42 });
	});

	it('editor teardown (item switch) sends a numeric opLogCursor for the outgoing item', async () => {
		const { r } = await mountWithRichEditor('edited before switch');
		await r.rerender({ username: 'u', wsSlug: 'ws', collSlug: 'tasks', ref: 'i2' });
		await waitFor(() => expect(r.container.textContent).toContain('Item i2'));
		expectEveryFlushHasNumericCursor(42);
		expect(flushCalls()[0][1]).toBe('i1');
		expect(flushCalls()[0][3]).toMatchObject({ keepalive: true, opLogCursor: 42 });
	});

	it('sends numeric opLogCursor 0 on an empty op-log (not omitted as falsy)', async () => {
		collab.lastOpLogID = 0;
		await mountWithRichEditor('edited on fresh op-log');
		window.dispatchEvent(new Event('pagehide'));
		expectEveryFlushHasNumericCursor(0);
		expect(flushCalls()[0][3]).toMatchObject({ keepalive: true, opLogCursor: 0 });
	});
});
