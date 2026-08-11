import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { flushSync } from 'svelte';

// The metadata machine's two probes. Mocking them lets these tests assert WHICH
// path a given transition takes — the forced no-store revalidation vs the plain
// seed-fill fetch — and how many times, which is exactly the T6
// always-revalidate-on-open contract (PLAN-2392 3c-ii).
//
// WHY THIS FILE (not just Lightbox.svelte.test.ts): the Lightbox host remounts
// the machine via `{#key request}` on every open, so a Lightbox-level "reopen"
// test cannot distinguish "the nonce forced a second probe" from "the remount
// created a fresh machine that forced". Driving `createSurfaceMetadata` directly
// — one machine, a mutable `openNonce` — is the only place the nonce mechanism
// is load-bearing and therefore falsifiable.
const metaFetch = vi.hoisted(() => vi.fn());
const metaRevalidate = vi.hoisted(() => vi.fn());
vi.mock('$lib/components/editor/attachment-metadata', () => ({
	fetchAttachmentMetadata: (...a: unknown[]) => metaFetch(...a),
	revalidateAttachmentMetadata: (...a: unknown[]) => metaRevalidate(...a),
	invalidateAttachmentMetadata: vi.fn(),
}));

import type { SurfaceMetadata } from './surfaceMetadata.svelte';

const { createSurfaceMetadata } = await import('./surfaceMetadata.svelte');

const ATT_A = 'aaaaaaaa-1111-4111-8111-aaaaaaaaaaaa';
const ATT_B = 'bbbbbbbb-2222-4222-8222-bbbbbbbbbbbb';

interface Addr {
	ws: string;
	att: string;
	openNonce: number;
	seedMime: string | null;
	seedSize: number | null;
	parentArchived: boolean;
	revalidateToken: number;
	open: boolean;
}

const roots: Array<() => void> = [];

/**
 * Build a machine over a MUTABLE address, inside an `$effect.root` so its
 * `$effect` runs (and can be torn down). Returns the reactive address to mutate
 * and the machine to read. Complete seed by default (MIME + size) so the only
 * thing that ever provokes a probe is the T6 forcing under test — never a
 * seed-fill.
 */
function harness(initial: Partial<Addr> = {}) {
	const addr = $state<Addr>({
		ws: 'ws',
		att: ATT_A,
		openNonce: 1,
		seedMime: 'image/png',
		seedSize: 2048,
		parentArchived: false,
		revalidateToken: 0,
		open: true,
		...initial,
	});
	let meta!: SurfaceMetadata;
	const dispose = $effect.root(() => {
		meta = createSurfaceMetadata(() => ({
			ws: addr.ws,
			attachmentId: addr.att,
			seed: { filename: null, mime_type: addr.seedMime, size_bytes: addr.seedSize },
			open: addr.open,
			parentArchived: addr.parentArchived,
			revalidateToken: addr.revalidateToken,
			openNonce: addr.openNonce,
		}));
	});
	roots.push(dispose);
	flushSync();
	return { addr, meta };
}

/** Drain the metadata machine's async probe continuations, flushing effects between. */
async function settle() {
	for (let i = 0; i < 6; i++) {
		await Promise.resolve();
		flushSync();
	}
}

beforeEach(() => {
	metaFetch.mockReset();
	metaRevalidate.mockReset();
	metaFetch.mockResolvedValue({ status: 'ok', mime: 'image/png', size: 2048 });
	metaRevalidate.mockResolvedValue({ status: 'ok', mime: 'image/png', size: 2048 });
});

afterEach(() => {
	while (roots.length) roots.pop()!();
});

describe('surfaceMetadata — always-revalidate-per-(open, entry) nonce (T6 + 3c-iii U3)', () => {
	it('a complete-seed open forces exactly ONE no-store revalidation (never the plain fetch)', () => {
		harness();
		// The old fast path would have short-circuited a complete seed with zero
		// probes; T6 forces one, and it is the revalidation with `cache: 'no-store'`.
		expect(metaRevalidate).toHaveBeenCalledTimes(1);
		expect(metaFetch).not.toHaveBeenCalled();
		expect(metaRevalidate.mock.calls[0][3]).toEqual({ cache: 'no-store' });
	});

	it('a fresh nonce on the SAME (ws, att) forces a SECOND no-store revalidation — WITHOUT a remount', () => {
		const { addr } = harness();
		expect(metaRevalidate).toHaveBeenCalledTimes(1);

		// The host mints a new nonce on reopen. There is NO remount here — the same
		// machine sees a nonce it has not forced for, and must probe again. This is
		// the property a `{#key}` remount would mask; if the nonce were ignored (or
		// seeded from the incoming value), the count would stay 1 and this fails.
		addr.openNonce = 2;
		flushSync();
		expect(metaRevalidate).toHaveBeenCalledTimes(2);
		expect(metaRevalidate.mock.calls[1][3]).toEqual({ cache: 'no-store' });
		expect(metaFetch).not.toHaveBeenCalled();
	});

	it('navigating to a fresh sibling forces a SECOND no-store revalidation (U3: per navigation step, even a complete seed)', async () => {
		// INVERTS the T6-era expectation. T6 forced only the OPENED entry; U3
		// generalizes to one forced probe per (nonce, attachment) pair, so arrowing to
		// a not-yet-probed sibling revalidates it too — a complete seed no longer
		// short-circuits a navigated-to entry, and the probe is the no-store
		// revalidation, never the plain (cacheable) fetch.
		const { addr } = harness();
		await settle();
		expect(metaRevalidate).toHaveBeenCalledTimes(1);

		addr.att = ATT_B;
		flushSync();
		expect(metaRevalidate).toHaveBeenCalledTimes(2);
		expect(metaRevalidate.mock.calls[1][3]).toEqual({ cache: 'no-store' });
		expect(metaFetch).not.toHaveBeenCalled();
	});

	it('navigating to an INCOMPLETE sibling ALSO takes the forced revalidation, not the plain fetch (U3)', async () => {
		// INVERTS the T6-era expectation that a navigated-to entry took the plain
		// seed-fill fetch. Under U3 an unseen pair forces regardless of seed
		// completeness: the reachability question owns the step, so it is the no-store
		// revalidation. A null-size seed does not downgrade it to the cacheable HEAD.
		const { addr } = harness();
		await settle();
		expect(metaRevalidate).toHaveBeenCalledTimes(1);

		addr.att = ATT_B;
		addr.seedSize = null;
		flushSync();
		expect(metaRevalidate).toHaveBeenCalledTimes(2);
		expect(metaFetch).not.toHaveBeenCalled();
	});

	it('arrowing BACK to an already-probed entry within the same open does NOT re-probe (one automatic probe per pair)', async () => {
		// The complement of the two inversions above: a pair is probed once per open.
		// A→B→A: A and B each force once on first sight; arrowing back to A (already
		// recorded under this nonce) takes the fast path — no third probe.
		const { addr } = harness();
		await settle(); // A's open probe completes → A recorded
		expect(metaRevalidate).toHaveBeenCalledTimes(1);

		addr.att = ATT_B;
		flushSync();
		await settle(); // B's probe completes → B recorded
		expect(metaRevalidate).toHaveBeenCalledTimes(2);

		addr.att = ATT_A;
		flushSync();
		await settle();
		// A was already recorded → its complete seed short-circuits. No re-probe.
		expect(metaRevalidate).toHaveBeenCalledTimes(2);
		expect(metaFetch).not.toHaveBeenCalled();
	});

	it('records the pair on COMPLETION, not dispatch: a delayed probe discarded stale leaves it unseen → arrow-back re-probes', async () => {
		// round-2 P1. Marking at dispatch would let A→B, arrow-away-before-B-resolves,
		// arrow-back-to-B take B's complete-seed fast path — painting a maybe-deleted B
		// live. B is recorded ONLY when its forced probe resolves non-stale; a
		// stale-discarded probe leaves B unseen so a later arrow-back re-forces.
		const { addr } = harness();
		await settle(); // A recorded
		expect(metaRevalidate).toHaveBeenCalledTimes(1);

		// Arrow to B, holding B's probe on a DEFERRED we resolve only after navigating
		// away — so its continuation runs against a moved-on subject and hits the
		// `req.stale()` guard at the mark site (exercising it, not just skipping it).
		let resolveB!: (v: { status: 'ok'; mime: string; size: number }) => void;
		metaRevalidate.mockReturnValueOnce(new Promise((r) => (resolveB = r)));
		addr.att = ATT_B;
		flushSync();
		expect(metaRevalidate).toHaveBeenCalledTimes(2); // B forced (U3), unresolved

		// Arrow back to A before B resolved.
		addr.att = ATT_A;
		flushSync();
		// NOW resolve B's probe: its continuation runs but the subject is A, so
		// `req.stale()` is true and the pair must NOT be recorded. A mark-before-the-
		// stale-guard regression would record B here and the final re-probe would vanish.
		resolveB({ status: 'ok', mime: 'image/png', size: 2048 });
		await settle();
		expect(metaRevalidate).toHaveBeenCalledTimes(2); // A already recorded → no probe

		// Arrow to B again: B is STILL unseen (its earlier probe was stale-discarded), so
		// a fresh automatic probe fires. Mark-at-dispatch — or marking before the stale
		// guard — would suppress this (stay at 2).
		metaRevalidate.mockResolvedValue({ status: 'ok', mime: 'image/png', size: 2048 });
		addr.att = ATT_B;
		flushSync();
		expect(metaRevalidate).toHaveBeenCalledTimes(3);
	});

	it('a completed Retry probe does NOT record the pair → a later arrow-back still gets its one automatic probe (round-4 P2)', async () => {
		// forcedFor records AUTOMATIC probes only. B's automatic probe is held pending
		// (never records B); a Retry on B then completes but, being the reload path,
		// must NOT record B either — so arrowing back to B still fires the automatic
		// probe. If Retry (wrongly) recorded B, the final arrow-back would find B seen
		// and skip, leaving the count one short.
		const { addr, meta } = harness();
		await settle(); // A recorded
		metaRevalidate.mockReset();

		// Arrow to B; hold its AUTOMATIC probe pending so it cannot record B.
		metaRevalidate.mockReturnValueOnce(new Promise<never>(() => {}));
		addr.att = ATT_B;
		flushSync();
		expect(metaRevalidate).toHaveBeenCalledTimes(1); // B automatic (pending)

		// Retry on B resolves via the reload path — records nothing (automatic-only).
		metaRevalidate.mockResolvedValue({ status: 'ok', mime: 'image/png', size: 2048 });
		meta.retry();
		flushSync();
		await settle();
		expect(metaRevalidate).toHaveBeenCalledTimes(2); // the Retry probe

		// Arrow away to A (recorded → no probe), then back to B: B is still unseen
		// (only a Retry ever completed for it), so the automatic probe fires.
		addr.att = ATT_A;
		flushSync();
		await settle();
		addr.att = ATT_B;
		flushSync();
		await settle();
		expect(metaRevalidate).toHaveBeenCalledTimes(3);
	});

	it('does not burn the nonce while CLOSED even with an addressable attachment — forces once it opens', () => {
		// The dangerous case: `open` is false but the attachment id is already set, so
		// the subject IS addressable (a non-null key). The pair must NOT be recorded
		// here — no probe runs while closed — or a later real open on the SAME nonce
		// would take the complete-seed fast path and skip the always-revalidate probe.
		// (Reachable only if a consumer decouples `open` from a present attachment; the
		// gate is `isOpen && req.key !== null`, matching the probe's own precondition.)
		const { addr } = harness({ att: ATT_A, open: false, openNonce: 7 });
		expect(metaRevalidate).not.toHaveBeenCalled();
		expect(metaFetch).not.toHaveBeenCalled();

		// Now it opens — same nonce, complete seed — and the forced probe fires.
		addr.open = true;
		flushSync();
		expect(metaRevalidate).toHaveBeenCalledTimes(1);
		expect(metaRevalidate.mock.calls[0][3]).toEqual({ cache: 'no-store' });
	});

	it('also does not burn the nonce while the subject is UN-addressable (no attachment yet)', () => {
		// The sibling case: neither open nor addressable. Resolving the entry forces
		// the opened entry on the same nonce.
		const { addr } = harness({ att: '', open: false, openNonce: 3 });
		expect(metaRevalidate).not.toHaveBeenCalled();

		addr.att = ATT_A;
		addr.open = true;
		flushSync();
		expect(metaRevalidate).toHaveBeenCalledTimes(1);
	});
});
