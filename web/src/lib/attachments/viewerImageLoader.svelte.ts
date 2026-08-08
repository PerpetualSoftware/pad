/**
 * The attachment viewer's image LOADER (PLAN-2392 / TASK-2459) — the desktop
 * thumb-then-original path with the DR-5b memory policy from
 * {@link ./viewerLoading}.
 *
 * MODEL. The viewer's `<img>` is loaded NATIVELY — `displaySrc` is the canonical
 * attachment URL (a `?variant=thumb-md` first, the plain original second), and
 * the browser fetches, decodes, caches and (on `src` reassignment) CANCELS each
 * request. That reassignment is the abort: repointing the loader immediately
 * drops the URL the user navigated away from, and no cross-image reference —
 * off-DOM `Image`, object URL, fetch controller — is ever retained, so arrowing
 * through twenty images can leak nothing. The bytes stay under the browser's own
 * cache, not a hand-rolled blob store.
 *
 * STALENESS (the no-`{#key}` switch-safety class). The `<img>` persists across
 * navigation, so a bitmap that finishes decoding LATE could report the wrong
 * image's pixels. Every `decoded()` therefore carries the src that decoded and
 * is ignored unless it still equals `displaySrc` — the fence that keeps a stale
 * completion from driving the fallback detector / upgrade for the image the user
 * has already left. The workspace slug is the CAPTURED one the caller passes
 * (the pane can switch workspace under a mounted viewer).
 *
 * The FALLBACK DETECTOR ({@link servedOriginal}) is the load-bearing piece: when
 * a `thumb-md` request is served the original (fresh upload mid-derivation, or a
 * WebP/AVIF the server can't derive), the decoded long edge exceeds the
 * thumbnail bound and the background original request is SKIPPED — the double
 * decode the whole policy exists to prevent.
 */
import { attachmentDownloadUrl } from '$lib/markdown/attachments';
import { canOpenInViewer } from '$lib/attachments/display';
import { decideFirstRequest, servedOriginal, type Platform } from './viewerLoading';
import type { LightboxImage } from './events';

export type LoadPhase =
	/** Nothing to load and nothing to offer — no image, or an unsafe/unresolved
	 *  entry (DR-16). No `<img>`, no affordance. */
	| 'idle'
	/** A mobile large/unknown cell: nothing auto-loads, but the ORIGINAL is
	 *  available on demand — the viewer shows the tap-to-load affordance, and a
	 *  tap (or zoom-past-fit once a bitmap exists) calls {@link
	 *  ViewerImageLoader.loadOriginal} (TASK-2460). Distinct from `idle`, which
	 *  offers nothing. */
	| 'deferred'
	/** A request is in flight (first paint, or the on-demand / background
	 *  original). */
	| 'loading'
	/** A bitmap is displayed. A background/on-demand original may still be
	 *  running. */
	| 'ready'
	/** The image failed to load; retryable. */
	| 'error';

export interface ViewerImageLoader {
	/** The URL the viewer's `<img>` should show (canonical attachment URL), or `''`. */
	readonly displaySrc: string;
	readonly phase: LoadPhase;
	/**
	 * A token that changes on every fresh REQUEST — a load or a retry, but NOT the
	 * thumb→original upgrade. The viewer keys its `<img>` on this so a retry (whose
	 * URL is often unchanged) actually re-mounts and re-requests, and so a nav
	 * gets a fresh element (tearing down the previous one's stale load listener),
	 * while the upgrade reuses the element and does not flash.
	 */
	readonly loadToken: number;
	/**
	 * Whether a bitmap has DECODED for the current image and is on screen (a thumb
	 * or the original). False from `load` until the first decode, and in `idle` /
	 * `deferred`; stays true across the thumb→original upgrade. The viewer's
	 * zoom-past-fit trigger reads it so a zoom made BEFORE the thumb paints still
	 * upgrades the moment it does — and, because `loadOriginal` never writes it, the
	 * trigger effect can track it without self-invalidating (CONVE-1688).
	 */
	readonly painted: boolean;
	/**
	 * Point the loader at `img` in workspace `wsSlug` on `platform`, superseding
	 * any prior load (its in-flight request is dropped by the `src` change). An
	 * `idle` no-op for no image, an unsafe/unresolved MIME (DR-16), or a cell that
	 * loads nothing now.
	 */
	load(img: LightboxImage | undefined, wsSlug: string, platform: Platform): void;
	/**
	 * Request the ORIGINAL on demand — the mobile fetch DR-5b defers. Called by
	 * BOTH the tap affordance (the `deferred` cell, where nothing painted) and by
	 * zoom-past-fit (the mobile thumb cell, where a thumbnail painted). The two are
	 * ONE deduplicated fetch: a call while the original is already requested or
	 * in flight is a no-op, so a tap racing a zoom, 3d's pinch, or a retry can
	 * never issue a second request (TASK-2460). A no-op on desktop and on any cell
	 * that already loaded the original directly, where nothing is deferred.
	 */
	loadOriginal(): void;
	/**
	 * The `<img>` decoded `decodedSrc` at `naturalWidth x naturalHeight`. `gen` is
	 * the `loadToken` the reporting element was mounted under. Ignored unless BOTH
	 * `gen` is the current `loadToken` (the generation fence) AND `decodedSrc` is
	 * still the current `displaySrc`. The generation fence is load-bearing where
	 * the URL fence is not: an A→B→A navigation reuses A's exact URL, so a detached
	 * first element's late completion has the same `decodedSrc` as the live request
	 * — only the per-mount `gen` tells them apart (the no-`{#key}` switch-safety
	 * class). Drives the fallback detector and the thumb→original upgrade.
	 */
	decoded(naturalWidth: number, naturalHeight: number, decodedSrc: string, gen: number): void;
	/**
	 * The `<img>` failed to load `erroredSrc`. `gen` is the reporting element's
	 * mount `loadToken`. Ignored unless it is the current generation AND src —
	 * without the generation fence a detached same-URL element's late error would
	 * clobber the live element's success (A→B→A).
	 */
	errored(erroredSrc: string, gen: number): void;
	/** Retry a failed load — RE-REQUESTS from scratch, never replays a failure. */
	retry(): void;
	/** Drop the current load (on close / set-shrink to empty). */
	dispose(): void;
}

interface ActiveLoad {
	img: LightboxImage;
	wsSlug: string;
	platform: Platform;
	/** Whether a background original upgrade is still owed (desktop thumb-md). */
	upgradePending: boolean;
	/** True once the original request has been issued. */
	upgrading: boolean;
	/**
	 * Whether the ORIGINAL is available to fetch ON DEMAND and has not been
	 * requested yet — the mobile deferral (TASK-2460). True for a mobile
	 * large/unknown cell (fetched on tap) AND a mobile thumb cell (fetched on
	 * zoom-past-fit); false on desktop (auto-upgrades) and where the original was
	 * loaded directly. {@link loadOriginal}'s dedup guard reads it: it flips false
	 * on the first fetch, so a second trigger is a no-op.
	 */
	originalDeferred: boolean;
}

/**
 * The URL for one variant. `original` is the CANONICAL, no-`?variant` download —
 * that is what "the original" IS on the server (it serves the original when no
 * derived row exists); the thumb↔original distinction IS the query param.
 */
function variantUrl(wsSlug: string, id: string, variant: 'thumb-md' | 'original'): string {
	return variant === 'original'
		? attachmentDownloadUrl(wsSlug, id)
		: attachmentDownloadUrl(wsSlug, id, 'thumb-md');
}

export function createViewerImageLoader(): ViewerImageLoader {
	let displaySrc = $state('');
	let phase = $state<LoadPhase>('idle');
	// Bumped on each fresh request so the viewer can key its <img> and force a
	// re-request even when the URL is unchanged (a retry) — see `loadToken`.
	let loadToken = $state(0);
	// Whether a bitmap has decoded for the current image (see the interface). The
	// zoom-past-fit trigger TRACKS this; `loadOriginal` deliberately never writes
	// it, so that tracking cannot self-invalidate the trigger's flush (CONVE-1688).
	let painted = $state(false);
	// Non-reactive: never read in an $effect's tracked scope, so writing it can't
	// self-invalidate a flush (CONVE-1688).
	let active: ActiveLoad | null = null;

	// Retire to the neutral state — no active load, no URL, nothing decoded, no
	// affordance. ONE cleanup so every "give up on this entry" site keeps the
	// `painted === false in idle` contract; sprinkling the resets inline is how one
	// gets left stale.
	function toIdle(): void {
		active = null;
		displaySrc = '';
		painted = false;
		phase = 'idle';
	}

	function start(): void {
		const a = active;
		if (!a) return;
		// DR-16 as a LOADING gate, at the request CHOKEPOINT: both load() and
		// retry() funnel through here, so an unsafe or unresolved MIME never issues
		// a request from either — even were a prior state to leave `active` set. The
		// gate is restated where the request is actually made (not only at the
		// renderer), because the renderer showing nothing is not the same as the
		// loader asking for nothing.
		if (!canOpenInViewer(a.img.mime_type)) {
			toIdle();
			return;
		}
		// A NEW request (load / retry). The upgrade in `decoded` and the on-demand
		// `loadOriginal` do NOT call `start`, so neither bumps this — the element
		// (and its bitmap) is reused.
		loadToken++;
		const decision = decideFirstRequest(a.img.width, a.img.height, a.platform);
		a.upgrading = false;
		// The original is DEFERRED (fetched on tap / zoom-past-fit) precisely when
		// the platform is mobile and the first request is not already the original:
		// the mobile large/unknown cell (nothing painted) and the mobile thumb cell
		// (a thumbnail painted, no auto-upgrade). Desktop auto-upgrades; a
		// direct-original cell has nothing left to fetch (TASK-2460).
		a.originalDeferred = a.platform === 'mobile' && decision.variant !== 'original';
		a.upgradePending = decision.variant === 'thumb-md' && decision.upgrade === true;
		if (decision.variant === null) {
			// Mobile large/unknown: nothing auto-loads. `deferred` (NOT `idle`) so the
			// viewer shows the tap-to-load affordance; the original arrives via
			// `loadOriginal` on tap.
			phase = 'deferred';
			displaySrc = '';
			return;
		}
		phase = 'loading';
		displaySrc = variantUrl(a.wsSlug, a.img.id, decision.variant);
	}

	function load(img: LightboxImage | undefined, wsSlug: string, platform: Platform): void {
		// Repoint: the `src` reassignment below (or the '' here) drops the old
		// request. No reference to release — nothing is retained across images.
		active = null;
		displaySrc = '';
		painted = false; // a new image has nothing decoded yet
		if (!img) {
			// No image to show. The DR-16 MIME gate lives in `start()` — the request
			// chokepoint, so retry is gated too — leaving only "no image" here.
			phase = 'idle';
			return;
		}
		active = { img, wsSlug, platform, upgradePending: false, upgrading: false, originalDeferred: false };
		start();
	}

	function loadOriginal(): void {
		const a = active;
		// Dedup: only fetch when an original is DEFERRED and none is already in
		// flight. This single guard makes the two triggers (tap, zoom-past-fit) one
		// fetch and closes every race — a second tap, 3d's pinch reaching
		// zoom-past-fit again, or a retry that overlaps a zoom all find
		// `originalDeferred` already false (or `upgrading` true) and no-op.
		if (!a || !a.originalDeferred || a.upgrading) return;
		// DR-16 restated at this request edge too (defensive: an entry only becomes
		// `originalDeferred` in `start()` AFTER its gate, so `a.img` is already safe —
		// but every place that issues a request re-checks, so the invariant is local
		// rather than argued). RETIRE the entry to `idle` — the same cleanup `start`
		// and `retry` do — rather than only clearing the flag, which would leave the
		// tap affordance rendered (`phase` stuck `'deferred'`) over a dead, no-op
		// button.
		if (!canOpenInViewer(a.img.mime_type)) {
			toIdle();
			return;
		}
		a.originalDeferred = false;
		a.upgrading = true;
		// No `loadToken` bump: the mobile THUMB cell reuses its painted element so
		// the thumbnail stays visible until the original decodes (no flash, like the
		// desktop upgrade); the DEFERRED cell has no element yet, so one mounts fresh
		// at the current token. `phase` returns to 'loading' while it is in flight.
		phase = 'loading';
		displaySrc = variantUrl(a.wsSlug, a.img.id, 'original');
	}

	function decoded(naturalWidth: number, naturalHeight: number, decodedSrc: string, gen: number): void {
		const a = active;
		// Staleness fence, two parts. The generation fence (`gen !== loadToken`)
		// rejects a DETACHED element's late completion even when it carries the
		// live URL — an A→B→A nav reuses A's exact URL, so the src check alone would
		// let the old A element's late decode drive the new A load. The src check
		// then handles the within-element thumb→original sequencing.
		if (!a || gen !== loadToken || decodedSrc === '' || decodedSrc !== displaySrc) return;
		phase = 'ready';
		// A bitmap is now on screen (thumb or original) — this is the paint the
		// zoom-past-fit trigger waits for, so a pre-paint zoom upgrades the instant
		// it becomes true.
		painted = true;
		// Decoding the ORIGINAL (a desktop upgrade or a mobile on-demand fetch is in
		// flight): nothing left to decide.
		if (a.upgrading) return;
		// Decoding the FIRST request (thumb-md, or an original-direct cell). The
		// fallback detector runs on EVERY first decode, desktop or mobile: if the
		// long edge exceeds the thumbnail bound the server served the ORIGINAL
		// (thumb-md absent — fresh upload, or a WebP/AVIF it can't derive). There is
		// then nothing more to fetch on EITHER path — clear the desktop upgrade AND
		// the mobile deferred original, so neither a background upgrade nor a mobile
		// zoom-past-fit re-requests bytes we already have (the double decode the
		// whole policy exists to prevent).
		if (servedOriginal(naturalWidth, naturalHeight)) {
			a.upgradePending = false;
			a.originalDeferred = false;
			return;
		}
		// A real thumbnail (<= bound) decoded. The mobile thumb cell leaves
		// `originalDeferred` true and waits for the zoom-past-fit trigger; only the
		// DESKTOP cell background-upgrades now.
		if (!a.upgradePending) return;
		// DR-16 restated at the upgrade request edge too. Defensive: `a.img` is the
		// same entry `start()` already gated, but every place that issues a request
		// re-checks, so the invariant is local rather than argued.
		if (!canOpenInViewer(a.img.mime_type)) {
			a.upgradePending = false;
			return;
		}
		// Background-request the original. `phase` returns to 'loading' while it is
		// in flight; the thumbnail stays displayed until the original decodes.
		a.upgrading = true;
		a.upgradePending = false;
		phase = 'loading';
		displaySrc = variantUrl(a.wsSlug, a.img.id, 'original');
	}

	function errored(erroredSrc: string, gen: number): void {
		// Generation + src fence (see `decoded`): a detached same-URL element's late
		// error must not flip the live element's success into 'error'.
		if (!active || gen !== loadToken || erroredSrc === '' || erroredSrc !== displaySrc) return;
		phase = 'error';
	}

	function retry(): void {
		const a = active;
		if (!a) return;
		// A fresh request, never a replay: the `src` is cleared and reissued, and
		// the server may now have the variant (derivation completes async).
		displaySrc = '';
		if (a.upgrading) {
			// The failed request was the on-demand / background ORIGINAL (a mobile tap
			// or zoom-past-fit, or a desktop upgrade). Re-request the ORIGINAL directly
			// — NOT the initial policy, which for a mobile deferred cell would revert
			// to the tap affordance and drop the user's committed load intent. A fresh
			// token remounts the element so the same URL actually refetches.
			// DR-16 restated at this request edge too (like `start`).
			if (!canOpenInViewer(a.img.mime_type)) {
				toIdle();
				return;
			}
			loadToken++;
			phase = 'loading';
			displaySrc = variantUrl(a.wsSlug, a.img.id, 'original');
			return;
		}
		start();
	}

	function dispose(): void {
		toIdle();
	}

	return {
		get displaySrc() {
			return displaySrc;
		},
		get phase() {
			return phase;
		},
		get loadToken() {
			return loadToken;
		},
		get painted() {
			return painted;
		},
		load,
		loadOriginal,
		decoded,
		errored,
		retry,
		dispose,
	};
}
