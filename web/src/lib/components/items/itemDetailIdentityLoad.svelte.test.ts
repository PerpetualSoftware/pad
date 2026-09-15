/**
 * BUG-3084, ItemDetail — the BEHAVIOURAL half. A MOUNT of ItemDetail, which no
 * suite had before this one: every other ItemDetail test is a source guard.
 *
 * WHAT THE MOUNT IS. Every child component is stubbed and `wsProvider` refuses
 * construction, so the collab and editor internals are NOT under test. What
 * runs for real is ItemDetail's own script: `loadData`, its generation
 * counters, the raw content saver and the identity path. That is the subject.
 *
 * THE FAKE AUTH STORE IS REACTIVE, and it has to be. `identityEpoch` is
 * `$state` in the real store, so a read of it inside an `$effect` is a
 * DEPENDENCY. The first probe for this suite bound the reactive hook after
 * mount, saw an identity move leave the load alone, and was wrong for exactly
 * that reason (BUG-3084 checkpoint 33): the effect's first run had read a
 * plain variable. Every leg below states the precondition it needs.
 *
 * ONE CONTROL PER REFUSAL. "No PATCH was sent" is satisfied as well by a flush
 * that never ran, so each refusal leg has a control that makes the same flush
 * run under an UNCHANGED identity and sees the PATCH go out.
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
// An INERT provider rather than a throwing one. Rich mode mounts a provider as
// soon as an editable item loads, so refusing construction turned every mount
// into uncaught effect errors; this one never connects, never syncs, and is
// only here so the component's own script can run around it.
const collab = vi.hoisted(() => ({ synced: false }));
vi.mock('$lib/collab/wsProvider.svelte', () => ({
	CollabProvider: class {
		state = 'connecting';
		synced = collab.synced;
		lastOpLogID = undefined;
		itemID: string;
		awareness = { setLocalStateField() {}, on() {}, off() {}, getStates: () => new Map() };
		constructor(itemId: string) {
			this.itemID = itemId;
		}
		destroy() {}
	},
}));

const COLL = vi.hoisted(() => ({
	id: 'c1', slug: 'tasks', name: 'Tasks', prefix: 'TASK',
	schema: '{"fields":[{"key":"estimate","label":"Estimate","type":"text"}]}', settings: '{}',
}));
function itemFor(slug: string) {
	return {
		id: slug, slug, title: `Item ${slug}`, item_number: 1, collection_slug: 'tasks', collection_id: 'c1',
		fields: '{}', tags: '[]', content: `body of ${slug}`, seq: 1,
		created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z',
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
	PadApiError: class PadApiError extends Error { code = ''; },
	isUpdateConflictError: () => false,
}));
vi.mock('$lib/services/sse.svelte', () => ({
	sseService: {
		onItemEvent: () => () => {}, connect: vi.fn(), disconnect: vi.fn(),
		get connected() { return true; }, get state() { return 'open'; },
	},
}));
const syncCallbacks = vi.hoisted(() => [] as Array<(result: unknown) => unknown>);
vi.mock('$lib/services/sync.svelte', () => ({
	syncService: {
		onSync: (fn: (result: unknown) => unknown) => {
			syncCallbacks.push(fn);
			return () => {};
		},
		markSynced: vi.fn(),
	},
}));
vi.mock('$lib/stores/localIndex.svelte', () => ({
	localIndex: { bootstrap: vi.fn(async () => {}), getAll: () => [], retagCollection: vi.fn() },
}));
vi.mock('$lib/stores/workspace.svelte', () => ({
	workspaceStore: { canEditItem: () => true, get isOwner() { return true; }, setCurrent: vi.fn(async () => {}) },
}));

/**
 * The family's fake auth store, on the collection suite's pattern: the epoch is
 * read through a late-bound hook backed by a real `$state` signal, and
 * listeners are dispatched AFTER the bump, matching the real store's order.
 */
const auth = vi.hoisted(() => {
	const hook = { read: null as null | (() => number), write: null as null | ((n: number) => void) };
	let fallback = 0;
	const getEpoch = () => (hook.read ? hook.read() : fallback);
	const listeners: Array<(previousUserId: string) => void> = [];
	return {
		__hook: hook,
		get identityEpoch() { return getEpoch(); },
		get userId() { return 'u1'; },
		get user() { return { id: 'u1', name: 'A' }; },
		get authenticated() { return true; },
		/** What `notifyIdentityChange` does: bump, THEN notify. */
		moveIdentity(previousUserId = 'u0') {
			if (hook.write) hook.write(getEpoch() + 1);
			else fallback++;
			for (const fn of [...listeners]) fn(previousUserId);
		},
		identityFence() {
			const captured = getEpoch();
			return () => getEpoch() === captured;
		},
		onIdentityChange(fn: (previousUserId: string) => void) {
			listeners.push(fn);
			return () => {
				const i = listeners.indexOf(fn);
				if (i >= 0) listeners.splice(i, 1);
			};
		},
		reset() { listeners.length = 0; },
	};
});
vi.mock('$lib/stores/auth.svelte', () => ({ authStore: auth }));

import { api } from '$lib/api/client';
import { localIndex } from '$lib/stores/localIndex.svelte';
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

function itemGets(): number {
	return vi.mocked(api.items.get).mock.calls.length;
}

function contentPatches(): Array<{ id: string; content: unknown; keepalive: boolean }> {
	return vi
		.mocked(api.items.update)
		.mock.calls.filter((c) => c[2] && 'content' in (c[2] as object))
		.map((c) => ({
			id: c[1] as string,
			content: (c[2] as { content: unknown }).content,
			keepalive: !!(c[3] as { keepalive?: boolean } | undefined)?.keepalive,
		}));
}

/** Loaded, switched to raw mode, one edit typed and still debounced. */
async function withPendingRawDraft(r: ReturnType<typeof mount>, draft: string) {
	await waitFor(() => expect(r.container.textContent).toContain('Item i1'));
	await fireEvent.click(r.getByTitle('Raw markdown editor'));
	const raw = await waitFor(() => {
		const p = stubs().find((s) => typeof s.onUpdate === 'function');
		if (!p) throw new Error('the raw editor did not mount');
		return p;
	});
	(raw.onUpdate as (md: string) => void)(draft);
	await settle();
	expect(contentPatches(), 'the draft must still be debounced, not saved').toEqual([]);
}

beforeEach(() => {
	(globalThis as { __stubProps?: Props[] }).__stubProps = [];
	resetEpoch();
	auth.reset();
	// Bound BEFORE mount: an effect whose first run reads a plain variable never
	// records a dependency, and every "did the load re-run" answer is then no.
	bindReactiveEpoch(auth.__hook);
	vi.mocked(api.items.get).mockClear();
	vi.mocked(api.items.update).mockClear();
	vi.mocked(api.items.flushCollabContent).mockClear();
	vi.mocked(api.members.list).mockClear();
	collab.synced = false;
	vi.mocked(api.tags.list).mockClear();
	vi.mocked(localIndex.retagCollection).mockClear();
	syncCallbacks.length = 0;
});

afterEach(() => {
	cleanup();
});

type Deferred = { resolve: (v: unknown) => void; reject: (e: unknown) => void };
function deferNext(fn: (...args: never[]) => unknown): Deferred[] {
	const sink: Deferred[] = [];
	vi.mocked(fn as (...a: unknown[]) => unknown).mockImplementationOnce(
		() => new Promise((resolve, reject) => sink.push({ resolve, reject }))
	);
	return sink;
}

async function loaded(r: ReturnType<typeof mount>) {
	await waitFor(() => expect(r.container.textContent).toContain('Item i1'));
}

describe('an identity change reloads ItemDetail', () => {
	it('re-runs the load when the identity moves (the precondition every leg below needs)', async () => {
		const r = mount();
		await waitFor(() => expect(r.container.textContent).toContain('Item i1'));
		const before = itemGets();
		auth.moveIdentity();
		await settle();
		await waitFor(() => expect(itemGets()).toBe(before + 1));
	});

	it('loads EXACTLY once per identity move — the listener, not also a tracked read in the effect', async () => {
		const r = mount();
		await loaded(r);
		const before = itemGets();
		auth.moveIdentity();
		await settle();
		await new Promise((res) => setTimeout(res, 20));
		await settle();
		expect(itemGets()).toBe(before + 1);
	});

	it('lands in the error state, not a throw or a stuck spinner, when the item is gone for the new identity', async () => {
		const r = mount();
		await loaded(r);
		vi.mocked(api.items.get).mockImplementationOnce(async () => {
			throw new Error('item not visible to this identity');
		});
		auth.moveIdentity();
		await settle();
		await waitFor(() => expect(r.container.textContent).toContain('item not visible to this identity'));
		expect(r.container.textContent).not.toContain('Item i1');
	});
});

describe('the previous identity\'s RICH draft is not flushed on teardown (live on main, like the raw one)', () => {
	/**
	 * Mounts the rich editor: a provider that reports synced, and a fake
	 * editor handed back through the Editor stub's `onEditor` — the seam the
	 * real Editor uses. Its markdown differs from the loaded body, so a flush
	 * is not deduped away.
	 */
	async function withRichDraft(r: ReturnType<typeof mount>, draft: string) {
		await loaded(r);
		const editorProps = await waitFor(() => {
			const p = stubs().find((s) => typeof s.onEditor === 'function');
			if (!p) throw new Error('the rich editor did not mount');
			return p;
		});
		(editorProps.onEditor as (e: unknown) => void)({
			isDestroyed: false,
			isEditable: true,
			storage: { markdown: { getMarkdown: () => draft } },
			commands: { setContent() {}, focus() {} },
			on() {},
			off() {},
		});
		await settle();
	}

	function richFlushes(): unknown[] {
		return vi.mocked(api.items.flushCollabContent).mock.calls.map((c) => c[2]);
	}

	it('REFUSAL: a pagehide after an identity change does not PATCH the old context\'s markdown', async () => {
		collab.synced = true;
		const r = mount();
		await withRichDraft(r, 'OLD RICH DRAFT');
		auth.moveIdentity();
		await settle();
		await loaded(r);
		window.dispatchEvent(new Event('pagehide'));
		await settle();
		expect(richFlushes()).not.toContain('OLD RICH DRAFT');
	});

	it('CONTROL: the same pagehide under an unchanged identity DOES flush it', async () => {
		collab.synced = true;
		const r = mount();
		await withRichDraft(r, 'OLD RICH DRAFT');
		window.dispatchEvent(new Event('pagehide'));
		await settle();
		await waitFor(() => expect(richFlushes()).toContain('OLD RICH DRAFT'));
	});
});

describe('state a load reuses is not carried across identities', () => {
	it('the member list is REFETCHED for the new identity, not served from the previous identity\'s cache', async () => {
		const r = mount();
		await loaded(r);
		await waitFor(() => expect(vi.mocked(api.members.list).mock.calls.length).toBe(1));
		auth.moveIdentity();
		await settle();
		await loaded(r);
		await waitFor(() => expect(vi.mocked(api.members.list).mock.calls.length).toBe(2));
	});

	it('CONTROL: a same-identity reload of the same workspace serves the cache', async () => {
		const r = mount();
		await loaded(r);
		await waitFor(() => expect(vi.mocked(api.members.list).mock.calls.length).toBe(1));
		await r.rerender({ username: 'u', wsSlug: 'ws', collSlug: 'tasks', ref: 'i2' });
		await waitFor(() => expect(r.container.textContent).toContain('Item i2'));
		await settle();
		expect(vi.mocked(api.members.list).mock.calls.length).toBe(1);
	});
});

describe('a child instance mounted under the previous identity cannot commit through its callback prop', () => {
	/**
	 * The parent side of class C (BUG-3084 checkpoint 36, lead ruling): a child
	 * keeps running after the parent moves on, and its promise calls back into
	 * the parent. The callback is gated on the identity key the child was
	 * HANDED DOWN under — a `{@const}` inside a `{#key identityKey}` block, which
	 * freezes because the identity change remounts the block.
	 */
	function backlinks(): Array<Record<string, unknown>> {
		return stubs().filter((s) => typeof s.onCountChange === 'function');
	}

	async function countFrom(which: 'old' | 'new') {
		const r = mount();
		await loaded(r);
		const old = backlinks().at(-1)!;
		auth.moveIdentity();
		await settle();
		await loaded(r);
		const fresh = backlinks().at(-1)!;
		expect(fresh, 'the child was not remounted by the identity change').not.toBe(old);
		((which === 'old' ? old : fresh).onCountChange as (n: number) => void)(7);
		await settle();
		return r.container.textContent ?? '';
	}

	it('REFUSAL: the previous identity\'s instance reporting a count is ignored', async () => {
		expect(await countFrom('old')).not.toContain('📎 7');
	});

	it('CONTROL: the remounted instance reporting the same count lands', async () => {
		expect(await countFrom('new')).toContain('📎 7');
	});
});

describe('continuations started under the previous identity do not commit', () => {
	function fieldEditor(): Record<string, unknown> {
		const p = stubs().filter((s) => typeof s.onchange === 'function' && s.field).at(-1);
		if (!p) throw new Error('no FieldEditor mounted — the schema field did not render');
		return p;
	}
	function tagInput(): Record<string, unknown> {
		const p = stubs().filter((s) => typeof s.onchange === 'function' && 'suggestions' in s).at(-1);
		if (!p) throw new Error('no TagInput mounted');
		return p;
	}

	async function fieldWriteRace(moveIdentity: boolean) {
		const r = mount();
		await loaded(r);
		const pending = deferNext(api.items.update);
		(fieldEditor().onchange as (v: unknown) => void)('5');
		await waitFor(() => expect(pending.length).toBe(1));
		if (moveIdentity) {
			auth.moveIdentity();
			await settle();
			await waitFor(() => expect(r.container.textContent).toContain('Item i1'));
		}
		pending[0]!.resolve({ ...itemFor('i1'), title: 'echo from the field write' });
		await settle();
		return r;
	}

	it('REFUSAL (generation fence, closed by the listener\'s load): a field write\'s echo does not commit', async () => {
		const r = await fieldWriteRace(true);
		await new Promise((res) => setTimeout(res, 20));
		expect(r.container.textContent).not.toContain('echo from the field write');
	});

	it('CONTROL: the same echo commits under an unchanged identity', async () => {
		const r = await fieldWriteRace(false);
		await waitFor(() => expect(r.container.textContent).toContain('echo from the field write'));
	});

	async function tagDrainRace(moveIdentity: boolean) {
		const r = mount();
		await loaded(r);
		const first = deferNext(api.items.update);
		(tagInput().onchange as (t: string[]) => void)(['a']);
		await waitFor(() => expect(first.length).toBe(1));
		// Queued behind the in-flight batch, typed by the same user.
		(tagInput().onchange as (t: string[]) => void)(['a', 'b']);
		if (moveIdentity) {
			auth.moveIdentity();
			await settle();
		}
		first[0]!.resolve({ ...itemFor('i1'), tags: '["a"]' });
		await settle();
		await new Promise((res) => setTimeout(res, 20));
		return vi
			.mocked(api.items.update)
			.mock.calls.filter((c) => c[2] && 'tags' in (c[2] as object))
			.map((c) => (c[2] as { tags: string }).tags);
	}

	it('REFUSAL (explicit fence, flushTagSaver): the queued batch is not sent on the new identity', async () => {
		expect(await tagDrainRace(true)).toEqual(['["a"]']);
	});

	it('CONTROL: the queued batch IS sent under an unchanged identity', async () => {
		expect(await tagDrainRace(false)).toEqual(['["a"]', '["a","b"]']);
	});

	/**
	 * The previous identity's tag vocabulary, resolved late. Captured by
	 * holding every `tags.list` open: the mount's own call and the listener's
	 * re-run each get their own deferred, so the leg can resolve the OLD one
	 * last and still tell the two apart.
	 */
	it('a tag edit typed by the NEW identity is sent, not folded into the previous identity\'s burst', async () => {
		const r = mount();
		await loaded(r);
		const first = deferNext(api.items.update);
		(tagInput().onchange as (t: string[]) => void)(['a']);
		await waitFor(() => expect(first.length).toBe(1));
		auth.moveIdentity();
		await settle();
		await loaded(r);
		// Still in flight: the previous identity's burst is running. An edit
		// coalesced into it would be dropped by that burst's refusal.
		(tagInput().onchange as (t: string[]) => void)(['x']);
		await settle();
		first[0]!.resolve({ ...itemFor('i1'), tags: '["a"]' });
		await settle();
		await new Promise((res) => setTimeout(res, 20));
		const sent = vi
			.mocked(api.items.update)
			.mock.calls.filter((c) => c[2] && 'tags' in (c[2] as object))
			.map((c) => (c[2] as { tags: string }).tags);
		expect(sent).toEqual(['["a"]', '["x"]']);
	});

	async function suggestionsRace(moveIdentity: boolean, oldOutcome: 'resolve' | 'reject' = 'resolve') {
		const calls: Deferred[] = [];
		vi.mocked(api.tags.list).mockImplementation(
			() => new Promise((resolve, reject) => calls.push({ resolve, reject }))
		);
		try {
			const r = mount();
			await loaded(r);
			await waitFor(() => expect(calls.length).toBeGreaterThanOrEqual(1));
			const old = calls[0]!;
			if (moveIdentity) {
				auth.moveIdentity();
				await settle();
				await waitFor(() => expect(calls.length).toBe(2));
				calls[1]!.resolve([{ tag: 'current-user-tag' }]);
				await settle();
			}
			if (oldOutcome === 'resolve') old.resolve([{ tag: 'previous-user-tag' }]);
			else old.reject(new Error('previous identity lost access'));
			await settle();
			return tagInput().suggestions as string[];
		} finally {
			vi.mocked(api.tags.list).mockImplementation(async () => []);
		}
	}

	it('REFUSAL (explicit fence, loadTagSuggestions) + RECOVERY: the old vocabulary never lands, the re-run does', async () => {
		expect(await suggestionsRace(true)).toEqual(['current-user-tag']);
	});

	it('CONTROL: the same vocabulary lands under an unchanged identity', async () => {
		expect(await suggestionsRace(false)).toEqual(['previous-user-tag']);
	});

	it('REFUSAL (failure arm): the previous identity\'s failed request does not clear the recovered vocabulary', async () => {
		expect(await suggestionsRace(true, 'reject')).toEqual(['current-user-tag']);
	});

	it('CONTROL: a failed request clears the vocabulary under an unchanged identity', async () => {
		expect(await suggestionsRace(false, 'reject')).toEqual([]);
	});

	/**
	 * A sync result drives `reconcileCollectionSegment`, whose collection list
	 * names the loaded collection under a NEW slug — the rename heal, which
	 * retags the local index before it navigates.
	 */
	async function reconcileRace(moveIdentity: boolean) {
		const r = mount();
		await loaded(r);
		const lists = deferNext(api.collections.list);
		const cb = syncCallbacks.at(-1);
		if (!cb) throw new Error('no sync subscription');
		const settled = cb({ workspace: 'ws', type: 'caught_up' });
		await waitFor(() => expect(lists.length).toBe(1));
		if (moveIdentity) {
			auth.moveIdentity();
			await settle();
		}
		lists[0]!.resolve([{ ...COLL, slug: 'renamed' }]);
		await settled;
		await settle();
		return vi.mocked(localIndex.retagCollection).mock.calls.length;
	}

	it('REFUSAL (explicit fence, reconcileCollectionSegment): the previous identity\'s list does not retag', async () => {
		expect(await reconcileRace(true)).toBe(0);
	});

	it('CONTROL: the same list retags under an unchanged identity', async () => {
		expect(await reconcileRace(false)).toBe(1);
	});
});

describe('the load a moved identity triggers does not persist the previous user\'s draft', () => {
	it('REFUSAL: a pending raw draft is not PATCHed when the identity moves', async () => {
		const r = mount();
		await withPendingRawDraft(r, 'typed by the previous user');
		const before = itemGets();
		auth.moveIdentity();
		await settle();
		await waitFor(() => expect(itemGets(), 'the load did not re-run, so this leg measures nothing').toBe(before + 1));
		expect(contentPatches()).toEqual([]);
	});

	it('CONTROL: the same draft IS flushed, keepalive, when a load runs under an unchanged identity', async () => {
		const r = mount();
		await withPendingRawDraft(r, 'typed by the signed-in user');
		await r.rerender({ username: 'u', wsSlug: 'ws', collSlug: 'tasks', ref: 'i2' });
		await settle();
		await waitFor(() =>
			expect(contentPatches()).toEqual([{ id: 'i1', content: 'typed by the signed-in user', keepalive: true }])
		);
	});
});
