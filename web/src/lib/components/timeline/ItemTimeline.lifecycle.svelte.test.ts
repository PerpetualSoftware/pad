import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { flushSync, mount, unmount, tick } from 'svelte';
import type { Comment, TimelineEntry, TimelineResponse } from '$lib/types';

/**
 * Timeline attMeta LIFECYCLE reconciliation (PLAN-2392 3c-iii U1 / TASK-2510).
 *
 * The timeline caches HEAD-probe results in `attMeta` and — before this task —
 * had NO invalidation path: it subscribed to neither the deletion bus nor any
 * archive/restore signal, so a deleted attachment stayed rendered as a live
 * `<img>`, an archived parent kept painting a soon-to-be-broken image, and a
 * restore never escaped a `missing` cached while archived.
 *
 * These tests drive the three reconciliation surfaces directly:
 *  - the deletion bus (REAL, so the announce genuinely fans out to the
 *    timeline's newly-registered listener — the TASK-2477 lesson),
 *  - the `parentArchived` PROP edges (false→true archive, true→false restore),
 *  - and the shared module cache, whose cached-vs-no-store behaviour is the
 *    whole point of several assertions.
 *
 * The metadata module is MOCKED with two distinct fns so a test can prove WHICH
 * path a probe took: `fetchAttachmentMetadata` is the cache-sharing HEAD and
 * `revalidateAttachmentMetadata` is the no-store existence probe. Making them
 * answer differently is what makes "used no-store" / "used cache" falsifiable.
 *
 * The markdown pipeline is REAL: a resolved id renders an `<img
 * data-attachment-id>`, a dropped/absent one renders a `.attachment-missing`
 * span. That rendered difference is the observable for every attMeta assertion.
 */

const PNG_A = '11111111-1111-4111-8111-111111111111';
const PNG_B = '33333333-3333-4333-8333-333333333333';

type Result =
	| { status: 'ok'; mime: string; size: number }
	| { status: 'missing' }
	| { status: 'transient' };

const OK: Result = { status: 'ok', mime: 'image/png', size: 4096 };
const MISSING: Result = { status: 'missing' };

/**
 * The cache-sharing HEAD (`fetchAttachmentMetadata`) and the no-store existence
 * probe (`revalidateAttachmentMetadata`). Separate spies so a test can assert
 * which one a probe went through, and mock their results independently.
 */
const fetchImpl = vi.fn<(ws: string, uuid: string) => Promise<Result>>();
const revalidateImpl =
	vi.fn<
		(ws: string, uuid: string, url: unknown, opts: unknown) => Promise<Result>
	>();
const invalidateSpy = vi.fn<(ws: string, uuid: string) => void>();

vi.mock('$lib/components/editor/attachment-metadata', () => ({
	fetchAttachmentMetadata: (ws: string, uuid: string, _url: unknown) => fetchImpl(ws, uuid),
	revalidateAttachmentMetadata: (ws: string, uuid: string, url: unknown, opts: unknown) =>
		revalidateImpl(ws, uuid, url, opts),
	invalidateAttachmentMetadata: (ws: string, uuid: string) => invalidateSpy(ws, uuid),
}));

const timelineListMock = vi.fn<(ws: string, slug: string) => Promise<TimelineResponse>>();

vi.mock('$lib/api/client', () => ({
	api: {
		timeline: { list: (ws: string, slug: string) => timelineListMock(ws, slug) },
		comments: {
			create: vi.fn(),
			update: vi.fn(),
			delete: vi.fn(),
			addReaction: vi.fn(),
			removeReaction: vi.fn(),
		},
	},
}));

// A controllable SSE seam: capture the timeline's handler so a test can fire a
// comment event that drives its (debounced) reload — the realistic way the
// referenced-attachment set churns without a user editing comments by hand.
const sseBox = vi.hoisted(() => ({ handler: null as null | ((e: { type: string }) => void) }));
vi.mock('$lib/services/sse.svelte', () => ({
	sseService: {
		onItemEvent: (fn: (e: { type: string }) => void) => {
			sseBox.handler = fn;
			return () => {
				if (sseBox.handler === fn) sseBox.handler = null;
			};
		},
	},
}));

vi.mock('$lib/stores/auth.svelte', () => ({
	authStore: { userId: 'user-1', user: { id: 'user-1', role: 'member' } },
}));

vi.mock('$lib/stores/workspace.svelte', () => ({
	workspaceStore: { canEditItem: () => false },
}));

// Tiptap in jsdom is not what these tests are about; the composer is inert.
vi.mock('$lib/components/CommentEditor.svelte', async () => ({
	default: (await import('./fixtures/InertCommentEditor.svelte')).default,
}));

// The deletion bus is REAL — the announce must genuinely reach the timeline's
// registered listener (a spy-only mock would prove nothing about the wiring).
const { notifyAttachmentDeleted } = await import('$lib/attachments/events');
const { default: ItemTimeline } = await import('./ItemTimeline.svelte');

function deferred<T>() {
	let resolve!: (v: T) => void;
	const promise = new Promise<T>((r) => {
		resolve = r;
	});
	return { promise, resolve };
}

function comment(body: string, id = 'c1'): Comment {
	return {
		id,
		item_id: 'item-a',
		workspace_id: 'ws-1',
		author: 'alice',
		body,
		created_by: 'alice',
		source: 'web',
		created_at: '2026-01-01T00:00:00Z',
		updated_at: '2026-01-01T00:00:00Z',
	};
}

function entry(c: Comment): TimelineEntry {
	return {
		id: `e-${c.id}`,
		kind: 'comment',
		created_at: c.created_at,
		actor: 'alice',
		source: 'web',
		comment: c,
	};
}

function bodyWith(ids: string[]): string {
	return ids.map((id, i) => `![image ${i}](pad-attachment:${id})`).join('\n\n');
}

function respond(ids: string[]): TimelineResponse {
	return { entries: [entry(comment(bodyWith(ids)))], has_more: false };
}

let host: HTMLElement;
let app: Record<string, unknown> | null = null;

const props = $state<{
	wsSlug: string;
	username: string;
	itemSlug: string;
	currentContent: string;
	itemId: string;
	collectionId: string;
	hostToken: string;
	parentArchived: boolean;
}>({
	wsSlug: 'ws',
	username: 'alice',
	itemSlug: 'TASK-1',
	currentContent: '',
	itemId: 'item-a',
	collectionId: 'coll-1',
	hostToken: 'host-1',
	parentArchived: false,
});

function resetProps() {
	props.wsSlug = 'ws';
	props.username = 'alice';
	props.itemSlug = 'TASK-1';
	props.currentContent = '';
	props.itemId = 'item-a';
	props.collectionId = 'coll-1';
	props.hostToken = 'host-1';
	props.parentArchived = false;
}

function render() {
	app = mount(ItemTimeline, { target: host, props }) as Record<string, unknown>;
	return app;
}

/** Several microtask hops: list() → entries → probe → attMeta → re-render. */
async function settle() {
	for (let i = 0; i < 10; i++) {
		await tick();
		flushSync();
	}
}

function imgFor(id: string): HTMLElement | null {
	return host.querySelector<HTMLElement>(`img[data-attachment-id="${id}"]`);
}

function missingFor(id: string): HTMLElement | null {
	return host.querySelector<HTMLElement>(`span.attachment-missing[data-attachment-id="${id}"]`);
}

beforeEach(() => {
	host = document.createElement('div');
	document.body.appendChild(host);
	resetProps();
	fetchImpl.mockReset();
	revalidateImpl.mockReset();
	invalidateSpy.mockReset();
	timelineListMock.mockReset();
	fetchImpl.mockResolvedValue(OK);
	revalidateImpl.mockResolvedValue(OK);
	timelineListMock.mockResolvedValue({ entries: [], has_more: false });
	sseBox.handler = null;
});

afterEach(() => {
	if (app) unmount(app);
	app = null;
	host.remove();
});

describe('ItemTimeline — attMeta lifecycle reconciliation (TASK-2510)', () => {
	it('renders a resolved image, and a missing placeholder when unresolved (observable baseline)', async () => {
		timelineListMock.mockResolvedValue(respond([PNG_A, PNG_B]));
		fetchImpl.mockImplementation(async (_ws, uuid) => (uuid === PNG_A ? OK : MISSING));
		render();
		await settle();

		// The whole suite reads attMeta state off this render difference; if this
		// ever stops holding the assertions below are vacuous.
		expect(imgFor(PNG_A)).not.toBeNull();
		expect(imgFor(PNG_B)).toBeNull();
		expect(missingFor(PNG_B)).not.toBeNull();
	});

	describe('deletion bus', () => {
		it('drops a rendered attachment when the app-wide bus announces its deletion', async () => {
			// The bus is real: notifyAttachmentDeleted fans out to the timeline's
			// registered listener (TASK-2477 — a spy-only mock would never fire it).
			timelineListMock.mockResolvedValue(respond([PNG_A]));
			render();
			await settle();
			expect(imgFor(PNG_A)).not.toBeNull();

			notifyAttachmentDeleted(PNG_A);
			await settle();

			// COULD FAIL: without the deletion listener the <img> stays; the drop of
			// the attMeta entry re-renders the reference through the missing path.
			expect(imgFor(PNG_A)).toBeNull();
			expect(missingFor(PNG_A)).not.toBeNull();
		});

		it('a HEAD that resolves ok AFTER the delete does not repopulate attMeta (tombstone fence)', async () => {
			// The delete clears the caches, but it does NOT cancel the in-flight HEAD
			// (the probe writes its result unconditionally otherwise). The tombstone
			// covers the id even though it was never populated.
			const d = deferred<Result>();
			timelineListMock.mockResolvedValue(respond([PNG_A]));
			fetchImpl.mockReturnValue(d.promise);
			render();
			await settle();
			// The probe MUST have been dispatched — otherwise this test would pass
			// against a no-op probe effect for the wrong reason (Codex round-1).
			expect(fetchImpl).toHaveBeenCalledWith('ws', PNG_A);
			// Not yet resolved → still unresolved.
			expect(imgFor(PNG_A)).toBeNull();

			notifyAttachmentDeleted(PNG_A);
			await settle();

			// The delayed HEAD lands ok, post-delete.
			d.resolve(OK);
			await settle();

			// COULD FAIL: without the tombstone the delayed ok writes attMeta and the
			// <img> appears for a row the server no longer has.
			expect(imgFor(PNG_A)).toBeNull();
			expect(missingFor(PNG_A)).not.toBeNull();
		});

		it('deleting A does not fence B’s in-flight probe (tombstone is per-id, not a global epoch)', async () => {
			// The linchpin distinguishing the per-id tombstone from a blunt epoch: a
			// single deletion must NOT refuse the OTHER attachments' in-flight writes.
			// An implementation that bumped a shared epoch on delete would fence B and
			// pass every other test in this suite — only this one catches it.
			const dA = deferred<Result>();
			const dB = deferred<Result>();
			timelineListMock.mockResolvedValue(respond([PNG_A, PNG_B]));
			fetchImpl.mockImplementation(async (_ws, uuid) =>
				uuid === PNG_A ? dA.promise : dB.promise
			);
			render();
			await settle();
			expect(fetchImpl).toHaveBeenCalledWith('ws', PNG_A);
			expect(fetchImpl).toHaveBeenCalledWith('ws', PNG_B);

			// Delete A while BOTH probes are still in flight.
			notifyAttachmentDeleted(PNG_A);

			// B resolves ok AFTER the delete of A.
			dB.resolve(OK);
			await settle();
			// COULD FAIL: a global-epoch delete would fence B here.
			expect(imgFor(PNG_B)).not.toBeNull();

			// A resolves ok too — but A is tombstoned, so it stays suppressed.
			dA.resolve(OK);
			await settle();
			expect(imgFor(PNG_A)).toBeNull();
			expect(missingFor(PNG_A)).not.toBeNull();
		});
	});

	describe('archive edge (false→true)', () => {
		it('drops this item’s cached metadata and invalidates the shared cache', async () => {
			timelineListMock.mockResolvedValue(respond([PNG_A]));
			fetchImpl.mockResolvedValue(OK);
			render();
			await settle();
			expect(imgFor(PNG_A)).not.toBeNull();

			props.parentArchived = true;
			await settle();

			// A cached `ok` renders a plain <img> with no error bridge to the missing
			// presentation — so the archive edge drops the entry (→ missing) and
			// invalidates the shared cache so a re-render cannot rehydrate a stale ok.
			expect(imgFor(PNG_A)).toBeNull();
			expect(missingFor(PNG_A)).not.toBeNull();
			expect(invalidateSpy).toHaveBeenCalledWith('ws', PNG_A);
		});

		it('drops a resolved-then-unreferenced id so re-referencing it while archived shows missing, not a broken <img>', async () => {
			// Codex round-2, symmetric to P2 on the ARCHIVE side: an attachment
			// resolved `ok`, then unreferenced BEFORE the archive, keeps its cached
			// `ok` + probe mark. If the archive edge cleaned only the currently-
			// referenced ids, re-adding it while archived would skip the probe and
			// replay the stale `ok` as a broken <img>. The whole-tracked-set cleanup
			// drops it so the re-reference probes no-store → missing.
			vi.useFakeTimers();
			try {
				const fireReload = async (resp: TimelineResponse) => {
					timelineListMock.mockResolvedValue(resp);
					sseBox.handler?.({ type: 'comment_updated' });
					vi.advanceTimersByTime(600);
					for (let i = 0; i < 12; i++) {
						await Promise.resolve();
						flushSync();
					}
				};
				const settleFake = async () => {
					for (let i = 0; i < 12; i++) {
						await Promise.resolve();
						flushSync();
					}
				};

				// Live: PNG_A referenced and resolved ok.
				timelineListMock.mockResolvedValue(respond([PNG_A]));
				fetchImpl.mockResolvedValue(OK);
				render();
				await settleFake();
				expect(imgFor(PNG_A)).not.toBeNull();

				// Edited to DROP the reference, still live. attMeta[PNG_A]=ok lingers.
				await fireReload({
					entries: [entry(comment('no attachments now', 'c1'))],
					has_more: false,
				});
				expect(imgFor(PNG_A)).toBeNull();

				// ARCHIVE. PNG_A is not referenced at this instant, but its cached ok
				// must be dropped AND its probe mark cleared / cache invalidated so it
				// can't repaint as a broken image later.
				invalidateSpy.mockClear();
				revalidateImpl.mockResolvedValue(MISSING); // archived → 404
				props.parentArchived = true;
				await settleFake();
				// The archive edge reconciled the historical (unreferenced) id, not only
				// the on-screen ones — proves whole-tracked-set cleanup rather than a
				// bare wholesale attMeta clear that leaves `probed` set.
				expect(invalidateSpy).toHaveBeenCalledWith('ws', PNG_A);

				// Re-add PNG_A while archived.
				await fireReload(respond([PNG_A]));

				// COULD FAIL (pre-fix): the lingering cached ok + probe mark would
				// render a broken <img>; the whole-set archive cleanup shows missing.
				expect(imgFor(PNG_A)).toBeNull();
				expect(missingFor(PNG_A)).not.toBeNull();
			} finally {
				vi.useRealTimers();
			}
		});

		it('a pre-archive ok that resolves after the edge refuses to write (epoch fence)', async () => {
			// The in-flight HEAD captured the pre-archive epoch; revalidate invalidates
			// the cache but can't cancel the promise, so the epoch fence is what stops
			// a pre-archive ok from repainting after the parent went archived.
			const d = deferred<Result>();
			timelineListMock.mockResolvedValue(respond([PNG_A]));
			fetchImpl.mockReturnValue(d.promise);
			render();
			await settle();
			expect(imgFor(PNG_A)).toBeNull();

			props.parentArchived = true;
			await settle();

			d.resolve(OK); // lands post-archive
			await settle();

			// COULD FAIL: without the epoch fence the delayed ok writes attMeta and
			// paints a broken <img> against the archived parent.
			expect(imgFor(PNG_A)).toBeNull();
			expect(missingFor(PNG_A)).not.toBeNull();
		});

		it('a re-render against the now-archived parent does not rehydrate a stale ok (no-store)', async () => {
			// archive→remount: the parent is archived, the shared cache would still
			// answer `ok` (fetch), but a genuine probe must go no-store and see the
			// 404. Simulated by fetch→ok (stale cache) vs revalidate→missing (server).
			timelineListMock.mockResolvedValue(respond([PNG_A]));
			fetchImpl.mockResolvedValue(OK);
			revalidateImpl.mockResolvedValue(MISSING);
			render();
			await settle();
			expect(imgFor(PNG_A)).not.toBeNull(); // live: cached ok painted it

			props.parentArchived = true;
			await settle();
			expect(imgFor(PNG_A)).toBeNull(); // archive edge dropped it

			// Remount the timeline against the archived parent (a card/pane remount).
			unmount(app!);
			revalidateImpl.mockClear();
			fetchImpl.mockClear();
			app = mount(ItemTimeline, { target: host, props }) as Record<string, unknown>;
			await settle();

			// COULD FAIL: a cached fetch would rehydrate the stale ok and paint a
			// broken <img>; the LEVEL no-store probe sees the 404 → missing.
			expect(imgFor(PNG_A)).toBeNull();
			expect(missingFor(PNG_A)).not.toBeNull();
			expect(revalidateImpl).toHaveBeenCalledWith('ws', PNG_A, expect.anything(), {
				cache: 'no-store',
			});
			expect(fetchImpl).not.toHaveBeenCalled();
		});
	});

	describe('archived is a LEVEL, not only an edge', () => {
		it('mounting archived with a stale cached ok presents missing, not a broken <img>', async () => {
			// No edge fires on a mount that is already archived — the LEVEL rule is
			// what routes the probe through no-store so the stale cached ok can't
			// repaint. fetch→ok (stale cache), revalidate→missing (archived 404).
			props.parentArchived = true;
			timelineListMock.mockResolvedValue(respond([PNG_A]));
			fetchImpl.mockResolvedValue(OK);
			revalidateImpl.mockResolvedValue(MISSING);
			render();
			await settle();

			expect(imgFor(PNG_A)).toBeNull();
			expect(missingFor(PNG_A)).not.toBeNull();
			expect(revalidateImpl).toHaveBeenCalledWith('ws', PNG_A, expect.anything(), {
				cache: 'no-store',
			});
			// The cached HEAD must never be consulted while archived.
			expect(fetchImpl).not.toHaveBeenCalled();
			// ...and NO archive EDGE fired: a mount that is already archived is a
			// LEVEL, so the edge-only shared-cache invalidation must not run.
			expect(invalidateSpy).not.toHaveBeenCalled();
		});

		it('an entry arriving while archived probes no-store', async () => {
			// The entry arrives via the post-mount async load while parentArchived is
			// already true — the probe that fires in response must be no-store.
			props.parentArchived = true;
			timelineListMock.mockResolvedValue(respond([PNG_A]));
			fetchImpl.mockResolvedValue(OK);
			revalidateImpl.mockResolvedValue(MISSING);
			render();
			await settle();

			expect(revalidateImpl).toHaveBeenCalledWith('ws', PNG_A, expect.anything(), {
				cache: 'no-store',
			});
			expect(fetchImpl).not.toHaveBeenCalled();
		});
	});

	describe('restore edge (true→false)', () => {
		it('re-probes no-store so a restore escapes a missing cached while archived', async () => {
			// Mount archived: the row reads missing (server 404s the archived parent),
			// and that missing is what the shared cache would hold. fetch (cached)
			// stays missing to model the stale cache; revalidate (no-store) tracks the
			// live server, which returns ok once restored.
			props.parentArchived = true;
			timelineListMock.mockResolvedValue(respond([PNG_A]));
			fetchImpl.mockResolvedValue(MISSING);
			revalidateImpl.mockResolvedValueOnce(MISSING); // archived probe
			revalidateImpl.mockResolvedValue(OK); // restored server
			render();
			await settle();
			expect(imgFor(PNG_A)).toBeNull();
			expect(missingFor(PNG_A)).not.toBeNull();

			props.parentArchived = false;
			await settle();

			// COULD FAIL: a restore that re-probed through the cache would keep seeing
			// the cached missing (fetch→MISSING) and the image would never come back;
			// the no-store re-probe escapes it.
			expect(imgFor(PNG_A)).not.toBeNull();
			expect(fetchImpl).not.toHaveBeenCalled();
			expect(revalidateImpl).toHaveBeenLastCalledWith('ws', PNG_A, expect.anything(), {
				cache: 'no-store',
			});
		});

		it('a pre-restore missing that resolves after the edge refuses to overwrite the restored ok (epoch fence)', async () => {
			// A probe dispatched while archived (→ missing) can land AFTER the restore
			// re-probe already wrote ok. Without the epoch fence its authoritative
			// missing would clear the now-correct attMeta entry.
			const archivedProbe = deferred<Result>();
			props.parentArchived = true;
			timelineListMock.mockResolvedValue(respond([PNG_A]));
			fetchImpl.mockResolvedValue(MISSING);
			revalidateImpl.mockReturnValueOnce(archivedProbe.promise); // archived, deferred
			revalidateImpl.mockResolvedValue(OK); // restore re-probe, immediate
			render();
			await settle();
			expect(imgFor(PNG_A)).toBeNull();

			props.parentArchived = false;
			await settle();
			// The restore re-probe resolved ok first.
			expect(imgFor(PNG_A)).not.toBeNull();

			// Now the stale archived probe lands missing, post-restore.
			archivedProbe.resolve(MISSING);
			await settle();

			// COULD FAIL: without the epoch fence the delayed missing clears attMeta
			// and the restored image vanishes again.
			expect(imgFor(PNG_A)).not.toBeNull();
			expect(missingFor(PNG_A)).toBeNull();
		});

		it('re-probes an id that went missing while archived, was unreferenced, then re-referenced after restore (P2)', async () => {
			// Codex round-1 P2: `probed` is cleared on restore only for the WHOLE
			// unresolved set, not just the currently-referenced ids. Otherwise an id
			// that probed `missing` while archived, then lost its reference before the
			// restore, keeps a stale `probed` mark and SKIPS the probe when re-added
			// afterward — stuck missing forever.
			vi.useFakeTimers();
			try {
				// Reload arrives via the timeline's own (debounced) SSE refresh.
				const fireReload = async (resp: TimelineResponse) => {
					timelineListMock.mockResolvedValue(resp);
					sseBox.handler?.({ type: 'comment_updated' });
					vi.advanceTimersByTime(600);
					for (let i = 0; i < 12; i++) {
						await Promise.resolve();
						flushSync();
					}
				};
				const settleFake = async () => {
					for (let i = 0; i < 12; i++) {
						await Promise.resolve();
						flushSync();
					}
				};

				props.parentArchived = true;
				timelineListMock.mockResolvedValue(respond([PNG_A]));
				revalidateImpl.mockResolvedValue(MISSING); // archived → 404
				render();
				await settleFake();
				expect(missingFor(PNG_A)).not.toBeNull();

				// The comment is edited to DROP the PNG_A reference, while still archived.
				await fireReload({
					entries: [entry(comment('no attachments now', 'c1'))],
					has_more: false,
				});
				expect(imgFor(PNG_A)).toBeNull();
				expect(missingFor(PNG_A)).toBeNull(); // not referenced at all

				// RESTORE. PNG_A is not referenced right now — but its probe mark must
				// still be cleared AND its shared-cache entry invalidated so a later
				// re-reference re-probes fresh rather than replaying a cached `missing`.
				invalidateSpy.mockClear();
				revalidateImpl.mockResolvedValue(OK);
				fetchImpl.mockResolvedValue(OK);
				props.parentArchived = false;
				await settleFake();
				// The restore reconciled the unreferenced id's cache, not only the
				// on-screen ones (proves whole-tracked-set invalidation, not just the
				// stale-mark clear).
				expect(invalidateSpy).toHaveBeenCalledWith('ws', PNG_A);

				// The comment is edited AGAIN to RE-ADD the PNG_A reference.
				await fireReload(respond([PNG_A]));

				// COULD FAIL (pre-P2-fix): the stale `probed` mark skips the re-probe and
				// PNG_A renders missing forever; the broadened restore clearing re-probes
				// it and the image returns.
				expect(imgFor(PNG_A)).not.toBeNull();
			} finally {
				vi.useRealTimers();
			}
		});
	});
});
