/**
 * BUG-3230 U0 — the pane's LEGACY RICH save (the no-provider branch of
 * handleContentUpdate) asks to be refused rather than replace another tab's
 * unstored edits, and a refusal becomes the pendingEditsDialog question.
 *
 * No e2e reaches this branch: the EDITABLE editor mounts only under a ydoc, and
 * the provider and ydoc are cleared together. The viewer editor, mounted with no
 * provider, is wired to the same handleContentUpdate, so its recorded onUpdate
 * prop is the seam that forces the branch here. That the server would refuse a
 * viewer's PATCH anyway is beside the point: the unit under test is what the
 * branch SENDS, and this is the only mount that runs it.
 */
import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest';
import { cleanup, render, waitFor } from '@testing-library/svelte';
import { flushSync, tick } from 'svelte';

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
vi.mock('$lib/collab/wsProvider.svelte', () => ({
	CollabProvider: class {
		state = 'connecting';
		synced = false;
		lastOpLogID = undefined;
		itemID: string;
		awareness = { setLocalStateField() {}, on() {}, off() {}, getStates: () => new Map() };
		constructor(itemId: string) {
			this.itemID = itemId;
		}
		destroy() {}
	},
}));

const collFor = (slug: string) => ({
	id: `c-${slug}`, slug, name: slug, prefix: 'TASK',
	schema: '{"fields":[]}', settings: '{}',
});
function itemFor(slug: string, collSlug = 'tasks') {
	return {
		id: slug, slug, title: `Item ${slug}`, item_number: 1, collection_slug: collSlug, collection_id: `c-${collSlug}`,
		fields: '{}', tags: '[]', content: `body of ${slug}`, seq: 1,
		created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z',
	};
}

vi.mock('$lib/api/client', () => {
	class PadApiError extends Error {
		code: string;
		constructor(code = '') {
			super(code);
			this.code = code;
		}
	}
	return {
		api: {
			items: {
				get: vi.fn(async (_ws: string, slug: string) => itemFor(slug)),
				update: vi.fn(async (_ws: string, id: string) => itemFor(id)),
				flushCollabContent: vi.fn(async (_ws: string, id: string) => itemFor(id)),
				progress: vi.fn(async () => ({ total: 0, done: 0, percentage: 0 })),
			},
			collections: { get: vi.fn(async (_ws: string, slug: string) => collFor(slug)), list: vi.fn(async () => [collFor('tasks')]) },
			links: { list: vi.fn(async () => []) },
			members: { list: vi.fn(async () => ({ members: [] })) },
			agentRoles: { list: vi.fn(async () => []) },
			tags: { list: vi.fn(async () => []) },
		},
		PadApiError,
		isUpdateConflictError: () => false,
		isSupersededWriteError: (e: unknown) => (e as { code?: string })?.code === 'superseded_write',
	};
});
vi.mock('$lib/services/sse.svelte', () => ({
	sseService: {
		onItemEvent: () => () => {},
		connect: vi.fn(), disconnect: vi.fn(),
		get connected() { return true; }, get state() { return 'open'; },
	},
}));
vi.mock('$lib/services/sync.svelte', () => ({
	syncService: { onSync: () => () => {}, markSynced: vi.fn() },
}));
vi.mock('$lib/stores/localIndex.svelte', () => ({
	localIndex: { bootstrap: vi.fn(async () => {}), getAll: () => [], retagCollection: vi.fn() },
}));
// The viewer branch: no provider is ever minted, so handleContentUpdate takes
// its legacy (no-provider) path.
vi.mock('$lib/stores/workspace.svelte', () => ({
	workspaceStore: { canEditItem: () => false, get isOwner() { return false; }, setCurrent: vi.fn(async () => {}) },
}));
vi.mock('$lib/stores/auth.svelte', () => ({
	authStore: {
		get identityEpoch() { return 0; },
		get userId() { return 'u1'; },
		get user() { return { id: 'u1', name: 'A' }; },
		get authenticated() { return true; },
		identityFence: () => () => true,
		onIdentityChange: () => () => {},
	},
}));
vi.mock('$lib/stores/pendingEditsDialog.svelte', () => ({
	pendingEditsDialog: { request: vi.fn(async () => true), keep: vi.fn(), overwrite: vi.fn(), abandonAll: vi.fn(), active: null },
}));

import { api, PadApiError } from '$lib/api/client';
import { pendingEditsDialog } from '$lib/stores/pendingEditsDialog.svelte';
import ItemDetail from './ItemDetail.svelte';

const sink: Record<string, unknown>[] = [];
(globalThis as { __stubProps?: Record<string, unknown>[] }).__stubProps = sink;

async function settle() {
	flushSync();
	await tick();
	await new Promise((r) => setTimeout(r, 0));
	await tick();
}

/** The viewer editor's onUpdate: the page's real handleContentUpdate. */
async function mountAndGetOnUpdate(): Promise<(md: string) => void> {
	const r = render(ItemDetail, { props: { username: 'u', wsSlug: 'ws', collSlug: 'tasks', ref: 'i1' } });
	await settle();
	await waitFor(() => expect(r.container.textContent).toContain('Item i1'));
	const editor = [...sink].reverse().find((p) => p.editable === false && typeof p.onUpdate === 'function');
	expect(editor, 'the viewer editor did not mount with an onUpdate: this suite forces nothing').toBeDefined();
	return editor!.onUpdate as (md: string) => void;
}

const contentUpdates = () =>
	vi.mocked(api.items.update).mock.calls.filter((c) => c[2] && 'content' in (c[2] as object)).map((c) => c[2] as Record<string, unknown>);

beforeEach(() => {
	sink.length = 0;
	vi.mocked(api.items.update).mockReset();
	vi.mocked(api.items.update).mockImplementation(async (_ws: string, id: string) => itemFor(id) as never);
	vi.mocked(pendingEditsDialog.request).mockClear();
});

afterEach(() => {
	cleanup();
});

describe('the legacy rich save refuses over unstored edits (BUG-3230 U0)', () => {
	it('sends refuse_pending_edits with the body', async () => {
		const onUpdate = await mountAndGetOnUpdate();
		onUpdate('typed in the rich editor');
		await waitFor(() => expect(contentUpdates().length).toBe(1), { timeout: 3000 });
		const sent = contentUpdates()[0]!;
		expect(sent.content).toBe('typed in the rich editor');
		expect(sent.refuse_pending_edits).toBe(true);
		expect(sent.overwrite_pending_edits).toBeUndefined();
	});

	it('a refusal asks, and Overwrite resends once with overwrite_pending_edits', async () => {
		vi.mocked(api.items.update).mockImplementationOnce(async () => {
			throw new (PadApiError as unknown as new (c: string) => Error)('content_pending_flush');
		});
		const onUpdate = await mountAndGetOnUpdate();
		onUpdate('typed in the rich editor');
		await waitFor(() => expect(contentUpdates().length).toBe(2), { timeout: 3000 });
		expect(pendingEditsDialog.request).toHaveBeenCalledTimes(1);
		const [first, second] = contentUpdates();
		expect(first!.overwrite_pending_edits).toBeUndefined();
		expect(second!.content).toBe('typed in the rich editor');
		expect(second!.overwrite_pending_edits).toBe(true);
	});

	it('a refusal answered Keep sends nothing more', async () => {
		vi.mocked(api.items.update).mockImplementationOnce(async () => {
			throw new (PadApiError as unknown as new (c: string) => Error)('content_pending_flush');
		});
		vi.mocked(pendingEditsDialog.request).mockResolvedValueOnce(false);
		const onUpdate = await mountAndGetOnUpdate();
		onUpdate('typed in the rich editor');
		await waitFor(() => expect(pendingEditsDialog.request).toHaveBeenCalledTimes(1), { timeout: 3000 });
		await new Promise((r) => setTimeout(r, 50));
		expect(contentUpdates().length).toBe(1);
	});

	it('Overwrite does not resend text older than typing done while the question was open', async () => {
		let onUpdate: (md: string) => void = () => {};
		vi.mocked(api.items.update).mockImplementationOnce(async () => {
			throw new (PadApiError as unknown as new (c: string) => Error)('content_pending_flush');
		});
		vi.mocked(pendingEditsDialog.request).mockImplementationOnce(async () => {
			onUpdate('newer typing');
			return true;
		});
		onUpdate = await mountAndGetOnUpdate();
		onUpdate('older typing');
		// The newer typing arms its own save, which lands; the older text is never resent.
		await waitFor(() => expect(contentUpdates().map((c) => c.content)).toEqual(['older typing', 'newer typing']), { timeout: 4000 });
		expect(contentUpdates()[1]!.overwrite_pending_edits).toBeUndefined();
	});
});
