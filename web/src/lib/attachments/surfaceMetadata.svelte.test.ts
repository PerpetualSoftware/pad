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
	const dispose = $effect.root(() => {
		createSurfaceMetadata(() => ({
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
	return addr;
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

describe('surfaceMetadata — always-revalidate-on-open nonce (T6)', () => {
	it('a complete-seed open forces exactly ONE no-store revalidation (never the plain fetch)', () => {
		harness();
		// The old fast path would have short-circuited a complete seed with zero
		// probes; T6 forces one, and it is the revalidation with `cache: 'no-store'`.
		expect(metaRevalidate).toHaveBeenCalledTimes(1);
		expect(metaFetch).not.toHaveBeenCalled();
		expect(metaRevalidate.mock.calls[0][3]).toEqual({ cache: 'no-store' });
	});

	it('a fresh nonce on the SAME (ws, att) forces a SECOND no-store revalidation — WITHOUT a remount', () => {
		const addr = harness();
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

	it('navigating to another attachment keeps the nonce → NO additional forced probe (a complete sibling probes not at all)', () => {
		const addr = harness();
		expect(metaRevalidate).toHaveBeenCalledTimes(1);

		// Arrow to a complete-seed sibling: a subject change, but the nonce is
		// unchanged, so nothing forces and the complete seed short-circuits — zero
		// new probes of either kind.
		addr.att = ATT_B;
		flushSync();
		expect(metaRevalidate).toHaveBeenCalledTimes(1);
		expect(metaFetch).not.toHaveBeenCalled();
	});

	it('navigating to an INCOMPLETE sibling uses the plain fetch, not a forced revalidation', () => {
		const addr = harness();
		expect(metaRevalidate).toHaveBeenCalledTimes(1);

		// The sibling has a null size → it needs a seed-fill, but navigation does NOT
		// force: it takes the plain (cacheable) HEAD, and the forced-probe count stays
		// at the single open. This is what proves the nonce is not consumed per step.
		addr.att = ATT_B;
		addr.seedSize = null;
		flushSync();
		expect(metaFetch).toHaveBeenCalledTimes(1);
		expect(metaRevalidate).toHaveBeenCalledTimes(1); // still just the open's
	});

	it('does not burn the nonce while CLOSED even with an addressable attachment — forces once it opens', () => {
		// The dangerous case: `open` is false but the attachment id is already set, so
		// the subject IS addressable (a non-null key). The nonce must NOT be consumed
		// here — no probe runs while closed — or a later real open on the SAME nonce
		// would take the complete-seed fast path and skip the always-revalidate probe.
		// (Reachable only if a consumer decouples `open` from a present attachment; the
		// gate is `isOpen && req.key !== null`, matching the probe's own precondition.)
		const addr = harness({ att: ATT_A, open: false, openNonce: 7 });
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
		const addr = harness({ att: '', open: false, openNonce: 3 });
		expect(metaRevalidate).not.toHaveBeenCalled();

		addr.att = ATT_A;
		addr.open = true;
		flushSync();
		expect(metaRevalidate).toHaveBeenCalledTimes(1);
	});
});
