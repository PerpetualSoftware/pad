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
	/** Nothing to load — no image, an unsafe/unresolved entry (DR-16), or a
	 *  mobile large/unknown cell whose original arrives on tap (TASK-2460). */
	| 'idle'
	/** A request is in flight (first paint, or the background upgrade). */
	| 'loading'
	/** A bitmap is displayed. The background upgrade may still be running. */
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
	 * Point the loader at `img` in workspace `wsSlug` on `platform`, superseding
	 * any prior load (its in-flight request is dropped by the `src` change). An
	 * `idle` no-op for no image, an unsafe/unresolved MIME (DR-16), or a cell that
	 * loads nothing now.
	 */
	load(img: LightboxImage | undefined, wsSlug: string, platform: Platform): void;
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
	// Non-reactive: never read in an $effect's tracked scope, so writing it can't
	// self-invalidate a flush (CONVE-1688).
	let active: ActiveLoad | null = null;

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
			active = null;
			displaySrc = '';
			phase = 'idle';
			return;
		}
		// A NEW request (load / retry). The upgrade in `decoded` does NOT call
		// `start`, so it never bumps this — the element (and its bitmap) is reused.
		loadToken++;
		const decision = decideFirstRequest(a.img.width, a.img.height, a.platform);
		if (decision.variant === null) {
			// Mobile large/unknown: nothing loads now (the tap affordance is
			// TASK-2460). Not an error — an intentional idle.
			phase = 'idle';
			displaySrc = '';
			return;
		}
		a.upgradePending = decision.variant === 'thumb-md' && decision.upgrade === true;
		a.upgrading = false;
		phase = 'loading';
		displaySrc = variantUrl(a.wsSlug, a.img.id, decision.variant);
	}

	function load(img: LightboxImage | undefined, wsSlug: string, platform: Platform): void {
		// Repoint: the `src` reassignment below (or the '' here) drops the old
		// request. No reference to release — nothing is retained across images.
		active = null;
		displaySrc = '';
		if (!img) {
			// No image to show. The DR-16 MIME gate lives in `start()` — the request
			// chokepoint, so retry is gated too — leaving only "no image" here.
			phase = 'idle';
			return;
		}
		active = { img, wsSlug, platform, upgradePending: false, upgrading: false };
		start();
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
		if (!a.upgradePending || a.upgrading) return;
		// The first (thumb-md) bitmap decoded. If its long edge exceeds the
		// thumbnail bound the server fell back to the ORIGINAL — do NOT request it
		// again (the double decode the whole policy exists to prevent).
		if (servedOriginal(naturalWidth, naturalHeight)) {
			a.upgradePending = false;
			return;
		}
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
		if (!active) return;
		// A fresh request, never a replay: the `src` is cleared and reissued, and
		// the server may now have the variant (derivation completes async).
		displaySrc = '';
		start();
	}

	function dispose(): void {
		active = null;
		displaySrc = '';
		phase = 'idle';
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
		load,
		decoded,
		errored,
		retry,
		dispose,
	};
}
