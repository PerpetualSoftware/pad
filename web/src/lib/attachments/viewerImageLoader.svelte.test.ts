import { describe, it, expect } from 'vitest';
import { createViewerImageLoader } from './viewerImageLoader.svelte';
import type { LightboxImage } from './events';

// TASK-2459 — the DR-5b loader. `displaySrc` IS the request: the canonical
// attachment URL the viewer's <img> loads natively (a `?variant=thumb-md` first,
// the plain original second). The acceptance is phrased in requests — a DR-16
// gate is "no request issued", the fallback detector is "no SECOND request", the
// upgrade is "the second request is the original" — and each is a `displaySrc`
// transition here.
//
// `decoded`/`errored` carry the `gen` (the `loadToken` the reporting element was
// mounted under); the current generation is `loader.loadToken`, so a live decode
// passes `loader.loadToken` and a DETACHED element's stale decode passes the
// token captured when it loaded.

function image(id: string, over: Partial<LightboxImage> = {}): LightboxImage {
	return {
		id,
		alt: id,
		filename: null,
		mime_type: 'image/png',
		size_bytes: null,
		width: null,
		height: null,
		...over,
	};
}

const THUMB = (id: string) => `/api/v1/workspaces/ws/attachments/${id}?variant=thumb-md`;
const ORIGINAL = (id: string) => `/api/v1/workspaces/ws/attachments/${id}`;

describe('viewerImageLoader — the decision table as requests (TASK-2459)', () => {
	it('small, long edge <= 1024: ONE request, the original directly (no variant)', () => {
		const loader = createViewerImageLoader();
		loader.load(image('A', { width: 800, height: 600 }), 'ws', 'desktop');
		expect(loader.displaySrc).toBe(ORIGINAL('A'));
		expect(loader.phase).toBe('loading');
		loader.decoded(800, 600, loader.displaySrc, loader.loadToken);
		// No upgrade — it IS the original.
		expect(loader.displaySrc).toBe(ORIGINAL('A'));
		expect(loader.phase).toBe('ready');
	});

	it('unknown dims on desktop: the original directly (one request)', () => {
		const loader = createViewerImageLoader();
		loader.load(image('A', { width: null, height: 900 }), 'ws', 'desktop');
		expect(loader.displaySrc).toBe(ORIGINAL('A'));
		loader.decoded(600, 900, loader.displaySrc, loader.loadToken);
		expect(loader.displaySrc).toBe(ORIGINAL('A'));
	});

	it('large / unknown on mobile: NO request — `deferred`, the tap affordance (TASK-2460)', () => {
		const large = createViewerImageLoader();
		large.load(image('A', { width: 5000, height: 5000 }), 'ws', 'mobile');
		expect(large.displaySrc).toBe(''); // no request issued
		expect(large.phase).toBe('deferred'); // NOT 'idle' — the original is on tap

		const unknown = createViewerImageLoader();
		unknown.load(image('B', { width: null, height: 900 }), 'ws', 'mobile');
		expect(unknown.displaySrc).toBe('');
		expect(unknown.phase).toBe('deferred');
	});
});

describe('viewerImageLoader — mobile on-demand original (TASK-2460)', () => {
	it('deferred cell: loadOriginal (tap) requests the ORIGINAL directly, once', () => {
		const loader = createViewerImageLoader();
		loader.load(image('A', { width: 5000, height: 5000 }), 'ws', 'mobile');
		expect(loader.phase).toBe('deferred');
		expect(loader.displaySrc).toBe('');

		loader.loadOriginal(); // the tap
		expect(loader.displaySrc).toBe(ORIGINAL('A'));
		expect(loader.phase).toBe('loading');

		// A second trigger (a second tap, or a zoom-past-fit racing it) is a no-op.
		const t = loader.loadToken;
		loader.loadOriginal();
		expect(loader.displaySrc).toBe(ORIGINAL('A'));
		expect(loader.loadToken).toBe(t);

		loader.decoded(5000, 5000, loader.displaySrc, loader.loadToken);
		expect(loader.phase).toBe('ready');
	});

	it('mobile thumb cell: thumb paints, then loadOriginal (zoom-past-fit) upgrades once', () => {
		const loader = createViewerImageLoader();
		loader.load(image('A', { width: 2000, height: 100 }), 'ws', 'mobile');
		expect(loader.displaySrc).toBe(THUMB('A')); // thumb painted, no auto-upgrade
		const thumbToken = loader.loadToken;
		loader.decoded(1024, 51, loader.displaySrc, loader.loadToken);
		expect(loader.displaySrc).toBe(THUMB('A')); // mobile: still the thumb

		loader.loadOriginal(); // zoom past fit
		expect(loader.displaySrc).toBe(ORIGINAL('A'));
		// SAME element reused (token unchanged) so the thumb stays until the original
		// decodes — no flash.
		expect(loader.loadToken).toBe(thumbToken);

		// A second zoom step past fit does not re-request.
		loader.loadOriginal();
		expect(loader.displaySrc).toBe(ORIGINAL('A'));
		expect(loader.loadToken).toBe(thumbToken);
	});

	it('mobile thumb cell whose thumb-md SERVED THE ORIGINAL issues no zoom re-fetch', () => {
		// The fallback detector must run on the mobile path too: a fresh upload /
		// WebP / AVIF has no derived thumb, so `?variant=thumb-md` returns the
		// ORIGINAL. Without clearing `originalDeferred`, a later zoom-past-fit would
		// download and decode the original a SECOND time.
		const loader = createViewerImageLoader();
		loader.load(image('A', { width: 2000, height: 100 }), 'ws', 'mobile');
		expect(loader.displaySrc).toBe(THUMB('A'));
		// The "thumb" decoded ABOVE the bound → it WAS the original.
		loader.decoded(2000, 100, loader.displaySrc, loader.loadToken);
		expect(loader.phase).toBe('ready');
		// Zoom past fit now finds nothing deferred — no second request.
		loader.loadOriginal();
		expect(loader.displaySrc).toBe(THUMB('A')); // unchanged
	});

	it('desktop never defers: loadOriginal is a no-op (auto-upgrade owns the original)', () => {
		const loader = createViewerImageLoader();
		loader.load(image('A', { width: 5000, height: 5000 }), 'ws', 'desktop');
		expect(loader.displaySrc).toBe(THUMB('A'));
		const before = loader.displaySrc;
		loader.loadOriginal(); // no-op on desktop
		expect(loader.displaySrc).toBe(before);
	});

	it('retry after an on-demand original FAILS re-requests the original, not the affordance', () => {
		const loader = createViewerImageLoader();
		loader.load(image('A', { width: 5000, height: 5000 }), 'ws', 'mobile');
		loader.loadOriginal(); // tap
		expect(loader.displaySrc).toBe(ORIGINAL('A'));
		loader.errored(loader.displaySrc, loader.loadToken);
		expect(loader.phase).toBe('error');

		const t = loader.loadToken;
		loader.retry();
		// Back to the ORIGINAL (a fresh token remounts to refetch) — NOT reverted to
		// the 'deferred' tap affordance.
		expect(loader.phase).toBe('loading');
		expect(loader.displaySrc).toBe(ORIGINAL('A'));
		expect(loader.loadToken).toBeGreaterThan(t);
	});

	it('retry after the INITIAL thumb fails re-requests the thumb (start path)', () => {
		const loader = createViewerImageLoader();
		loader.load(image('A', { width: 2000, height: 100 }), 'ws', 'mobile');
		expect(loader.displaySrc).toBe(THUMB('A'));
		loader.errored(loader.displaySrc, loader.loadToken);
		expect(loader.phase).toBe('error');
		loader.retry();
		expect(loader.displaySrc).toBe(THUMB('A')); // the initial policy, re-run
	});
});

describe('viewerImageLoader — thumb then original, the four bound ways (TASK-2459)', () => {
	it('large on desktop: first request bounded, second the original, the bitmap CHANGES', () => {
		const loader = createViewerImageLoader();
		loader.load(image('A', { width: 5000, height: 5000 }), 'ws', 'desktop');
		// (1) first response bounded.
		expect(loader.displaySrc).toBe(THUMB('A'));

		// The thumb decoded at a bounded size → the background upgrade fires.
		loader.decoded(1024, 768, loader.displaySrc, loader.loadToken);
		// (2) second request is explicitly the original (canonical, no variant).
		expect(loader.displaySrc).toBe(ORIGINAL('A'));
		// (3) the displayed bitmap visibly CHANGES (thumb URL → original URL).
		expect(loader.displaySrc).not.toBe(THUMB('A'));

		loader.decoded(5000, 5000, loader.displaySrc, loader.loadToken);
		expect(loader.phase).toBe('ready');
	});

	it('small, long edge > 1024 on desktop: thumb-md then upgrade; on mobile: thumb-md, no upgrade', () => {
		const desktop = createViewerImageLoader();
		desktop.load(image('A', { width: 2000, height: 100 }), 'ws', 'desktop');
		expect(desktop.displaySrc).toBe(THUMB('A'));
		desktop.decoded(1024, 51, desktop.displaySrc, desktop.loadToken);
		expect(desktop.displaySrc).toBe(ORIGINAL('A'));

		const mobile = createViewerImageLoader();
		mobile.load(image('A', { width: 2000, height: 100 }), 'ws', 'mobile');
		expect(mobile.displaySrc).toBe(THUMB('A'));
		mobile.decoded(1024, 51, mobile.displaySrc, mobile.loadToken);
		expect(mobile.displaySrc).toBe(THUMB('A')); // NO auto upgrade on mobile
	});

	it('(4) the FALLBACK case issues NO second request: a thumb-md served the original', () => {
		const loader = createViewerImageLoader();
		loader.load(image('A', { width: 5000, height: 5000 }), 'ws', 'desktop');
		expect(loader.displaySrc).toBe(THUMB('A'));
		// The "thumb" decoded ABOVE the thumbnail bound → it WAS the original.
		loader.decoded(5000, 5000, loader.displaySrc, loader.loadToken);
		expect(loader.displaySrc).toBe(THUMB('A')); // never upgraded — no double decode
		expect(loader.phase).toBe('ready');
	});
});

describe('viewerImageLoader — DR-16 as a LOADING gate (TASK-2459)', () => {
	it('issues NO request for an unsafe MIME', () => {
		const loader = createViewerImageLoader();
		loader.load(image('A', { mime_type: 'image/svg+xml', width: 800, height: 600 }), 'ws', 'desktop');
		expect(loader.displaySrc).toBe('');
		expect(loader.phase).toBe('idle');
	});

	it('issues NO request for an unresolved MIME', () => {
		const loader = createViewerImageLoader();
		loader.load(image('A', { mime_type: null, width: 800, height: 600 }), 'ws', 'desktop');
		expect(loader.displaySrc).toBe('');
	});

	it('issues NO request for no image', () => {
		const loader = createViewerImageLoader();
		loader.load(undefined, 'ws', 'desktop');
		expect(loader.displaySrc).toBe('');
	});
});

describe('viewerImageLoader — staleness / abort on navigate + shrink (TASK-2459)', () => {
	it('repointing DROPS the old URL immediately (abort by src reassignment)', () => {
		const loader = createViewerImageLoader();
		loader.load(image('A', { width: 5000, height: 5000 }), 'ws', 'desktop');
		expect(loader.displaySrc).toBe(THUMB('A'));
		// Navigate to B before A finishes: the old URL is gone at once.
		loader.load(image('B', { width: 800, height: 600 }), 'ws', 'desktop');
		expect(loader.displaySrc).toBe(ORIGINAL('B'));
		expect(loader.displaySrc).not.toContain('/A');
	});

	it('a LATE decode for a navigated-away image does NOT drive the new image', () => {
		const loader = createViewerImageLoader();
		loader.load(image('A', { width: 5000, height: 5000 }), 'ws', 'desktop');
		const staleSrc = loader.displaySrc; // A's thumb
		const staleGen = loader.loadToken; // A's generation
		loader.load(image('B', { width: 800, height: 600 }), 'ws', 'desktop');
		expect(loader.displaySrc).toBe(ORIGINAL('B'));

		// A's thumbnail finishes decoding LATE, at a fallback size (>1024). Without
		// the src fence this would flip B into an unexpected upgrade / phase.
		loader.decoded(5000, 5000, staleSrc, staleGen);
		expect(loader.displaySrc).toBe(ORIGINAL('B')); // untouched
	});

	it('the GENERATION fence rejects an A→B→A same-URL stale decode', () => {
		// The URL fence alone is insufficient: navigating A→B→A reuses A's exact
		// URL, so the detached first A element's late decode has the SAME src as the
		// live third request. Only the captured generation tells them apart.
		const loader = createViewerImageLoader();
		loader.load(image('A', { width: 5000, height: 5000 }), 'ws', 'desktop');
		const firstA = loader.loadToken; // the detached A element's generation
		loader.load(image('B', { width: 5000, height: 5000 }), 'ws', 'desktop');
		loader.load(image('A', { width: 5000, height: 5000 }), 'ws', 'desktop');
		expect(loader.displaySrc).toBe(THUMB('A')); // the live third request

		// The FIRST A element decodes late at a fallback size (>1024). If accepted it
		// would call `servedOriginal` true and SUPPRESS the live A's upgrade.
		loader.decoded(5000, 5000, THUMB('A'), firstA);
		// Live A is untouched — its own decode still drives the upgrade.
		loader.decoded(1024, 768, loader.displaySrc, loader.loadToken);
		expect(loader.displaySrc).toBe(ORIGINAL('A')); // upgraded, not suppressed
	});

	it('the GENERATION fence rejects an A→B→A same-URL stale ERROR', () => {
		const loader = createViewerImageLoader();
		loader.load(image('A', { width: 800, height: 600 }), 'ws', 'desktop');
		const firstA = loader.loadToken;
		loader.load(image('B', { width: 800, height: 600 }), 'ws', 'desktop');
		loader.load(image('A', { width: 800, height: 600 }), 'ws', 'desktop');
		// The live third A decodes successfully.
		loader.decoded(800, 600, loader.displaySrc, loader.loadToken);
		expect(loader.phase).toBe('ready');
		// The detached first A element errors LATE at the same URL — it must NOT
		// flip the live, ready image into 'error'.
		loader.errored(ORIGINAL('A'), firstA);
		expect(loader.phase).toBe('ready');
	});

	it('dispose (close / shrink to empty) drops the load', () => {
		const loader = createViewerImageLoader();
		loader.load(image('A', { width: 800, height: 600 }), 'ws', 'desktop');
		expect(loader.displaySrc).not.toBe('');
		loader.dispose();
		expect(loader.displaySrc).toBe('');
		expect(loader.phase).toBe('idle');
		// A stale decode after dispose is inert.
		loader.decoded(800, 600, ORIGINAL('A'), loader.loadToken);
		expect(loader.phase).toBe('idle');
	});
});

describe('viewerImageLoader — error + retry (TASK-2459)', () => {
	it('a load failure shows a retryable error; retry RE-REQUESTS (never replays)', () => {
		const loader = createViewerImageLoader();
		loader.load(image('A', { width: 800, height: 600 }), 'ws', 'desktop');
		const url = loader.displaySrc;
		loader.errored(url, loader.loadToken);
		expect(loader.phase).toBe('error');

		loader.retry();
		// Re-issued: the src is reset then set again (a real re-request, not a
		// replay of the failed one).
		expect(loader.displaySrc).toBe(url);
		expect(loader.phase).toBe('loading');
		loader.decoded(800, 600, loader.displaySrc, loader.loadToken);
		expect(loader.phase).toBe('ready');
	});

	it('retry bumps the load token so the viewer re-requests a same-URL failure', () => {
		const loader = createViewerImageLoader();
		loader.load(image('A', { width: 800, height: 600 }), 'ws', 'desktop');
		const t1 = loader.loadToken;
		loader.errored(loader.displaySrc, loader.loadToken);
		loader.retry();
		// Same URL, but a NEW token — the viewer re-mounts the <img> and re-fetches.
		expect(loader.displaySrc).toBe(ORIGINAL('A'));
		expect(loader.loadToken).toBeGreaterThan(t1);
	});

	it('the thumb→original UPGRADE does NOT bump the load token (element reused)', () => {
		const loader = createViewerImageLoader();
		loader.load(image('A', { width: 5000, height: 5000 }), 'ws', 'desktop');
		const t1 = loader.loadToken;
		loader.decoded(1024, 768, loader.displaySrc, loader.loadToken); // upgrade
		expect(loader.displaySrc).toBe(ORIGINAL('A'));
		expect(loader.loadToken).toBe(t1); // unchanged — same element, no flash
	});

	it('ignores an error for a stale (already-navigated-away) src', () => {
		const loader = createViewerImageLoader();
		loader.load(image('A', { width: 800, height: 600 }), 'ws', 'desktop');
		const staleSrc = loader.displaySrc;
		const staleGen = loader.loadToken;
		loader.load(image('B', { width: 800, height: 600 }), 'ws', 'desktop');
		loader.errored(staleSrc, staleGen);
		expect(loader.phase).toBe('loading'); // B is unaffected
	});
});
