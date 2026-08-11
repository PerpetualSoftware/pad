<script lang="ts">
	/**
	 * Full-screen image viewer for attachment thumbnails (IDEA-1660).
	 * Opened by a host that captures a click on an `img[data-attachment-id]`
	 * and passes the attachment id(s). Loading follows the DR-5b memory policy
	 * (PLAN-2392 phase 3b): a bounded `thumb-md` paints first and the viewer
	 * upgrades to the full-resolution original in the background on desktop, while
	 * a large image on mobile defers the original behind a tap-to-load affordance
	 * (TASK-2459 / TASK-2460) — see `$lib/attachments/viewerImageLoader`.
	 *
	 * MODAL CONTRACT (PLAN-2392 phase 3a / TASK-2429, DR-4b). This is a real
	 * modal now: `role="dialog"` + `aria-modal`, portaled to `<body>`, focus
	 * entry + restore, a Tab trap, background inertness through the shared
	 * viewer-backdrop manager, and Escape via the shared `escapeStack`. It is
	 * the contract the editor's hand-rolled `showModal()` dialog was getting
	 * from the platform for free, written out by hand — because 3a's later
	 * tasks delete that dialog and route the inline body images here.
	 *
	 * Keyboard: Esc closes (through `escapeStack`, NOT a local listener),
	 * ←/→ navigate when multiple images were passed, Tab cycles within the
	 * viewer, `+`/`-` zoom about the stage centre and `0` resets (PLAN-2392
	 * phase 3b / TASK-2455). Backdrop click closes; clicking the image itself
	 * does not.
	 */
	import { untrack } from 'svelte';
	import { paneFocusables, nextTrapTarget, handoffFocus } from '$lib/collections/paneFocus';
	import { createViewerImageLoader } from '$lib/attachments/viewerImageLoader.svelte';
	import type { Platform } from '$lib/attachments/viewerLoading';
	import { viewport } from '$lib/stores/breakpoint.svelte';
	import {
		acquire,
		isBlockedByModal,
		isViewerFrontmost,
		noteEscapeConsumedByViewer,
		VIEWER_ROOT_CLASS,
	} from '$lib/a11y/viewerBackdrop';
	import { pushEscapeHandler, ESCAPE_PRIORITY } from '$lib/stores/escapeStack';
	// The renderer registry (TASK-2476): 'raster-image' for the DR-16 allowlist,
	// null → the no-bytes icon fallback. This is the single gate that decides the
	// stage's ARM; the old `canOpenInViewer` last-mile FILTER is gone.
	import { getSurfaceRenderer, type SurfaceRendererId } from '$lib/attachments/surfaceRenderers';
	import {
		reset as resetZoom,
		clampState,
		clampPan,
		zoomTo,
		toggleFitOrActual,
		stageCenter,
		isAtFit,
		ZOOM_STEP,
		type Geometry,
		type ZoomState,
	} from '$lib/attachments/zoom';
	/**
	 * ONE definition of what an image in this viewer is (PLAN-2392 / TASK-2431).
	 *
	 * It used to be declared here as `{id, alt}` and again — as a superset — on
	 * the open-viewer channel, with a comment noting the two were structurally
	 * compatible. Two declarations of the same thing drift the moment one gains
	 * a field, which is exactly what TASK-2431 does (`mime_type` is what makes
	 * the DR-16 open gate total, and `size_bytes` / `width` / `height` land now
	 * so phase 3b's pixel-based loading policy need not reopen the event, the
	 * host and every producer). The channel's is now the only one.
	 *
	 * Everything past `id` / `alt` is NULLABLE and this component treats it as
	 * such: an inline image's metadata comes from a HEAD probe that may not have
	 * completed, and an upload event carries only the `UploadedAttachment` fields
	 * (filename, MIME, size, and — since TASK-2459 — the pixel dimensions).
	 *
	 * Producers that mount this component directly import the type from the
	 * CHANNEL too, not from here — a `.svelte` module cannot re-export a type,
	 * and a local `interface … extends` would be a second declaration, which is
	 * the drift this consolidation removes.
	 */
	import {
		registerAttachmentDeletionListener,
		type LightboxImage,
	} from '$lib/attachments/events';
	// ── Toolbar (PLAN-2392 phase 3c-i / TASK-2474) ───────────────────────────
	// The viewer's copy of the shared attachment-action list (DR-5) — the SAME
	// descriptors the options panel renders, drawn here as an inline toolbar over
	// the stage. Delete confirmation reuses the panel's B module (TASK-2473): the
	// module owns the confirmation GATE, the descriptor owns the delete.
	import {
		attachmentActionsFor,
		type AttachmentAction,
		type AttachmentActionContext,
		type ButtonAttachmentAction,
	} from '$lib/attachments/actions';
	import { createDeleteConfirm } from '$lib/attachments/surfaceDeleteConfirm.svelte';
	import AttachmentIcon from '$lib/attachments/icons/AttachmentIcon.svelte';
	import AttachmentDeleteConfirm from '$lib/components/attachments/AttachmentDeleteConfirm.svelte';
	import { attachmentRefsIn } from '$lib/utils/commentAttachments';
	import { toastStore } from '$lib/stores/toast.svelte';
	// ── Metadata header (PLAN-2392 phase 3c-i / TASK-2475) ───────────────────
	// Filename / type / size for the shown image, seeded from the LightboxImage
	// and completed by the SAME B metadata module the panel uses (TASK-2473): it
	// fetches only what the seed left null (DR-2 — open now, fill after).
	import { describeAttachmentType, formatBytes, iconForAttachment } from '$lib/attachments/display';
	import { createSurfaceMetadata } from '$lib/attachments/surfaceMetadata.svelte';

	interface Props {
		images: LightboxImage[];
		/** Index to open at (clamped). */
		index?: number;
		wsSlug: string;
		onClose: () => void;
		/**
		 * The control that opened the viewer — focus goes back to it on close.
		 * OPTIONAL: the strip, the timeline and the NodeView thread real values in
		 * TASK-2431 / TASK-2433. Until then it falls back to whatever held focus
		 * at open (see below), which is the same element in every current path.
		 */
		invoker?: HTMLElement | null;
		/**
		 * Whether the viewer may offer MUTATING actions — Delete (TASK-2474). The
		 * host's answer (`canEdit && !peeking`), threaded identically at every hop
		 * (host / strip / timeline). DEFAULT false: an omitted prop degrades to a
		 * read-only toolbar (open / download / copy-link), never a wrongly-gated
		 * one. The timeline's OWN `canEdit` (which ignores peeking) is never the
		 * source — a peeked master must show no Delete.
		 */
		mutationsEnabled?: boolean;
		/**
		 * The persisted item body + the editor's LIVE markdown, as getters — the
		 * pattern the retired options panel used. Used ONLY to warn when a delete
		 * would break a live reference in this body (DR-5); read at confirm time so
		 * the live getter sees unflushed edits. Absent → the warning falls to its
		 * hedged arm.
		 */
		getItemContent?: () => string | null;
		getLiveContent?: () => string | null;
		/**
		 * The shown attachment's parent item is archived (PLAN-2392 3c-ii / T3).
		 * An archived parent makes reads 404, so the seed is not evidence the bytes
		 * are REACHABLE (DR-14): the metadata machine is forced to probe, and every
		 * toolbar action stays inert until that probe resolves (no live actions on
		 * stale bytes). DEFAULT false — the common open is of a live item. Hardcoded
		 * false before T3; a prop now so the host can thread its parent's state (the
		 * archive-closes / restore-revalidates lifecycle lands with the host in T2a).
		 */
		parentArchived?: boolean;
		/**
		 * Host-owned forced-revalidation signal (PLAN-2392 3c-ii / T2a). A BUMP
		 * forces the metadata machine to invalidate-then-fetch (DR-14) — the
		 * restore-revalidate path: a surface opened while the parent was archived
		 * (probe-gated / missing) re-probes when the parent is restored, rather than
		 * assuming the pre-archive answer still holds. DEFAULT 0. T6's
		 * always-revalidate-on-open `openNonce` layers onto this same forcing input.
		 */
		revalidateToken?: number;
		/**
		 * Per-OPEN nonce (PLAN-2392 3c-ii T6). The host mints a fresh value per
		 * accepted open and rides it on the request (so the `{#key request}` remount
		 * carries the matching nonce). It joins the metadata machine's SUBJECT
		 * identity, forcing exactly one `no-store` revalidating HEAD of the opened
		 * entry — the always-revalidate-on-open guarantee that catches a cross-tab /
		 * background delete the browser's `max-age` HEAD cache would otherwise hide.
		 * CONSTANT for the life of this mount (a new open remounts via `{#key}`), so
		 * arrowing does not re-force. DEFAULT 0.
		 */
		openNonce?: number;
	}

	let {
		images,
		index = 0,
		wsSlug,
		onClose,
		invoker = null,
		mutationsEnabled = false,
		getItemContent,
		getLiveContent,
		parentArchived = false,
		revalidateToken = 0,
		openNonce = 0,
	}: Props = $props();

	/**
	 * ADMISSION vs THE ARM (PLAN-2392 DR-20 final form / 3c-ii T3).
	 *
	 * Through 3c-i this component was image-only: a last-mile filter kept ONLY
	 * positively-allowlisted (raster) MIMEs navigable and REFUSED everything else
	 * — a null/unresolved MIME, or a resolved-unsafe one — outright. 3c-ii
	 * converges the panel and the viewer onto ONE surface that opens ANY
	 * attachment: an image, a file (PDF, ZIP), or a row whose MIME is not yet
	 * resolved. So admission and rendering split into two decisions:
	 *
	 *  - ADMISSION is now UNIVERSAL. Every entry is navigable; `←/→` page through
	 *    the whole set. There is no MIME refusal left here.
	 *  - THE ARM is where safety lives (`shownRenderer`, from `getSurfaceRenderer`
	 *    on the RESOLVED MIME). `'raster-image'` mounts the `<img>` and loads
	 *    bytes; `null` — an unsafe/active type, a file, or a still-unresolved MIME
	 *    — mounts the NO-BYTES icon fallback (no `<img>`, no `src`, no fetch). The
	 *    allowlist governs the ARM, never admission.
	 *
	 * WHY ADMITTING UNSAFE/UNRESOLVED IS SAFE. The DR-16 concern was never "a
	 * hostile row in the set" — it was rendering hostile BYTES as active
	 * same-origin content. The fallback arm renders no bytes at all, and the arm
	 * is re-derived from the resolved MIME every frame (and joins the load key),
	 * so an entry that is unsafe at open, flips unsafe mid-view, or resolves
	 * unsafe after a probe all land on the same no-bytes fallback. The raster arm
	 * only ever activates for a MIME `getSurfaceRenderer` positively claims. What
	 * the 3c-i filter did by DROPPING, the arm now does by CLASSIFYING — with the
	 * file/unresolved rows kept and shown, which is the whole point of the
	 * converged surface.
	 *
	 * (The producer-side resolve-before-emit contract on the legacy viewer
	 * channel is unchanged and unrelated: that is the NodeView's obligation, not
	 * this component's. Nothing routes files here yet — T4a repoints producers.)
	 */
	// A blank/whitespace-only string is not a value — it is the ABSENCE of one, and
	// the display chains (`filename ?? alt ?? …`) only skip null/undefined, so an
	// empty string would win over the fallback and render nothing (TASK-2475).
	// Normalized to a trimmed value or null, once, at ingestion below.
	function blankToNull(s: string | null | undefined): string | null {
		const t = (s ?? '').trim();
		return t.length > 0 ? t : null;
	}

	// THE NAVIGABLE SET — what ←/→ page through and the counter counts. Admission is
	// now universal (3c-ii T3): EVERY entry is kept, whatever its MIME — safe
	// raster, unsafe/active, a file, or unresolved (null). Safety is the ARM's job
	// (`shownRenderer`), not this set's: a non-raster entry draws the no-bytes
	// fallback, so there is nothing to refuse. The 3c-i `unsafeAtOpenIds` snapshot
	// is retired with the refusal it fed — "unsafe at open" and "unsafe mid-view"
	// are no longer different cells, because neither is dropped; both classify to
	// the fallback. INGESTION (TASK-2475) still normalizes each row's blank filename
	// to null here so the display-name chain falls through to `alt`; identity is
	// preserved for an already-clean row so the downstream fences that key on
	// `img.id` are unaffected.
	let navigable = $derived(
		images.map((im) => {
			const clean = blankToNull(im.filename);
			return clean === im.filename ? im : { ...im, filename: clean };
		})
	);

	// DELETION TOMBSTONES (PLAN-2392 DR-5c / TASK-2477). Ids the deletion bus has
	// announced gone while this viewer is open — the SURVIVING set is `navigable`
	// minus these. Fresh per instance and never reset: every producer keys the
	// mount, so a reopen is a new component with an empty set (no cross-open
	// leakage), and a delete is authoritative for the life of the viewer that saw
	// it. A `Set` reassigned (not mutated) on each add so the `survivors` derived
	// re-runs.
	let tombstones = $state<Set<string>>(new Set());

	// THE SHOWN IMAGE, TRACKED BY ID (TASK-2477). The index is DERIVED from this,
	// not the source of truth — so deleting an EARLIER image keeps the SAME image
	// on screen (identity, not a position that now names a different member), and
	// deleting the shown one advances by recomputing from the new survivor list.
	// Seeded once at mount from the requested index, resolved through the id.
	// Admission is universal now, so `navigable` keeps every entry in order and the
	// requested position always names its own image; the id resolution stays for
	// robustness (a clamp on an out-of-range index, and the empty-set null).
	let shownId = $state<string | null>(
		untrack(() => {
			const wanted = images[Math.min(Math.max(index, 0), Math.max(images.length - 1, 0))];
			const at = wanted ? navigable.find((im) => im.id === wanted.id) : undefined;
			return (at ?? navigable[0])?.id ?? null;
		})
	);

	// CAPTURED AT OPEN, never read live (TASK-2429). The pane switches workspace
	// without remounting whatever is above it, so a live read could rebuild the
	// URLs of already-captured attachment ids against a DIFFERENT workspace —
	// serving a 404, or worse, another workspace's attachment at the same id.
	// `invoker` is captured for the same reason: the value that opened this
	// viewer is the one to return focus to.
	const openWsSlug = untrack(() => wsSlug);
	// The invoker falls back to WHATEVER HELD FOCUS AT OPEN (the pattern
	// `BottomSheet` uses), not to nothing. Focus entry is about to move focus
	// into the viewer, so without this the producers that don't thread an
	// invoker yet — the strip and the timeline, until TASK-2431 — would be
	// strictly worse off than before this component managed focus at all: they
	// keep focus on the clicked tile today, and a null invoker would drop it to
	// `<body>` on close. Captured at init, BEFORE the entry focus runs.
	const openInvoker = untrack(() => {
		if (invoker) return invoker;
		if (typeof document === 'undefined') return null;
		const active = document.activeElement;
		return active && active !== document.body ? (active as HTMLElement) : null;
	});

	// THE SURVIVING SET — `navigable` minus the tombstones (TASK-2477). EVERYTHING
	// PAST THIS POINT READS `survivors`, NEVER `images` or even `navigable`: the nav
	// wrap-around, the counter and the rendered stage alike. A read of the raw
	// `images` prop would skip the blank-filename normalization (and any future
	// ingestion step); a read of `navigable` (pre-tombstone) would page onto a
	// deleted image. Safety is no longer among the reasons — the arm handles that.
	let survivors = $derived(navigable.filter((im) => !tombstones.has(im.id)));
	let hasMultiple = $derived(survivors.length > 1);
	// The position actually shown, DERIVED from `shownId`. Falls to 0 when the id
	// dangles — a shown image removed straight from the `images` prop (a producer's
	// set change, not the deletion bus, which advances `shownId` itself). Deriving
	// the index (rather than writing it from an effect that reads it) keeps the
	// viewer on a real member instead of blanking or showing `undefined`.
	let shownIndex = $derived(
		Math.max(
			survivors.findIndex((im) => im.id === shownId),
			0
		)
	);
	let img = $derived(survivors[shownIndex]);

	// ── Metadata header (PLAN-2392 phase 3c-i / TASK-2475) ────────────────────
	//
	// Filename / type / size for the shown image, seeded from the LightboxImage and
	// completed by the B module (TASK-2473) — it fetches only what the seed left
	// null (DR-2: open now, fill after). 3c-ii T3 makes two things live that were
	// constant before: the MIME is no longer known at open (a file or an unresolved
	// row is a first-class member), so a null-seed `mime_type` is fetched too and
	// the RESOLVED value below drives the stage arm and the toolbar; and
	// `parentArchived` is the prop, so an archived-parent open forces the machine's
	// reachability probe (DR-14) instead of trusting the seed.
	const headerMeta = createSurfaceMetadata(() => ({
		ws: openWsSlug,
		attachmentId: img?.id ?? '',
		seed: {
			filename: img?.filename ?? null,
			mime_type: img?.mime_type ?? null,
			size_bytes: img?.size_bytes ?? null,
		},
		open: !!img,
		parentArchived,
		revalidateToken,
		openNonce,
	}));

	// THE RESOLVED MIME, and THE STAGE ARM off it (3c-ii T3, TASK-2476 for the arm).
	// `headerMeta.fields.mime_type` is the seed's MIME OR — when the seed was null —
	// what the HEAD probe resolved. Deriving the arm (and the toolbar's Open, below)
	// from this is the RECLASSIFICATION: a null-seed open shows the no-bytes
	// fallback until the probe answers, then re-derives — raster→the `<img>`,
	// PDF/ZIP→the fallback (with/without Open). `'raster-image'` mounts bytes; a
	// null renderer (unsafe/active type, a file, or a still-null MIME) mounts the
	// no-bytes icon fallback. The arm never trusts anything but a positively
	// allowlisted RESOLVED MIME, so admitting unsafe/unresolved rows renders no
	// hostile bytes (see the admission note above).
	let resolvedMime = $derived(img ? headerMeta.fields.mime_type : null);
	let shownRenderer = $derived<SurfaceRendererId | null>(
		img ? getSurfaceRenderer(resolvedMime) : null
	);
	// Type only when the MIME is known — no "unknown type" noise (DR-2). Size only
	// when it is a real number — `formatBytes` is never fed null, and absent beats
	// "0 B" (the chip call-site precedent).
	let headerType = $derived(
		headerMeta.fields.mime_type
			? describeAttachmentType(headerMeta.fields.mime_type, headerMeta.fields.filename)
			: null
	);
	let headerSize = $derived(
		typeof headerMeta.fields.size_bytes === 'number' ? formatBytes(headerMeta.fields.size_bytes) : null
	);
	let headerDetail = $derived([headerType, headerSize].filter(Boolean).join(' · '));
	// A non-404 fetch failure: show the DR-10 inline retry BESIDE what is already
	// known (the name + type never blank out), and Retry revalidates through the
	// module rather than replaying the cached failure. The module's other authoritative
	// phase — `missing` (404) — is NOT a header state: it means the shown file is
	// gone, so it is routed through the deletion path instead (see the effect below).
	let headerTransient = $derived(headerMeta.phase === 'transient');
	// The fallback arm's large icon (TASK-2476): the same family glyph the strip /
	// panel show for this type, from the shared registry. Off the RESOLVED MIME so
	// a null-seed file reclassifies its icon too (a probe that resolves ZIP shows
	// the archive glyph, not the generic one).
	let fallbackIconId = $derived(iconForAttachment(resolvedMime, img?.filename ?? null));

	// ARCHIVED-PARENT / MISSING GATING (3c-ii T3, mirroring the panel). `missing`
	// (404) is authoritative; `unreachablePending` holds an archived parent's
	// actions inert until its reachability probe reaches a DEFINITIVE `ok`. Two
	// conditions clear-in reachability, and BOTH are needed:
	//   - `phase !== 'ok'` covers `seeded` (before the probe starts) and
	//     `transient` (a probe that failed or timed out past `METADATA_SLOW_MS`) —
	//     `slow` is false in both, so a `slow`-only gate would wrongly re-enable.
	//   - `|| slow` covers a FORCED re-probe after a prior `ok` (an archive/restore
	//     transition, T2a; a revalidate, T6): the fetched fields are retained so
	//     `phase` stays `ok` while the new probe is in flight, and only `slow`
	//     catches that window.
	// Only a settled `ok` (phase ok AND not loading) enables the actions. `missing`
	// is handled on its own line and drives the deletion/advance path below.
	let missing = $derived(headerMeta.phase === 'missing');
	let unreachablePending = $derived(
		parentArchived && (headerMeta.phase !== 'ok' || headerMeta.slow) && !missing
	);
	// A SINGLE-item surface whose file is gone (metadata 404) shows an inert "no
	// longer available" overlay instead of closing — the behavior the retired
	// options panel had, preserved through the T2b cutover (a panel open is always
	// single-item, so it is keyed on the whole set being one). A MULTI-image set is
	// unchanged: a member's 404 advances to a survivor, and the last one closes,
	// via the tombstone path below. An EXTERNAL delete (the bus) still closes even a
	// single, exactly as the panel host did — it is only the metadata-404 answer to
	// a question the user asked that earns the message rather than a flash-close.
	let soleMissing = $derived(!!img && missing && images.length === 1);

	// ── Action toolbar (PLAN-2392 phase 3c-i / TASK-2474) ─────────────────────
	//
	// The SAME `attachmentActionsFor` list the options panel renders (DR-5), drawn
	// as an inline toolbar over the stage. 3c-ii T3: the surface now holds ANY
	// attachment, so the descriptors decide per RESOLVED type — Open applies per
	// `canBrowserPreview` (an image or PDF/plain-text, not a ZIP), Download and
	// Copy-link always apply, Delete applies only when the host granted
	// `mutationsEnabled` — and `missing` / `unreachablePending` disable all of them.
	const uid = $props.id();
	const deletePromptId = `lightbox-delete-note-${uid}`;
	// THE DISPLAY-NAME CHAIN (TASK-2475): filename ?? alt ?? 'Attachment'. `filename`
	// is already blank-normalized at ingestion, so a nameless image falls through to
	// `alt` (also blank-normalized) and then the generic label — never an empty
	// string. Shared by the metadata header, the download attribute and the delete
	// prompt, so all three name the file identically.
	let displayName = $derived(img?.filename ?? blankToNull(img?.alt) ?? 'Attachment');
	// The dialog's accessible NAME (3c-ii T2b): the file's display name plus the
	// type · size the header shows, so a screen reader announces WHAT is open — the
	// metadata the retired panel exposed — not a bare alt. Never empty (`displayName`
	// always resolves), so an unnamed `role="dialog"` — announced as nothing — can't
	// happen.
	let dialogLabel = $derived([displayName, headerDetail].filter(Boolean).join(', '));
	// Inline, transient error from a button action (a failed clipboard write). Sits
	// in the toolbar beside the controls, never a blocking dialog.
	let toolbarError = $state<string | null>(null);
	let toolbarBusy = $state(false);

	/**
	 * Ids referenced by the host's body — the delete-warning check (DR-5). Read at
	 * confirm time (not derived) so the live getter sees unflushed editor edits.
	 * Both getters are optional: a producer that threads neither leaves the warning
	 * on its hedged arm, which is the honest default.
	 */
	function toolbarReferencedHere(): boolean {
		const id = img?.id;
		if (!id) return false;
		let live: string | null = null;
		try {
			live = getLiveContent?.() ?? null;
		} catch {
			live = null;
		}
		let persisted: string | null = null;
		try {
			persisted = getItemContent?.() ?? null;
		} catch {
			persisted = null;
		}
		return new Set(attachmentRefsIn(live ?? persisted ?? '')).has(id);
	}

	// The delete-confirmation machine (TASK-2473's B module). Owns the drill-down
	// STATE — pending / warning / permission-withdrawn abandon; the descriptor owns
	// the delete. `mutationsEnabled` is read live so a pane going peeked mid-confirm
	// abandons it.
	const deleteConfirm = createDeleteConfirm({
		mutationsEnabled: () => mutationsEnabled,
		isReferenced: () => toolbarReferencedHere(),
		displayName: () => displayName,
	});
	// Teardown latch: an action's async continuation may land AFTER the viewer was
	// unmounted (a keyed producer tore it down), and `onClose` is a stale
	// producer callback by then — calling it would dismiss whatever viewer the
	// producer has open BY NOW. Plain `let`, read only in the continuation.
	let destroyed = false;
	// Drop any pending confirmation on unmount so the descriptor's awaited
	// `confirmDelete()` promise settles rather than dangling (the panel's teardown),
	// and latch `destroyed` so no post-unmount continuation calls `onClose`. The
	// metadata module's fences are invalidated here too so a late HEAD can't write
	// into a torn-down header (TASK-2475).
	$effect(() => () => {
		destroyed = true;
		deleteConfirm.dispose();
		headerMeta.dispose();
	});

	/**
	 * The action context. GETTERS, not a snapshot: the delete descriptor re-reads
	 * `mutationsEnabled` and the identity on the far side of the confirmation, and
	 * a frozen object would defeat those re-checks. `workspaceSlug` comes off the
	 * captured `openWsSlug` for the same reason every other URL here does — a live
	 * read could name the wrong workspace after a pane switch.
	 */
	const toolbarCtx: AttachmentActionContext = {
		get workspaceSlug() {
			return openWsSlug;
		},
		get attachment() {
			return {
				id: img?.id ?? '',
				filename: displayName,
				// The RESOLVED MIME (3c-ii T3): the descriptor's Open (`canBrowserPreview`)
				// re-derives when a null-seed open's HEAD answers, exactly as the stage
				// arm does — the toolbar and the arm reclassify off the same value.
				mime_type: resolvedMime,
			};
		},
		get mutationsEnabled() {
			return mutationsEnabled;
		},
		confirmDelete: () => deleteConfirm.request(),
		// NO `onDeleted` (TASK-2477): the panel uses it to close on delete, but the
		// viewer reconciles a delete through the DELETION BUS instead — the
		// descriptor's `announceAttachmentDeleted` fires `handleDeletion`, which
		// tombstones the id and advances (or closes when nothing survives). That is
		// the SAME path an external delete takes, so an own-toolbar delete and a
		// strip/other-tab delete are indistinguishable to the survivor logic. The
		// C1 close-on-delete latch is retired: the viewer had no survivor logic then,
		// so closing was the only sane outcome; now it advances when survivors remain.
		onCopied: () => toastStore.show('Link copied to clipboard', 'success'),
	};

	let toolbarActions = $derived(attachmentActionsFor(toolbarCtx));

	// One disabled rule for every toolbar control (3c-ii T3): the descriptor's own
	// `enabled` (Delete without `mutationsEnabled`, Open on a non-previewable type),
	// OR an authoritative `missing` (404), OR an archived parent's still-pending
	// reachability probe. The template adds `toolbarBusy` for the mutating button.
	function toolDisabled(action: AttachmentAction): boolean {
		return !action.enabled(toolbarCtx) || missing || unreachablePending;
	}

	async function runToolbarAction(action: ButtonAttachmentAction) {
		if (toolDisabled(action)) return;
		// Fence the whole action against the shown identity (the SAME view fence the
		// metadata machine invalidates on a subject change). A confirmed delete of the
		// shown image advances `shownId` to a survivor synchronously inside `run`'s
		// announce, and a metadata `missing` for the shown image can advance it while a
		// SLOW delete of that same image is still awaiting — either way the view has
		// moved on, and this continuation must NOT write `toolbarBusy` / `toolbarError`
		// onto the survivor now on screen. The subject-change reset (the id-keyed effect)
		// clears the busy/error state for the new image; a stale continuation just
		// bows out. A same-image failure (delete rejected, no advance) is NOT stale, so
		// its error still shows against the image it belongs to.
		const token = headerMeta.viewFence.begin();
		toolbarError = null;
		toolbarBusy = true;
		try {
			await action.run(toolbarCtx);
			// A confirmed delete is reconciled through the deletion bus (`handleDeletion`
			// runs synchronously inside `run`'s announce) — advance or close — so nothing
			// to do here. Copy-link and the rest simply finish. The guards cover a
			// continuation landing after the bus close tore the viewer down (`destroyed`)
			// or after the view advanced off this image (`token.stale()`).
			if (destroyed || token.stale()) return;
		} catch (err) {
			if (destroyed || token.stale()) return;
			toolbarError =
				err instanceof Error ? err.message : `Couldn't ${action.label.toLowerCase()}`;
		} finally {
			// Only the still-current continuation clears the busy flag; a stale one leaves
			// it for the subject-change reset to zero, so it can't un-busy the survivor
			// mid-action.
			if (!destroyed && !token.stale()) toolbarBusy = false;
		}
	}

	// The delete drill-down's rows, as a proper ARIA menu (TASK-2474). The confirm
	// renders `role="menuitem"` buttons; a `role="menu"` parent is required, and a
	// menu manages focus by ROVING TABINDEX — exactly ONE row is in the tab order
	// (tabindex 0), the rest are -1, Up/Down move between them, and Tab moves OUT to
	// the viewer chrome rather than cycling the rows. The Lightbox hosts the shared
	// confirm inline (not inside a `Menu`), so it drives that itself.
	function confirmMenuItems(): HTMLElement[] {
		const el = rootEl;
		if (!el) return [];
		return Array.from(
			el.querySelectorAll<HTMLElement>('.lightbox-delete-confirm [role="menuitem"]')
		);
	}
	// Make `active` the single tab stop; every other row leaves the tab order. Tab
	// then exits the menu (the trap's `paneFocusables` skips the -1 rows), while
	// the active row can still be Tabbed BACK into — the ARIA menu contract.
	function applyRoving(active: HTMLElement | null) {
		const items = confirmMenuItems();
		if (items.length === 0) return;
		const target = active && items.includes(active) ? active : items[0];
		for (const it of items) it.tabIndex = it === target ? 0 : -1;
	}
	function moveConfirmFocus(delta: number) {
		const items = confirmMenuItems();
		if (items.length === 0) return;
		const at = items.indexOf(document.activeElement as HTMLElement);
		const next = at < 0 ? 0 : (at + delta + items.length) % items.length;
		const target = items[next];
		applyRoving(target);
		target?.focus({ preventScroll: true });
	}

	// When the drill-down OPENS, set the roving tab stop and move focus to its first
	// row (Cancel), as the panel's `Menu` does — otherwise focus is stranded on the
	// now-unmounted Delete button and the generic handoff would land it on Close,
	// not Cancel. Plain sentinel so this fires only on the false→true edge, and
	// reads `pending` (tracked) while writing only DOM (focus + tabindex) and the
	// sentinel, never its own trigger.
	let confirmWasPending = untrack(() => deleteConfirm.pending);
	$effect(() => {
		const pending = deleteConfirm.pending;
		if (pending === confirmWasPending) return;
		confirmWasPending = pending;
		if (!pending) return;
		untrack(() => {
			const first = confirmMenuItems()[0] ?? null;
			applyRoving(first);
			first?.focus({ preventScroll: true });
		});
	});

	// ── Image loading (PLAN-2392 phase 3b / TASK-2459) ────────────────────────
	//
	// The DR-5b thumb-then-original policy. The loader owns which URL the <img>
	// shows (`displaySrc`, the canonical attachment URL) and the load phase; this
	// component drives it from the shown image and reports each decode / error.
	const loader = createViewerImageLoader();
	// The id AND the pixel dimensions, as ONE stable primitive string — never the
	// `img` object. A prop re-emit with the same VALUES (a re-derived `navigable`
	// array) must not re-fire the load effect, but a genuine dimension change (an
	// async metadata fill that flips `unknown` → a sized class) MUST: the DR-5b
	// policy is a function of the pixels, so a stale dimension is a stale policy.
	// A shrink to no image collapses to `'::'`, still a change → the effect fires
	// and releases the load. THE ARM JOINS THE KEY (TASK-2476): a same-id
	// safe→unsafe MIME flip keeps id + dims identical, so without the renderer in
	// the key the load effect would not re-fire and the SAFE bytes would stay on
	// screen behind the fallback arm — the no-bytes invariant depends on this.
	// `soleMissing` joins the key too: a single-item image whose 404 lands flips it
	// true, and the load effect must re-fire to DISPOSE the loader (no bytes for a
	// gone file) and let the missing overlay take the stage.
	// The T6 `openNonce` deliberately does NOT join this key: a reopen is a WHOLE
	// new `{#key request}` mount (fresh loader, empty state), so cross-open
	// coherence is the remount's job, not the key's — and the nonce is constant
	// within one open, so adding it would be inert anyway. The same-id
	// safe→unsafe MIME flip that DOES need in-open coherence is already covered by
	// `shownRenderer` above.
	let loadKey = $derived(
		`${img?.id ?? ''}:${img?.width ?? ''}:${img?.height ?? ''}:${shownRenderer ?? ''}:${soleMissing}`
	);
	// Captured NON-reactively at load time (see the effect): a breakpoint flip
	// alone must not reload — desktop→mobile must not abort an in-flight original,
	// mobile→desktop must not retroactively auto-fetch (TASK-2459).
	let platform = $derived<Platform>(viewport.isMobile ? 'mobile' : 'desktop');
	// THE MOBILE SHEET LAYOUT SELECTOR (PLAN-2392 3c-ii / T5, AM-3). Off the SAME
	// `viewport.isMobile` the platform reads, so the phone-sheet layout switches at
	// the one app breakpoint. It toggles the `.lightbox-sheet` class on the root
	// (below) and the stylesheet re-lays-out the existing chrome from it — a class,
	// NOT a bare `@media`, deliberately: JS and CSS then share one breakpoint, and
	// the flip is a DOM fact the modal-contract suite can drive and observe under
	// its viewport mock. This is layout-only; the modal contract, the loader and the
	// zoom state below are all layout-independent, so a flip mid-open re-lays out the
	// SAME instance without touching any of them (no `{#key}` keys off it — that is
	// what preserves the zoom/selection state across a breakpoint flip). Reading a
	// store getter in a `$derived` is not an effect self-write (CONVE-1688).
	let isSheet = $derived(viewport.isMobile);
	// Whether a decoded bitmap exists to zoom / pan. False in the mobile `deferred`
	// cell (a placeholder, nothing decoded), when there is no image, AND in the
	// `error` state: `errored()` flips only the phase, leaving `displaySrc` set (the
	// failed URL), so without the phase guard drag-arming and the zoom keys would
	// act over the error UI with nothing decoded behind it. Zoom is then DISABLED,
	// not merely a no-op (TASK-2460). A successful retry returns to loading/ready
	// and re-enables it.
	// ...and only on the RASTER arm (TASK-2476): the fallback arm has nothing
	// decoded to transform, so zoom / pan / the zoom keys are disabled over it.
	let bitmapPresent = $derived(
		shownRenderer === 'raster-image' && !!img && !!loader.displaySrc && loader.phase !== 'error'
	);

	// RELEASE THE BYTES AT UNMOUNT (TASK-2476). Disposing the loader clears its
	// state, but the `<img>` Svelte is about to remove keeps its `src` — and thus a
	// possibly in-flight native request and its decoded bitmap — alive on the
	// detached element until GC. Clearing the src in the element's own `destroy`
	// aborts that request the instant the element leaves the tree, on EVERY unmount
	// path: the arm flip to fallback, a nav to another image, and close. Belt to
	// the loader's dispose; together the no-bytes invariant holds without waiting
	// on the garbage collector.
	function releaseImg(node: HTMLImageElement) {
		return {
			destroy() {
				node.removeAttribute('src');
			},
		};
	}
	// The tap-to-load placeholder's box. Where dimensions are known it takes the
	// image's own aspect ratio (so the affordance previews the shape that will
	// arrive); where they are not, a neutral box (TASK-2460).
	let placeholderStyle = $derived(
		img?.width && img?.height
			? `aspect-ratio: ${img.width} / ${img.height}; width: min(70vw, 520px); max-width: 90%; max-height: 80%;`
			: `width: min(60vw, 360px); height: min(45vh, 270px); max-width: 90%; max-height: 80%;`
	);

	// The portaled root. `$state` so the effect below re-runs once `bind:this`
	// lands; read-only inside every effect, so nothing here can self-invalidate
	// a flush (CONVE-1688).
	let rootEl = $state<HTMLElement | null>(null);

	// ── Zoom / pan (PLAN-2392 phase 3b / TASK-2455) ──────────────────────────
	//
	// The transform is `translate(x,y) scale(scale)` on the <img>, about the
	// stage centre. Every number comes from `$lib/attachments/zoom` (TASK-2454),
	// which owns the arithmetic and its bounds; this component only MEASURES the
	// rendered geometry, wires the keys, and re-clamps on resize.
	let zoom = $state<ZoomState>(resetZoom());
	// The stage is the 92vw×92vh box the bare <img> used to be; the <img> sits
	// inside it, `object-fit: contain`. Both are read live for geometry — never
	// through `getBoundingClientRect()` on the transformed image, which returns
	// the POST-scale box and would make the pan bounds grow with the zoom.
	let stageEl = $state<HTMLElement | null>(null);
	let imgEl = $state<HTMLImageElement | null>(null);

	/**
	 * The measured geometry, or null before there is anything to measure.
	 *
	 * `offsetWidth` / `offsetHeight` are the UNSCALED layout box — transforms do
	 * not touch them, which is the whole reason they, not `getBoundingClientRect`,
	 * are the source here. A not-yet-decoded bitmap reads back all zeros; the zoom
	 * module is defensive about that and still returns an in-bounds transform, so
	 * no guard is needed here.
	 */
	function readGeometry(): Geometry | null {
		const stage = stageEl;
		const image = imgEl;
		if (!stage || !image) return null;
		return {
			stageW: stage.clientWidth,
			stageH: stage.clientHeight,
			fittedW: image.offsetWidth,
			fittedH: image.offsetHeight,
			naturalW: image.naturalWidth,
			naturalH: image.naturalHeight,
		};
	}

	// `+` / `-` zoom about the stage centre. Reading and writing `zoom` from an
	// EVENT handler is fine — the CONVE-1688 rule is about `$effect`s that read
	// the state they write, not about handlers.
	function stepZoom(factor: number) {
		const g = readGeometry();
		if (!g) return;
		zoom = zoomTo(zoom, zoom.scale * factor, stageCenter(g), g);
		rebaseDrag(); // keyboard zoom mid-drag must not desync the pan baseline
	}

	// Wheel / ctrl-cmd-wheel zoom, anchored at the CURSOR (TASK-2457 / DR-4). Both
	// plain AND ctrl/cmd wheel zoom, so there is no modifier gate — but the
	// listener is registered NON-PASSIVELY (see the effect below) so
	// `preventDefault` takes effect: the inert page behind must not scroll, and
	// ctrl/cmd+wheel must not trigger the browser's own page zoom. `stopPropagation`
	// as well, so the page's scroll-restoration listener does not count this as a
	// user scroll (belt; `restore.svelte.ts` also ignores viewer input — braces).
	function onWheel(e: WheelEvent) {
		const el = rootEl;
		// Same gates as `onKeydown`: only the frontmost, non-blocked viewer acts.
		if (!el || !isViewerFrontmost(el) || isBlockedByModal(el)) return;
		// We own the wheel while frontmost — consume it even before the bitmap is
		// measurable, so a scroll can never leak past the modal into the inert app.
		e.preventDefault();
		e.stopPropagation();
		// A wheel over the TOOLBAR (or its delete drill-down) is consumed like every
		// other wheel — the modal owns it, so the inert page can't scroll — but must
		// NOT zoom the image behind it (TASK-2474). The same exclusion the pointerdown
		// and double-click handlers carry, applied here too.
		if ((e.target as Element | null)?.closest?.('.lightbox-toolbar, .lightbox-meta')) return;
		// Consumed (the modal owns the wheel) but INERT with no decoded bitmap — the
		// mobile deferred placeholder or the error UI, where the broken `<img>` still
		// satisfies `readGeometry` (TASK-2461). Same guard the keys use.
		if (!bitmapPresent) return;
		// A horizontal-only wheel (`deltaY === 0`, e.g. a trackpad side-swipe) is
		// still consumed — the modal owns the wheel — but must NOT zoom, or it would
		// read as a zoom-out. Direction comes from `deltaY` alone.
		if (e.deltaY === 0) return;
		const g = readGeometry();
		const rect = stageEl?.getBoundingClientRect();
		if (!g || !rect) return;
		// Anchor in stage-local px (top-left origin) — the coordinate system the
		// zoom module documents. The stage is untransformed, so its rect is stable.
		const anchor = { x: e.clientX - rect.left, y: e.clientY - rect.top };
		// Wheel up / away (deltaY < 0) zooms in.
		const factor = e.deltaY < 0 ? ZOOM_STEP : 1 / ZOOM_STEP;
		zoom = zoomTo(zoom, zoom.scale * factor, anchor, g);
		// A wheel DURING a captured drag just moved `zoom.{x,y}` from this pointer
		// position — rebase so the next pointermove continues from here.
		lastClientX = e.clientX;
		lastClientY = e.clientY;
		rebaseDrag();
	}

	// ── Double-click toggle + drag-to-pan (PLAN-2392 / TASK-2458) ────────────
	//
	// Desktop single-pointer half of the drag-pan work (3d keeps two-pointer
	// pinch, double-TAP and touch semantics). Modelled on the captured-drag house
	// pattern at `graph/ItemGraph.svelte` — arm on pointerdown, capture only once a
	// real drag engages (capturing on down would swallow the dblclick), re-check
	// the arbitration gates on every move so a gesture that STRADDLES a
	// frontmost/modal change aborts instead of panning an image the user no longer
	// owns, and suppress the synthesized click a pan produces.
	const DRAG_THRESHOLD = 4; // CSS px below which the gesture is a click, not a pan
	let maybeDrag = false;
	// `$state` so the template can drop the transform TRANSITION while dragging —
	// otherwise the image eases toward each pan target and visibly trails the
	// pointer. Read/written only in pointer handlers (never an $effect's tracked
	// scope), so no CONVE-1688 self-write.
	let dragging = $state(false);
	let suppressClick = false;
	let capturedPointerId: number | null = null;
	// The pointer that OWNS the current gesture, captured at pointerdown. Every
	// later move/up/cancel/lost must come from it: a touch or a second pointer
	// (pen) is not captured, so its events still reach this root and would
	// otherwise engage or terminate an in-flight mouse drag (touch stays native
	// until 3d).
	let gesturePointerId: number | null = null;
	// THE POINTER REGISTRY (PLAN-2392 phase 3d / TASK-2517). Every primary pointer
	// that presses on the backdrop — mouse, pen AND touch — enters here keyed by
	// `pointerId` with its down position + type; every up / cancel / capture-loss /
	// teardown removes it. It is the multi-pointer plumbing V2's pinch needs (two
	// live touch points at once), shipped INERT: V1 reads it for nothing, it only
	// keeps the active-pointer set honest. The single-pointer drag still keys its
	// OWNERSHIP off `gesturePointerId` above and its capture off `capturedPointerId`
	// — the registry is their superset, and in the mouse-only case it holds exactly
	// that one entry (the "1-entry case" the existing mouse suite pins byte-for-byte).
	// A plain `let`, NOT `$state`: gesture internals are imperative, read/written
	// only in pointer handlers + their teardowns, so no CONVE-1688 self-write; and
	// instance-scoped (like every gesture scalar here) so stacked viewers never
	// share a registry. V1 stores the DOWN position only — live position tracking is
	// V2's (pinch) to add on move.
	let registry = new Map<number, { x: number; y: number; type: string }>();
	let dragStartClientX = 0;
	let dragStartClientY = 0;
	let dragOriginX = 0; // zoom.x at drag baseline
	let dragOriginY = 0; // zoom.y at drag baseline
	let lastClientX = 0; // last pointer position seen during the gesture
	let lastClientY = 0;

	// Re-baseline the gesture to the CURRENT zoom + last pointer position. The drag
	// computes `origin + total delta`, so anything that moves `zoom.{x,y}` from
	// OUTSIDE the drag — wheel, `+`/`-`/`0` keys, a resize re-clamp — must rebase,
	// or the next move snaps the pan back to the pre-change origin (a visible jump).
	// Rebases an ARMED gesture too (not only a live drag): a zoom that lands
	// between pointerdown and the drag threshold would otherwise leave the engage
	// baseline stale. A no-op when no gesture is in flight.
	function rebaseDrag(): void {
		if (!maybeDrag && !dragging) return;
		dragOriginX = zoom.x;
		dragOriginY = zoom.y;
		dragStartClientX = lastClientX;
		dragStartClientY = lastClientY;
	}
	// A SINGLE owned timer for clearing `suppressClick`, so an EARLIER gesture's
	// pending clear can never fire during a LATER one and unsuppress its pan's
	// click. `cancelSuppressClear` runs whenever a new gesture is armed or a drag
	// engages (the flag is held true for the whole drag); `armSuppressClear` runs
	// once at the END of a pan to drop it on the next tick, after the synthesized
	// click has been (and gone).
	let suppressClickTimer: ReturnType<typeof setTimeout> | null = null;
	function cancelSuppressClear(): void {
		if (suppressClickTimer !== null) {
			clearTimeout(suppressClickTimer);
			suppressClickTimer = null;
		}
	}
	function armSuppressClear(): void {
		suppressClick = true;
		cancelSuppressClear();
		suppressClickTimer = setTimeout(() => {
			suppressClick = false;
			suppressClickTimer = null;
		}, 0);
	}

	// The same gates `onKeydown` / `onWheel` carry: only the frontmost, non-blocked
	// viewer owns the pointer. Applied at EVERY entry point (down / move / up /
	// dblclick / backdrop click), not just the start.
	function pointerGatesOpen(el: HTMLElement): boolean {
		return isViewerFrontmost(el) && !isBlockedByModal(el);
	}

	function releaseCapture(e: PointerEvent): void {
		if (capturedPointerId !== null) {
			try {
				(e.currentTarget as Element).releasePointerCapture(capturedPointerId);
			} catch {
				// already released — ignore.
			}
			capturedPointerId = null;
		}
	}

	// Tear a gesture down mid-flight: release the capture (an early return alone
	// would leave the pointer captured and `dragging` latched, still delivering
	// moves), and — if a real pan was underway — still swallow the click it
	// produces. The transform is LEFT WHERE IT WAS (the abort does not undo pan).
	function abortGesture(e: PointerEvent): void {
		registry.delete(e.pointerId); // reconcile the registry with the gesture teardown
		const wasDragging = dragging;
		maybeDrag = false;
		dragging = false;
		gesturePointerId = null;
		releaseCapture(e);
		if (wasDragging) armSuppressClear();
	}

	// The event-less twin of `abortGesture`, for a REACTIVE teardown (TASK-2476):
	// the bitmap can vanish (the arm flips to fallback) with NO further pointer
	// event to carry the abort — the user holds still while the prop changes. This
	// tears the same state down and releases the capture through the root (no
	// `currentTarget` to hand `abortGesture`), so a stale gesture can neither pan
	// the reloaded image after an A→unsafe→A flip nor leak into the next press.
	function cancelGesture(): void {
		// Registry hygiene runs UNCONDITIONALLY, BEFORE the no-gesture early return
		// (round-2 P1): the bitmap has vanished, so no pointer can be gesturing over
		// it. Drain the whole set — the mouse owner (if any) AND any touch pointers,
		// which arm nothing in V1 but would otherwise leak here if the arm flip beats
		// their pointerup/cancel, poisoning later pinch detection. Per-instance, so
		// this never clears another viewer's registry. V2's pinch/tap-timer/baseline
		// teardown joins this single path.
		registry.clear();
		if (!maybeDrag && !dragging) return;
		const wasDragging = dragging;
		maybeDrag = false;
		dragging = false;
		gesturePointerId = null;
		if (capturedPointerId !== null) {
			try {
				rootEl?.releasePointerCapture(capturedPointerId);
			} catch {
				// already released — ignore.
			}
			capturedPointerId = null;
		}
		if (wasDragging) armSuppressClear();
	}

	// TEST-ONLY (TASK-2517). The pointer registry is inert plumbing in V1, so a
	// leak has NO V1-observable consequence — there is no indirect invariant that
	// would catch it. This accessor lets the jsdom suite assert the drain invariant
	// directly (registry empties on pointerup AND pointercancel, never leaks). Not
	// used by production code. Exposed on the mount() result / `bind:this`.
	export function __registrySize(): number {
		return registry.size;
	}

	function onPointerDown(e: PointerEvent) {
		if (e.button !== 0) return; // primary button only
		// EVERY primary pointer — mouse, pen AND touch — enters the registry FIRST,
		// before any arm / gate / early-return below. `onPointerUp`/`onPointerCancel`
		// delete by id before their own guards, so a press that never arms (touch,
		// chrome, gated, no-bitmap) still drains cleanly (round-2 P1).
		registry.set(e.pointerId, { x: e.clientX, y: e.clientY, type: e.pointerType });
		// V1 (TASK-2517): a TOUCH press enters the registry but arms NO drag and takes
		// NO capture — touch pan is arbitration-unsound under `touch-action: auto` (the
		// browser can claim the gesture and `pointercancel` mid-stream), so it is V2's.
		// Taps still work: arming nothing, this press falls through to backdrop
		// `onclick` (close), the chrome exclusion, or the deferred tap-to-load button —
		// none of which the registry entry disturbs.
		if (e.pointerType === 'touch') return;
		// A second pointer (a pen, say) pressing mid-drag must NOT seize the gesture:
		// re-arming below would replace `gesturePointerId` and hand the pan to the
		// interloper. An active drag is owned until its own pointer releases.
		if (dragging) return;
		// A new primary press supersedes any STALE armed gesture — one whose
		// pointerup was missed off-root (no capture yet, so it was never delivered).
		// Clear it before any early return below, or a later control / gated press
		// leaves `maybeDrag` latched and the next move engages a phantom drag from a
		// dead baseline. (We already returned above if a drag is live.)
		//
		// Reconcile the REGISTRY at the SAME point (round-2 P1 leak-freedom): that
		// stale owner's missed off-root up never ran `registry.delete`, so its entry
		// would linger. Drop it here — but only when it is a DIFFERENT id than this
		// press (a same-id re-press, e.g. a mouse, already refreshed its own entry via
		// the `registry.set` at the top; deleting it would drop the live one). This
		// mirrors the owner-scalar cleanup: the registry is reconciled at every
		// DELIVERED interaction (this press, the buttons-released move, cancel, lost),
		// which is the strongest the event model allows without capture.
		if (gesturePointerId !== null && gesturePointerId !== e.pointerId) {
			registry.delete(gesturePointerId);
		}
		maybeDrag = false;
		// A press ON a control (close / nav / the DR-10 retry) is that control's
		// click, never a pan: arming here would let a drag OFF a button still fire
		// its click, since the buttons' own handlers don't consult `suppressClick`
		// (the house pattern in ItemGraph excludes its interactive overlays the same
		// way). Retry sits over the (broken) stage in the error state, so it needs
		// the same exclusion as the always-present chrome.
		if ((e.target as Element | null)?.closest?.('.lightbox-close, .lightbox-nav, .lightbox-retry, .lightbox-tap-load, .lightbox-toolbar, .lightbox-meta')) return;
		// No decoded bitmap to pan (the mobile `deferred` placeholder shows no
		// `<img>`) — do NOT arm a drag, so the gesture stays fully inert instead of
		// capturing the pointer and latching `dragging` for a pan that can never
		// happen (TASK-2460). A plain press still reaches the backdrop to close.
		if (!bitmapPresent) return;
		const el = rootEl;
		if (!el || !pointerGatesOpen(el)) return; // START gate
		maybeDrag = true;
		dragging = false;
		gesturePointerId = e.pointerId;
		suppressClick = false;
		cancelSuppressClear(); // a fresh gesture — drop any prior pan's pending clear
		dragStartClientX = e.clientX;
		dragStartClientY = e.clientY;
		lastClientX = e.clientX;
		lastClientY = e.clientY;
		// Capture the pan origin from the state at pointer-down, so every move
		// computes origin + TOTAL delta — never delta-of-delta, which the zoom
		// module warns loses the slack at a bound (its "gesture is not associative"
		// note). No `setPointerCapture` yet: capturing here would swallow dblclick.
		dragOriginX = zoom.x;
		dragOriginY = zoom.y;
	}

	function onPointerMove(e: PointerEvent) {
		if (!maybeDrag) return;
		if (e.pointerId !== gesturePointerId) return; // only the owning pointer drives
		const el = rootEl;
		// WHOLE-GESTURE arbitration: the press may have been captured before another
		// viewer / a native modal became frontmost. Abort rather than early-return.
		if (!el || !pointerGatesOpen(el)) {
			abortGesture(e);
			return;
		}
		// The bitmap vanished mid-drag — the background original failed and the phase
		// went to `error` while a pan was live (TASK-2461). Abort: there is nothing
		// left to pan, and continuing would move the (broken) error UI.
		if (!bitmapPresent) {
			abortGesture(e);
			return;
		}
		// Primary button no longer held — a pointerup we never received (it ended
		// off-target pre-capture, or was swallowed after a drag engaged). Tear the
		// whole gesture down, not just `maybeDrag`: a full `abortGesture` also
		// releases any capture and clears `dragging`, so nothing leaks into the next
		// pointerdown (a bare `maybeDrag = false` left a live capture + `dragging`).
		if ((e.buttons & 1) === 0) {
			abortGesture(e);
			return;
		}
		lastClientX = e.clientX;
		lastClientY = e.clientY;
		const dx = e.clientX - dragStartClientX;
		const dy = e.clientY - dragStartClientY;
		if (!dragging) {
			if (Math.hypot(dx, dy) < DRAG_THRESHOLD) return;
			dragging = true;
			// This gesture is a pan, not a click: suppress until the drag ENDS. Held
			// true for the whole drag; cancel any prior gesture's pending clear so it
			// can't fire mid-pan and unsuppress us.
			suppressClick = true;
			cancelSuppressClear();
			capturedPointerId = e.pointerId;
			try {
				(e.currentTarget as Element).setPointerCapture(e.pointerId);
			} catch {
				// capture unsupported/failed — panning still works while over the root
			}
		}
		const g = readGeometry();
		if (!g) return;
		// Clamp PAN only — the drag never changes scale — from the captured origin
		// plus the total delta. `clampPan` keeps a stage edge from showing past the
		// image; at fit the bound is zero, so a drag is a no-op pan.
		zoom = clampPan({ scale: zoom.scale, x: dragOriginX + dx, y: dragOriginY + dy }, g);
	}

	function onPointerUp(e: PointerEvent) {
		registry.delete(e.pointerId); // hygiene FIRST — before the owner guards below,
		// so a foreign / never-armed pointer (touch, chrome, gated) still drains and
		// never leaks (round-2 P1).
		if (!maybeDrag && !dragging) return; // not a gesture we started
		if (e.pointerId !== gesturePointerId) return; // a foreign pointer can't end ours
		const el = rootEl;
		// Re-check on release too (spec): a gesture that straddled a
		// frontmost/modal change aborts rather than finalising a pan.
		if (!el || !pointerGatesOpen(el)) {
			abortGesture(e);
			return;
		}
		// The bitmap vanished before release — the shown entry flipped to the
		// fallback arm (or errored) while the pointer was down (TASK-2476). ABORT,
		// don't finalise: there is nothing to pan, and finalising would leave the
		// suppress-click machinery armed over a stage with no image.
		if (!bitmapPresent) {
			abortGesture(e);
			return;
		}
		maybeDrag = false;
		gesturePointerId = null;
		if (dragging) {
			dragging = false;
			releaseCapture(e);
			// Keep the click this pan produces swallowed, then clear on the next tick
			// so a later genuine click is unaffected.
			armSuppressClear();
		}
	}

	function onPointerCancel(e: PointerEvent) {
		registry.delete(e.pointerId); // hygiene FIRST — under `touch-action: auto` the
		// browser claims and `pointercancel`s touches ROUTINELY in the V1 window; a
		// literal owner-guarded early-return would leak every one of them (round-2 P1).
		if (!maybeDrag && !dragging) return; // no gesture to cancel
		if (e.pointerId !== gesturePointerId) return;
		abortGesture(e);
	}

	function onLostPointerCapture(e: PointerEvent) {
		registry.delete(e.pointerId); // reconcile the registry on capture loss too (a
		// real pointerup already deleted it, so this is a no-op there; it matters for an
		// OS/browser-stolen capture that arrives with no up).
		if (!maybeDrag && !dragging) return; // no gesture; ignore a stray capture-loss
		if (e.pointerId !== gesturePointerId) return;
		// Capture taken away (OS/browser, or our own release) — the gesture is over.
		// Clear WITHOUT releasing (already gone); keep the click suppressed if a pan
		// was underway. Idempotent with `onPointerUp`, which sets `dragging` false
		// before releasing, so a release-driven event here is a no-op.
		const wasDragging = dragging;
		maybeDrag = false;
		dragging = false;
		capturedPointerId = null;
		gesturePointerId = null;
		if (wasDragging) armSuppressClear();
	}

	// Double-click toggles fit <-> actual size, anchored at the pointer
	// (`toggleFitOrActual`). A pan suppresses its clicks, so a dblclick only fires
	// on a genuine double-click, never after a drag.
	function onDoubleClick(e: MouseEvent) {
		// A pan just ended (its click is being swallowed) — don't also toggle: a
		// gesture is either a drag or a double-click, never both. Same swallow the
		// backdrop click consults.
		if (suppressClick) return;
		// A double-click ON a control (close / nav / retry) is that control's, not a
		// zoom toggle — without this, double-clicking Next navigates twice AND
		// toggles, or a double-click on Retry toggles zoom on the broken stage.
		if ((e.target as Element | null)?.closest?.('.lightbox-close, .lightbox-nav, .lightbox-retry, .lightbox-tap-load, .lightbox-toolbar, .lightbox-meta')) return;
		// Inert with no decoded bitmap (deferred placeholder / error UI) — the same
		// guard the keys and wheel use (TASK-2461).
		if (!bitmapPresent) return;
		const el = rootEl;
		if (!el || !pointerGatesOpen(el)) return;
		const g = readGeometry();
		const rect = stageEl?.getBoundingClientRect();
		if (!g || !rect) return;
		const anchor = { x: e.clientX - rect.left, y: e.clientY - rect.top };
		zoom = toggleFitOrActual(zoom, anchor, g);
	}

	// Move `shownId` (identity), stepped from the DERIVED `shownIndex` over the
	// SURVIVORS. Writing the id, not an index, is what keeps the viewer on a real
	// member when the set changes under it. No-ops on an empty set — reachable only
	// through the nav controls / arrow keys, which `hasMultiple` already hides and
	// gates, but written so the modulo can never be `% 0`.
	function prev() {
		if (survivors.length === 0) return;
		shownId = survivors[(shownIndex - 1 + survivors.length) % survivors.length].id;
	}
	function next() {
		if (survivors.length === 0) return;
		shownId = survivors[(shownIndex + 1) % survivors.length].id;
	}

	// THE DELETION SUBSCRIPTION (PLAN-2392 DR-5c / TASK-2477). ONE path for both
	// origins: the toolbar's own Delete announces on the bus (the descriptor's
	// `announceAttachmentDeleted`), and another in-page surface (the strip, a body
	// NodeView) announces the same way — so this reacts identically to both, and the
	// toolbar's ctx deliberately carries NO `onDeleted` (that close-on-delete is
	// the PANEL's; the viewer advances instead). The bus is PROCESS-LOCAL, so an
	// out-of-page delete (another tab, a job, the API) reaches this viewer as a
	// metadata `missing` phase instead — routed through here by the effect below.
	// An event handler, not an $effect, so writing `shownId` / `tombstones` here is
	// not a self-write.
	function handleDeletion(uuid: string) {
		if (!uuid || tombstones.has(uuid)) return;
		// Snapshot BEFORE tombstoning: the position of the deleted entry decides
		// where "advance" lands.
		const before = survivors;
		const i = before.findIndex((im) => im.id === uuid);
		// NOT in our surviving set: an unsafe-at-open / unresolved entry (D excluded
		// it), another item's attachment on the shared bus, or an already-dangling
		// `shownId` (a producer removed the shown image straight from the prop). Still
		// tombstone it — a delete is authoritative — but there is nothing to advance
		// to or close over, so DON'T touch `shownId` (its index derives to a survivor)
		// and DON'T read `after[-1]` or close an already-empty viewer.
		if (i < 0) {
			tombstones = new Set(tombstones).add(uuid);
			return;
		}
		const deletingShown = uuid === shownId;
		// Authoritative and latched for this viewer's life — reassign the Set so
		// `survivors` recomputes.
		tombstones = new Set(tombstones).add(uuid);
		const after = survivors; // recomputed, minus uuid
		if (after.length === 0) {
			// Nothing left to show — zero-left / deleting-only → close.
			onClose();
			return;
		}
		if (deletingShown) {
			// ADVANCE to the entry that FOLLOWED the deleted one — now at index `i` in
			// the shrunk list — wrapping to the first when the last was deleted. The
			// zoom transform resets through the existing id-keyed effect, since the
			// shown id changes here. Deleting an EARLIER/LATER entry leaves `shownId`
			// untouched: identity, not index, so the SAME image stays on screen.
			shownId = after[i < after.length ? i : 0].id;
		}
	}

	// AN AUTHORITATIVE 404 IS A DELETION BY ANOTHER DOOR (PLAN-2392 DR-17 /
	// 3c-i final fix). The deletion bus is PROCESS-LOCAL (`events.ts`), so a delete
	// from another browser tab, a background job, or a direct API call never
	// announces on it — but where the metadata machine PROBES the shown attachment,
	// its HEAD returns the `missing` phase, which is just as authoritative (DR-17).
	// Route that id through the SAME tombstone path so advance-or-close, the id-keyed
	// zoom reset, focus handling, and the confirm-abandon all come for free — rather
	// than leaving stale image bytes and live Open / Download / Copy / Delete on a
	// file that is gone. ONLY `missing` latches; a `transient` (non-404) stays
	// retryable and never gets here.
	//
	// SCOPE: the machine only HEAD-probes when a seed field is null (an inline / body
	// image seeds no size, so it probes; a strip image seeds mime AND size and is not
	// re-probed — `surfaceMetadata`'s "nothing to complete" short-circuit). So this
	// catches an out-of-page delete for a probed image; a strip-opened image whose
	// file is deleted elsewhere is reconciled only if that delete also crosses the
	// in-page bus. A blanket reachability probe for every viewer image is deliberately
	// NOT done here (a HEAD per open, for the uncommon cross-tab case) — 3c-ii owns
	// any always-revalidate decision.
	//
	// A sentinel + `untrack` keep it CONVE-1688-safe. The ONLY tracked read is
	// `headerMeta.phase`; `img?.id` is read untracked, so an arrow-nav that changes
	// the shown id without changing the phase can't re-fire this, and the advance's
	// subject change (which resets the machine's phase away from `missing`) is what
	// re-runs it for the NEXT id. `handleDeletion` is idempotent, so a whole gone set
	// cascades advance→advance→close, one 404 at a time, and terminates.
	let lastMissingId: string | undefined = undefined;
	$effect(() => {
		// A SINGLE-item surface (`images.length === 1`) does NOT route through the
		// tombstone path on a 404 — it shows the inert `soleMissing` overlay and
		// stays open (the retired panel's behavior). Only a MULTI-image set advances
		// / closes here, so the whole-set cascade is unchanged.
		const advanceMissing = missing && images.length > 1;
		untrack(() => {
			const missingId = advanceMissing ? img?.id : undefined;
			if (!missingId) {
				lastMissingId = undefined;
				return;
			}
			if (missingId === lastMissingId) return;
			lastMissingId = missingId;
			handleDeletion(missingId);
		});
	});

	/**
	 * Return focus where it came from, on close.
	 *
	 * Declines when focus has ALREADY moved somewhere outside this viewer —
	 * a surface opened over the viewer owns focus outright, and so does a
	 * producer that moves focus from its own close handler (which runs BEFORE
	 * this teardown). `AttachmentViewerHost` used to be exactly that producer;
	 * TASK-2429 moved the restore here precisely because its version ran while
	 * the invoker was still inert, so this guard is now about the surfaces the
	 * viewer does not control rather than about that host. Focus resting on
	 * `<body>` / nowhere is the adrift state a teardown leaves behind, and
	 * counts as ours to move.
	 *
	 * The invoker is verified rather than trusted: it can have been detached
	 * (an editor NodeView is re-rendered on any document change), hidden, or
	 * inerted since it was captured, and `focus()` on such an element silently
	 * does nothing on some engines and drops focus to `<body>` on others. So we
	 * focus it and CHECK, falling back to parking focus on `<body>` — which is
	 * where the browser would have put it anyway, but deterministically, and
	 * without leaving focus inside a subtree that is about to be removed.
	 */
	function restoreFocus(root: HTMLElement): void {
		const active = document.activeElement;
		if (active !== null && active !== document.body && !root.contains(active)) return;

		if (openInvoker?.isConnected) {
			openInvoker.focus?.({ preventScroll: true });
			if (document.activeElement === openInvoker) return;
		}
		(document.activeElement as HTMLElement | null)?.blur?.();
	}

	// ONE effect owns the whole modal contract, because the steps are ordered
	// with respect to each other and to teardown: portal → lease → focus entry →
	// Escape registration, unwound in reverse.
	$effect(() => {
		const el = rootEl;
		if (!el) return;

		// PORTAL TO <body> DIRECTLY — deliberately NOT `portalAction.ts`, which
		// targets the nearest ancestor `<dialog>` when there is one: exactly the
		// wrong target for a surface that must sit ABOVE everything. A `position:
		// fixed` overlay is only viewport-fixed while no ancestor establishes a
		// containing block, and `transform` / `filter` / `contain: layout` on any
		// ancestor silently does (see the container-query foot-gun). `<body>` is
		// the only parent with no such ancestor, and it is what the backdrop
		// manager's inert bookkeeping requires: it writes `inert` on BODY CHILDREN
		// and exempts this one.
		document.body.appendChild(el);

		// Background inertness is the manager's, not ours — no hand-rolled
		// `inert`, no `aria-hidden` sweep. It refcounts, so two viewers stacked
		// (the strip's and a NodeView's) can't clobber each other's writes.
		const lease = acquire(el);

		// Focus ENTRY goes to the first tabbable DESCENDANT (the close button),
		// not the root: a screen-reader user landing on the container has to
		// discover the controls, and the trap below cycles from wherever focus is.
		// The root is `tabindex="-1"` purely as the fallback for a viewer with no
		// tabbable control yet (single image, still loading) — focus must not stay
		// on whatever is behind the backdrop.
		const entry = paneFocusables(el)[0] ?? el;
		entry.focus({ preventScroll: true });

		// Escape has ONE owner: the shared stack. The local `<svelte:window>`
		// Escape branch this component used to carry was DELETED rather than
		// gated — it ignored `defaultPrevented`, so with the stack also running,
		// a single press closed the viewer AND the layer beneath it.
		//
		// Registered ABOVE `menu` (40) so the viewer is the innermost layer.
		// Declines (returns false) unless this viewer is the FRONTMOST lease, so
		// with two viewers open one press closes exactly the top one and the
		// stack falls through to it rather than to an unrelated layer.
		const unregisterEscape = pushEscapeHandler((event) => {
			if (!isViewerFrontmost(el)) return false;
			// The delete drill-down is an inner layer (TASK-2474): Escape backs OUT of
			// it to the toolbar, and only a second Escape closes the viewer — the
			// two-step the panel's Menu gives its own drill-down. Consume this press.
			if (deleteConfirm.pending) {
				if (event) noteEscapeConsumedByViewer(event);
				deleteConfirm.cancel();
				return true;
			}
			// Mark the DISPATCH before closing, not after (TASK-2448 / BUG-2441).
			// `onClose()` runs the teardown below synchronously — the lease is gone
			// by the time it returns — so a later `window` listener in this same
			// event would be told "no viewer" and close a second layer. The mark is
			// the only thing that survives that, and it has to be in place before
			// the state it stands in for is destroyed.
			if (event) noteEscapeConsumedByViewer(event);
			onClose();
			return true;
		}, ESCAPE_PRIORITY.viewer);

		// Subscribe to the deletion bus (TASK-2477). Registered alongside the lease so
		// it is DISPOSED in the same teardown — a leaked listener would fire
		// `shownId` / `onClose` writes into a torn-down viewer.
		const unregisterDeletion = registerAttachmentDeletionListener(handleDeletion);

		return () => {
			unregisterDeletion();
			unregisterEscape();
			// Release BEFORE restoring focus, and let the RESULT decide: when a
			// viewer remains beneath this one the manager has already handed focus
			// into it, and restoring our own invoker would yank focus out of a
			// viewer the user is still looking at. Only the last one out restores.
			const { stackEmpty } = lease.release();
			if (stackEmpty) restoreFocus(el);
			// Svelte removes the node itself, but it is reparented out of its
			// anchor — remove it explicitly so the DOM can never be left with a
			// stranded backdrop, and so the manager's next reconcile sees the
			// body child list it expects.
			el.remove();
		};
	});

	// Reset the transform whenever the SHOWN image changes — arrow nav, or a delete
	// that ADVANCES `shownId` to a survivor (TASK-2455 / TASK-2477). Keyed on
	// `img?.id`, so it fires exactly when a DIFFERENT image is shown: deleting an
	// earlier/later entry leaves `shownId` (and thus `img.id`) put, so the same
	// image's zoom survives; advancing onto a new one resets it. Close needs no
	// handling: every producer keys the mount, so closing unmounts this instance
	// and the next open starts from `resetZoom()`.
	//
	// `lastResetForId` is a PLAIN let, not `$state`: an effect that read and wrote
	// the same `$state` would self-depend and abort its own flush (CONVE-1688),
	// stranding unrelated reactivity nearby. This effect reads `img?.id` (tracked)
	// and writes `zoom` (which it never reads) plus this sentinel (a plain let, so
	// never tracked) — nothing it writes is anything it reads. Seeded to the
	// current id so the mount does not fire a redundant reset.
	let lastResetForId: string | undefined = untrack(() => img?.id);
	$effect(() => {
		const id = img?.id;
		if (id === lastResetForId) return;
		lastResetForId = id;
		zoom = resetZoom();
		// The shown image changed (arrow-nav, or a set-shrink moving a different
		// member into view), so a delete confirmation still up is for a file the user
		// is no longer looking at — abandon it (TASK-2474), exactly as the panel's
		// subject-change does. `cancel()` writes the delete-confirm MODULE's state,
		// not this effect's tracked `zoom`, so it cannot self-invalidate the flush.
		deleteConfirm.cancel();
		// Drop the toolbar's per-image action state too: an action that was in flight
		// on the PREVIOUS image (a slow delete raced by this advance, or the confirmed
		// delete that caused it) fenced itself out of the `finally` that would clear
		// `toolbarBusy`, so zero it here — otherwise the survivor shows "Deleting…" for
		// a request that isn't about it. `toolbarError` likewise belonged to the old
		// image. Both are `$state` this effect never reads, so no self-invalidation.
		toolbarBusy = false;
		toolbarError = null;
		// A drag live ACROSS the image change (arrow-nav mid-drag) is left with a
		// stale baseline, and deliberately so: `resetZoom` is fit, where `clampPan`
		// pins the pan to 0 for ANY baseline, so the next move can't jump; and the
		// next wheel/keyboard zoom rebases via `rebaseDrag`. Re-seeding here would be
		// unobservable — and calling `rebaseDrag` (which reads `zoom`) inside this
		// `zoom`-writing effect would self-invalidate it (CONVE-1688).
		//
		// The POINTER REGISTRY is deliberately NOT reconciled here (TASK-2517): a
		// shown-image change is nav / a set-shrink, not a pointer up/down, so the
		// physical pointer set is unchanged — the registry must keep tracking exactly
		// the pointers still down (clearing one still held would desync it, and its
		// eventual up would drain nothing). The bitmap-vanish teardown (below) owns
		// registry hygiene for the arm-flip case, via `cancelGesture`.
	});

	// (Re)load whenever the SHOWN image changes — nav, a dimension fill, or a set
	// shrink. This is also the ABORT + release point: `loader.load` drops the URL
	// the user left, and it re-runs on the set shrinking to empty (`loadKey` →
	// `'::'`) so a closed / emptied viewer holds no in-flight request. Reads only
	// `loadKey` (tracked, a stable string so a same-values prop re-emit doesn't
	// re-fire); `img`, `openWsSlug` and `platform` are captured non-reactively so a
	// breakpoint flip alone can't reload (TASK-2459).
	$effect(() => {
		void loadKey;
		untrack(() => {
			// THE NO-BYTES INVARIANT (TASK-2476). The raster arm loads; EVERY other arm
			// — the fallback (unsafe/no-preview) or no image at all — DISPOSES the
			// loader, so the arm flip releases any in-flight or decoded bitmap and
			// issues no request. `loadKey` carries the renderer, so a same-id
			// safe→unsafe flip actually re-runs this and reaches the dispose. The
			// loader's own DR-16 gate (`start()`) is the backstop; the Lightbox
			// decides no-bytes explicitly here.
			if (shownRenderer === 'raster-image' && !soleMissing) {
				// Pass the RESOLVED MIME, not the seed. A null-seed entry that
				// reclassified to raster (its HEAD resolved an image) still carries a
				// null seed `mime_type`, and the loader's own DR-16 gate (`start()`)
				// reads the img it is handed — it would refuse a null. `shownRenderer`
				// is already the resolved-MIME arm and is `'raster-image'` only for a
				// positively allowlisted resolved type, so handing the loader that same
				// resolved value keeps its gate agreeing with the arm rather than
				// refusing a row the arm admitted.
				loader.load({ ...img, mime_type: resolvedMime }, openWsSlug, platform);
			} else {
				loader.dispose();
			}
		});
	});
	// Drop the load on unmount (close) — one teardown, no dependencies.
	$effect(() => () => loader.dispose());

	// Reactively abort a live gesture the instant the bitmap goes away (TASK-2476).
	// The pointer handlers catch a flip that arrives WITH an event (move / up), but
	// the arm can flip to fallback from a PROP change while the pointer is held
	// still — no event to carry the abort. Reads `bitmapPresent` (tracked) and
	// tears the gesture down in `untrack` (it writes `dragging` etc., never read
	// here), so it cannot self-invalidate (CONVE-1688).
	$effect(() => {
		if (bitmapPresent) return;
		untrack(() => cancelGesture());
	});

	// Zoom-past-fit is the mobile THUMB cell's trigger to fetch the original
	// (TASK-2460): once a thumbnail has PAINTED, zooming past FIT (`scale > 1`, not
	// past the thumb's own 1:1) upgrades to the original. Threshold is
	// `!isAtFit(zoom)` — exactly "past fit", robust to the FIT_EPSILON float noise.
	// Depends on `zoom` AND `loader.painted` (both tracked): gating on `painted`
	// means a pre-paint zoom cannot fetch the original early, AND — because the flip
	// to painted RE-RUNS this effect — a zoom made WHILE the thumb was still loading
	// still upgrades the instant it paints (not stranded on the thumbnail).
	// `loadOriginal` writes neither `zoom` nor `painted`, so tracking them cannot
	// self-invalidate the flush (CONVE-1688). Fires on every zoom step past fit, but
	// `loadOriginal`'s own dedup makes all but the first a no-op — the original is
	// requested exactly once — and it no-ops entirely wherever nothing is deferred
	// (desktop, or an already-loaded cell).
	$effect(() => {
		if (isAtFit(zoom) || !loader.painted) return;
		untrack(() => loader.loadOriginal());
	});

	// Re-clamp on stage resize. `maxScale` is geometry-dependent and geometry is
	// viewport-dependent, so ENLARGING the window lowers the ceiling and can
	// strand a previously-valid scale above it. `clampState` re-clamps SCALE
	// first, then pan — the order a geometry change needs; clamping pan first
	// would bound it against a scale that is about to change. The callback runs
	// outside any tracked scope, so its `zoom` read/write is not a self-write.
	// Guarded for environments without `ResizeObserver` (SSR); the jsdom test
	// project ships a global stand-in (`src/test/setup-jsdom.ts`).
	$effect(() => {
		const stage = stageEl;
		if (!stage || typeof ResizeObserver === 'undefined') return;
		const ro = new ResizeObserver(() => {
			const g = readGeometry();
			if (g) {
				zoom = clampState(zoom, g);
				rebaseDrag(); // a resize re-clamp mid-drag must not desync the baseline
			}
		});
		ro.observe(stage);
		return () => ro.disconnect();
	});

	// Register the wheel listener imperatively with `{ passive: false }` so
	// `preventDefault` is guaranteed to take effect (a declarative `onwheel`
	// binding's passivity is not something to rely on), and on the viewer ROOT so
	// a wheel over the backdrop letterbox is consumed too — scrolling "past" the
	// modal into the inert app is exactly what the contract forbids (TASK-2457).
	$effect(() => {
		const el = rootEl;
		if (!el) return;
		el.addEventListener('wheel', onWheel, { passive: false });
		return () => el.removeEventListener('wheel', onWheel);
	});

	// aria-modal promises focus never leaves the surface, but a focused nav button
	// is CONDITIONALLY rendered (`hasMultiple`) — when the set shrinks to one it
	// unmounts, dropping focus to <body> behind the inerted app until the next Tab
	// (TASK-2456; a 3a defect every control 3b adds inherits). Keyed on the
	// control-visibility signal, this hands focus back to the stable fallback
	// (close button, else root) within the SAME flush the removal happens in, so
	// <body> is never observably focused. `handoffFocus` with no `departing` is
	// the reactive-removal shape; TASK-2459/2460 will call the same helper with
	// THEIR departing control before disabling it.
	//
	// Guarded to the frontmost, non-blocked viewer: a surface stacked OVER this
	// one owns focus, and pulling it back here would be the wrong direction (the
	// same reason the key handler stands down for those). Reads only derived /
	// element state and mutates focus — a DOM side effect, not $state — so it
	// cannot self-invalidate its own flush (CONVE-1688).
	$effect(() => {
		// Tracked so the effect re-runs whenever a focus-holding control mounts or
		// unmounts: the nav buttons (`hasMultiple`), the phase-gated controls (the
		// DR-10 retry button in `error`, the tap-to-load button in `deferred` — both
		// replaced by the image they load, TASK-2459 / TASK-2460), and the image
		// itself (`img`). A set shrinking to empty, an errored image navigated away,
		// or a tap that swaps the affordance for the bitmap could otherwise strand
		// focus on <body>. Tracking the whole `phase` covers every such transition;
		// `handoffFocus` no-ops while focus is still inside the surface.
		void hasMultiple;
		void loader.phase;
		void img;
		// The toolbar's drill-down swaps the action buttons for the confirm rows and
		// back (TASK-2474); the control that had focus (the Delete button, or Cancel)
		// unmounts across that switch, so track `pending` to re-home a focus stranded
		// on <body> to the stable fallback within the same flush.
		void deleteConfirm.pending;
		// The metadata header's Retry button unmounts the instant its own click
		// clears the transient phase (TASK-2475), so a keyboard user who pressed it is
		// left on <body>; track the transient flag to re-home them the same way.
		void headerTransient;
		// The stage arm swap (raster ↔ fallback, TASK-2476) unmounts whatever raster
		// control had focus — the Retry/tap-load button — and the fallback arm has no
		// focusable content, so track the arm to re-home a stranded focus.
		void shownRenderer;
		const el = rootEl;
		if (!el || !isViewerFrontmost(el) || isBlockedByModal(el)) return;
		handoffFocus(el);
	});

	function onKeydown(e: KeyboardEvent) {
		// A control that already handled this key owns it.
		if (e.defaultPrevented) return;
		const el = rootEl;
		// Every mounted viewer listens on `window`, so ONLY the frontmost may act.
		// This matters more than the usual layer-isolation argument: `nextTrapTarget`
		// deliberately pulls out-of-container focus back INWARD, so a background
		// viewer running the trap would drag focus into itself, out of the viewer
		// in front of it.
		if (!el || !isViewerFrontmost(el)) return;
		// ...and a `showModal()` dialog opened OVER the viewer owns the top layer,
		// above any body-portaled surface, so the frontmost LEASE is not
		// necessarily the frontmost SURFACE. Without this the viewer's trap would
		// fight a native modal for Tab and pull focus back out of it — the exact
		// inward-redirect hazard the frontmost gate exists to prevent, one layer
		// up. The manager already keeps such a dialog out of the inert set
		// (`keepInteractiveAsDialog`), so it is reachable and this is real.
		if (isBlockedByModal(el)) return;

		if (e.key === 'Tab') {
			// Reuses the pane's tested trap math (paneFocus.ts) — one trap
			// implementation for the whole app, not a second one here.
			const target = nextTrapTarget(
				paneFocusables(el),
				document.activeElement,
				e.shiftKey,
				el
			);
			if (target) {
				e.preventDefault();
				target.focus({ preventScroll: true });
			}
			return;
		}

		// The delete drill-down owns the arrow keys as a MENU while it is open
		// (TASK-2474): Up/Down move between its rows, and it consumes Left/Right so a
		// confirmation can't page the gallery behind it. Tab still cycles the rows
		// through the trap above; Escape backs out (via the escape stack). Returns
		// for every key, so zoom keys are inert over the confirm too.
		if (deleteConfirm.pending) {
			if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
				e.preventDefault();
				moveConfirmFocus(e.key === 'ArrowDown' ? 1 : -1);
			} else if (e.key === 'ArrowLeft' || e.key === 'ArrowRight') {
				e.preventDefault();
			}
			return;
		}

		if (e.key === 'ArrowLeft' && hasMultiple) {
			e.preventDefault();
			prev();
			return;
		}
		if (e.key === 'ArrowRight' && hasMultiple) {
			e.preventDefault();
			next();
			return;
		}

		// Zoom: `+` / `-` about the stage centre, `0` resets. INSIDE the gates
		// above by construction — placed after `defaultPrevented`,
		// `isViewerFrontmost` and `isBlockedByModal` have each had their say, so a
		// zoom key can never steal a press another owner or a layer above is due.
		//
		// The modifier rule is a CONTRACT, not a nicety: this listens on `window`,
		// so acting on Ctrl/Cmd/Alt-`-`/`0` would swallow the browser's own
		// page-zoom (and OS) shortcuts from every surface while a viewer is open.
		// Leave those keys entirely — do not act, and do NOT `preventDefault`, or
		// the native shortcut is cancelled even though we declined to handle it.
		// `shiftKey` is fine (on most layouts `+` IS Shift+`=`), so it is absent
		// from the guard.
		if (e.ctrlKey || e.metaKey || e.altKey) return;
		// Zoom keys are DISABLED, not merely no-ops, while no bitmap exists — the
		// mobile `deferred` cell shows a placeholder with nothing to zoom
		// (TASK-2460). Consume the key so it can't leak past the modal, but do not
		// act. (`+`/`-` are already inert via `readGeometry`; `0` would otherwise
		// still reset.)
		const isZoomKey = e.key === '+' || e.key === '=' || e.key === '-' || e.key === '0';
		if (isZoomKey && !bitmapPresent) {
			e.preventDefault();
			return;
		}
		if (e.key === '+' || e.key === '=') {
			// `+` — including the numpad, whose `.key` is also `'+'` — and bare `=`.
			e.preventDefault();
			stepZoom(ZOOM_STEP);
		} else if (e.key === '-') {
			// `-`, including the numpad, whose `.key` is also `'-'`.
			e.preventDefault();
			stepZoom(1 / ZOOM_STEP);
		} else if (e.key === '0') {
			e.preventDefault();
			zoom = resetZoom();
			rebaseDrag(); // keyboard reset mid-drag must not desync the pan baseline
		}
		// NO Escape branch. See the registration above.
	}

	// Close only on a click of the backdrop itself — clicks on the image or
	// controls have a different target, so they don't dismiss. This avoids
	// putting a click handler (and its a11y burden) on the <img>.
	function onBackdropClick(e: MouseEvent) {
		// A pan that released here produced this click — a drag is not a dismissal
		// (TASK-2458). A below-threshold press (still a click) leaves this false, so
		// it still closes; a plain click on the backdrop closes as before.
		if (suppressClick) return;
		const el = rootEl;
		// Same gates as every other pointer entry point. Background inertness makes
		// the normal path unreachable, so this is robustness, not a live fix — but a
		// pointer owner that skips the gates the keyboard owner carries is the drift
		// that breeds the next BUG-2441.
		if (!el || !pointerGatesOpen(el)) return;
		if (e.target === e.currentTarget) onClose();
	}
</script>

<svelte:window onkeydown={onKeydown} />

<!--
	The backdrop click has no keyboard twin ON THIS ELEMENT by design: the
	keyboard route out is Escape (through the shared stack) and the close
	button, which is the first thing focus lands on. Adding a key handler here
	would put a second, undocumented dismissal on the container.
-->
<!-- svelte-ignore a11y_click_events_have_key_events -->
<div
	bind:this={rootEl}
	class="lightbox-backdrop {VIEWER_ROOT_CLASS}"
	class:lightbox-sheet={isSheet}
	role="dialog"
	aria-modal="true"
	aria-label={dialogLabel}
	tabindex="-1"
	onclick={onBackdropClick}
	ondblclick={onDoubleClick}
	onpointerdown={onPointerDown}
	onpointermove={onPointerMove}
	onpointerup={onPointerUp}
	onpointercancel={onPointerCancel}
	onlostpointercapture={onLostPointerCapture}
>
	<!--
		Explicit `aria-label`s: the glyph IS the label otherwise ("✕", "‹", "›"),
		and `title` does not win over element content for the accessible name. The
		browser suite (TASK-2436) also addresses these controls BY NAME, never by
		class or a bare `[role="dialog"]`.
	-->
	<button
		class="lightbox-close"
		type="button"
		title="Close (Esc)"
		aria-label="Close"
		onclick={onClose}
	>
		&#10005;
	</button>

	<!--
		ACTION TOOLBAR (TASK-2474). The shared descriptor list, drawn over the
		stage. The `.lightbox-toolbar` wrapper is the positioned pill AND the
		pointer control-exclusion hook — registered in BOTH lists above
		(`onPointerDown` / `onDoubleClick` / `onWheel`) via `.lightbox-toolbar`, so
		a press / double-click / wheel on it is that control's, never a pan or a
		zoom. The ARIA ROLE lives on the inner state, not the wrapper: the actions
		are a `role="toolbar"`, and the delete drill-down a `role="menu"` (its rows
		are `role="menuitem"`, which a toolbar must not parent). Gated on `img`.
	-->
	{#if img}
		<div class="lightbox-toolbar">
			{#if deleteConfirm.pending}
				<!--
					Delete confirmation as a drill-down (DR-18) — the SAME shared component
					the panel and the strip render, so the wording and shape can only ever
					change in one place. The module owns the pending/warning state; the
					descriptor's awaited gate resolves through confirm() / cancel(). It is a
					`role="menu"` of `role="menuitem"` rows: focus enters the first row on
					open, Up/Down move between them, Escape backs out (see the handlers).
				-->
				<div class="lightbox-delete-confirm" role="menu" aria-label="Confirm delete">
					<AttachmentDeleteConfirm
						prompt={deleteConfirm.warning ?? ''}
						promptId={deletePromptId}
						oncancel={() => deleteConfirm.cancel()}
						onconfirm={() => deleteConfirm.confirm()}
					/>
				</div>
			{:else}
				<div class="lightbox-toolbar-actions" role="toolbar" aria-label="Attachment actions">
					{#if toolbarError}
						<div class="lightbox-toolbar-error" role="alert">{toolbarError}</div>
					{/if}
					{#each toolbarActions as action (action.id)}
						{#snippet toolIcon()}
							<AttachmentIcon id={action.icon} />
						{/snippet}
						{#if action.element === 'anchor'}
							<!-- Open / Download are links. When disabled — the descriptor says so,
							     or `missing` / `unreachablePending` — the href is DROPPED (not just
							     aria-disabled), so the anchor is inert to keyboard activation too,
							     not only to the `pointer-events: none` the aria-disabled style adds. -->
							<a
								class="lightbox-tool"
								href={toolDisabled(action) ? undefined : action.href(toolbarCtx)}
								download={action.download?.(toolbarCtx)}
								target={action.target}
								rel={action.rel}
								aria-label={action.label}
								title={action.description ?? action.label}
								aria-disabled={toolDisabled(action)}
							>
								<span class="lightbox-tool-icon" aria-hidden="true">{@render toolIcon()}</span>
								<span class="lightbox-tool-label">{action.label}</span>
							</a>
						{:else}
							<button
								class="lightbox-tool"
								class:danger={action.danger}
								type="button"
								aria-label={action.label}
								title={action.description ?? action.label}
								disabled={toolDisabled(action) || toolbarBusy}
								onclick={() => runToolbarAction(action)}
							>
								<span class="lightbox-tool-icon" aria-hidden="true">{@render toolIcon()}</span>
								<span class="lightbox-tool-label"
									>{toolbarBusy && action.id === 'delete' ? 'Deleting…' : action.label}</span
								>
							</button>
						{/if}
					{/each}
				</div>
			{/if}
		</div>
	{/if}

	<!--
		The STAGE is the 92vw×92vh box the bare <img> used to be; the image sits
		inside it, `object-fit: contain`, and carries the zoom transform. The stage
		is `pointer-events: none` so a click on the empty letterbox around the image
		falls through to the backdrop and closes (exactly as it did when the image
		was the only thing here); the image re-enables pointer events so a click ON
		it does not close — and so 3c's drag / 3d's pinch have a target. The toolbar
		and meta chrome sit ABOVE the stage (their own `z-index`), never inside it.

		The prev/next nav are the ONE exception: they live INSIDE the stage (3c-ii
		nav-placement fix). On desktop the stage is `position: static`, so their
		`position: absolute` still resolves against the fixed backdrop — byte-identical
		full-viewport centring. In the mobile sheet the stage is `position: relative`
		(it docks the chrome to the bottom and shortens), so `top: 50%` re-anchors to
		the SHORTENED stage box and the arrows clear the dock automatically — no
		magic-number dock height. Because the stage is `pointer-events: none`, the nav
		re-enables `pointer-events: auto` in its own rule (the same pattern the image
		and the tap-to-load / retry buttons use); on desktop that value was already the
		inherited default, so nothing changes there.
	-->
	<div class="lightbox-stage" bind:this={stageEl}>
		{#if hasMultiple}
			<button
				class="lightbox-nav prev"
				type="button"
				title="Previous (←)"
				aria-label="Previous image"
				onclick={prev}
			>
				&#8249;
			</button>
			<button
				class="lightbox-nav next"
				type="button"
				title="Next (→)"
				aria-label="Next image"
				onclick={next}
			>
				&#8250;
			</button>
		{/if}

		{#if shownRenderer === 'raster-image' && loader.displaySrc}
			<!--
				KEYED ON THE LOAD TOKEN (TASK-2459). The <img> would otherwise persist
				across navigation, and a bitmap that finished decoding LATE would fire
				`load` reading the NEW image's src — labelling A's fallback decode as B
				and suppressing B's upgrade (the no-`{#key}` switch-safety class). A
				fresh element per REQUEST tears the stale element's listener down. The
				token changes on load / retry but NOT on the thumb→original upgrade, so
				the upgrade reuses the SAME element (no flash) while a retry — whose URL
				is usually unchanged — still re-mounts and re-requests. The decode is
				read from the EVENT's own target, and the loader fences on the decoded
				src too, belt and braces.
			-->
			{#key loader.loadToken}
				<!-- draggable=false: a native image drag would otherwise pre-empt the pan. -->
				<!--
					`data-gen` is the generation this element was mounted under, read back
					in the handlers via `dataset.gen` — the SAME frozen-DOM-attribute
					snapshot the src fence uses (`getAttribute('src')`). A DOM attribute on
					a DETACHED element is not updated, so it is a true per-element snapshot;
					a plain `{@const}` would compile to a lazy `$derived` and re-read the
					CURRENT `loadToken` when a late event fires, defeating the fence in an
					A→B→A navigation (the third A reuses A's exact URL, so only the
					generation tells the detached first element apart). The `{#key}` gives a
					fresh element per load / retry; the thumb→original upgrade reuses the
					SAME element (token unchanged), so `data-gen` is stable across it.
				-->
				<img
					bind:this={imgEl}
					use:releaseImg
					class="lightbox-image"
					class:panning={dragging}
					src={loader.displaySrc}
					data-gen={loader.loadToken}
					alt={img.alt || 'Attachment'}
					draggable="false"
					onload={(e) => {
						const el = e.currentTarget as HTMLImageElement;
						loader.decoded(el.naturalWidth, el.naturalHeight, el.getAttribute('src') ?? '', Number(el.dataset.gen));
						// RE-CLAMP on a fresh bitmap. A same-id reload can change the geometry
						// (an async dimension fill swaps a large original for a smaller thumb,
						// lowering actualScale and MAX_SCALE), stranding the current scale above
						// the new ceiling with out-of-bounds pan. The reset effect fires only on
						// image CHANGE (id) and the ResizeObserver only on stage resize, so
						// neither catches this. `clampState`, NOT reset — the same image's valid
						// zoom survives; only an over-ceiling scale/pan is pulled back. Geometry
						// is measurable only post-decode. Gated to the LIVE element (`el ===
						// imgEl`, the same fence `decoded` applies) so a stale detached load can't
						// drive the current zoom. Event handler, so the `zoom` read/write is not a
						// CONVE-1688 self-write.
						if (el === imgEl) {
							const g = readGeometry();
							if (g) {
								zoom = clampState(zoom, g);
								rebaseDrag(); // a re-clamp mid-drag must not desync the pan baseline
							}
						}
					}}
					onerror={(e) => {
						const el = e.currentTarget as HTMLImageElement;
						loader.errored(el.getAttribute('src') ?? '', Number(el.dataset.gen));
					}}
					style="transform: translate({zoom.x}px, {zoom.y}px) scale({zoom.scale});"
				/>
			{/key}
		{/if}

		{#if shownRenderer === 'raster-image' && loader.phase === 'loading'}
			<!-- pointer-events:none so it never intercepts a pan / dblclick over the
			     stage; the spinner is decoration over the (loading) image. -->
			<div class="lightbox-status lightbox-loading" role="status" aria-label="Loading image">
				<span class="lightbox-spinner" aria-hidden="true"></span>
			</div>
		{/if}

		{#if shownRenderer === 'raster-image' && loader.phase === 'deferred'}
			<!-- The mobile large/unknown cell: DR-5b auto-fetches nothing here, so this
			     is a TAP-TO-LOAD affordance, not a spinner (TASK-2460). The layer is
			     inert; the button itself re-enables pointer events (the stage turns
			     them off) so a tap can reach it — the one failure mode this cell exists
			     to avoid. The whole placeholder is the button (a large touch target),
			     sized to the image's aspect ratio when known. On tap it is replaced by
			     the image it loads, so it hands focus off first (TASK-2456). -->
			<div class="lightbox-status lightbox-deferred">
				<button
					class="lightbox-tap-load"
					type="button"
					style={placeholderStyle}
					onclick={() => {
						handoffFocus(rootEl!, rootEl?.querySelector('.lightbox-tap-load') ?? null);
						loader.loadOriginal();
					}}
				>
					<span class="lightbox-tap-label">Tap to load full image</span>
				</button>
			</div>
		{/if}

		{#if shownRenderer === 'raster-image' && loader.phase === 'error'}
			<div class="lightbox-status lightbox-error" role="alert">
				<p class="lightbox-error-text">This image couldn't be loaded.</p>
				<!-- pointer-events:auto EXPLICITLY: the stage turns pointer events off,
				     and this control must be clickable. On a successful retry it
				     disappears, so it hands focus off first (TASK-2456). -->
				<button
					class="lightbox-retry"
					type="button"
					onclick={() => {
						handoffFocus(rootEl!, rootEl?.querySelector('.lightbox-retry') ?? null);
						loader.retry();
					}}
				>
					Retry
				</button>
			</div>
		{/if}

		<!--
			THE FALLBACK ARM (TASK-2476). A navigable entry the viewer cannot draw as an
			image — an unsafe/active type that flipped or was added while open. NO
			BYTES: no `<img>`, no `src`, no request (the load effect disposed the
			loader on the arm flip). Just the file's identity — the large family icon,
			its name, type · size — and an honest "No preview available". Same chrome,
			same modal contract, same lease as the raster arm; zoom is disabled
			(`bitmapPresent` is false here). `pointer-events: none` like the other
			stage overlays, so a click on the empty area still reaches the backdrop.
		-->
		{#if img && shownRenderer !== 'raster-image' && !soleMissing}
			<div class="lightbox-fallback" role="group" aria-label="No preview available">
				<span class="lightbox-fallback-icon" aria-hidden="true">
					<AttachmentIcon id={fallbackIconId} size={72} />
				</span>
				<p class="lightbox-fallback-name" title={displayName}>{displayName}</p>
				{#if headerDetail}
					<p class="lightbox-fallback-detail">{headerDetail}</p>
				{/if}
				<p class="lightbox-fallback-note">No preview available</p>
			</div>
		{/if}

		<!--
			THE MISSING ARM (3c-ii T2b, TASK-2488). A SINGLE-item surface whose file is
			gone (metadata 404) sits in this inert "no longer available" state rather
			than flash-closing — the message the retired options panel showed. NO BYTES
			(the load effect disposed the loader on the `soleMissing` flip); the toolbar
			is already inert (every action is disabled while `missing`). `role="status"`
			so it is announced. A multi-image set never reaches here — it advances or
			closes through the tombstone path.
		-->
		{#if soleMissing}
			<div class="lightbox-fallback lightbox-missing" role="status">
				<span class="lightbox-fallback-icon" aria-hidden="true">
					<AttachmentIcon id={fallbackIconId} size={72} />
				</span>
				<p class="lightbox-fallback-name" title={displayName}>{displayName}</p>
				<p class="lightbox-fallback-note">This file is no longer available. It may have been deleted.</p>
			</div>
		{/if}
	</div>

	{#if hasMultiple}
		<!-- Derived `shownIndex` over the SURVIVORS, so the counter names the image
		     actually on screen even after a deletion shrank the set. -->
		<div class="lightbox-counter">{shownIndex + 1} / {survivors.length}</div>
	{/if}

	<!--
		METADATA HEADER (TASK-2475). Filename / type / size for the shown image.
		Bottom-left, NOT the top row: the two primary actions (the toolbar) and
		Close own the top, and metadata must never crowd them on a phone (DR-18) —
		placing it here keeps that true by construction. A LABEL like the counter,
		not a dismiss target: it is excluded from the pan/zoom gestures (via
		`.lightbox-meta` in the three lists) so a press on it is inert, and only the
		Retry button acts. The name is truncated with the FULL value in `title`
		(DR-13); the detail line adds type · size only when known.
	-->
	{#if img}
		<div class="lightbox-meta">
			<div class="lightbox-meta-name" title={displayName}>{displayName}</div>
			{#if headerDetail}
				<div class="lightbox-meta-detail">{headerDetail}</div>
			{/if}
			{#if headerTransient}
				<!-- DR-10: retryable, BESIDE the name/type it already knows — never a
				     blank sheet. Retry revalidates through the module. -->
				<div class="lightbox-meta-error" role="status">
					<span>Couldn't load details</span>
					<span aria-hidden="true">·</span>
					<button class="lightbox-meta-retry" type="button" onclick={() => headerMeta.retry()}
						>Retry</button
					>
				</div>
			{/if}
		</div>
	{/if}
</div>

<style>
	.lightbox-backdrop {
		position: fixed;
		inset: 0;
		/*
		 * ABOVE EVERY OTHER OVERLAY IN THE APP (TASK-2429). The viewer is a modal:
		 * it inerts every other body child, so anything that PAINTS over it is a
		 * surface the user can see but cannot touch — the worst of both. The app
		 * shell wrapper is `display: contents` (`app.html`), which creates no
		 * stacking context, so every fixed overlay in the tree competes with this
		 * one in the ROOT stacking context — being a body child is not by itself
		 * protection.
		 *
		 * What this value must out-rank, highest first (swept for TASK-2429 — CSS
		 * declarations AND `style.cssText` written from TypeScript, which a CSS-only
		 * grep misses):
		 *   99999  EmojiPickerButton's desktop dropdown (`EmojiPickerButton.svelte`,
		 *          body-portaled via `portalAction`) — the highest in the tree by
		 *          two orders of magnitude, and the reason 1000 was not enough
		 *    1000  the editor's block-drag ghost, appended to `<body>` and styled
		 *          inline (`editor/block-drag-handle.ts::createGhost`)
		 *     200  Menu (body-portaled) and the editor's slash/link popovers
		 *     199  the editor's block context menu
		 *     100  toasts, the notification panel, the editor's mobile toolbar
		 *   45-60  DockedSheet / BottomSheet / Sidebar / CommandPalette / TopBar
		 * (`ItemDetail`'s 1000 is a `@media print` footer, not a runtime overlay.)
		 *
		 * A NEW OVERLAY ABOVE THIS VALUE IS A BUG unless it is meant to cover a
		 * modal viewer. One thing legitimately renders above it and needs no
		 * z-index to do so: a native `showModal()` dialog, which the platform puts
		 * in the TOP LAYER — the manager deliberately leaves those interactive
		 * (`viewerBackdrop.ts::keepInteractiveAsDialog`) and the key handler stands
		 * down for them (`isBlockedByModal`).
		 *
		 * jsdom cannot see any of this; the paint-order proof is TASK-2436's.
		 */
		z-index: 100000;
		display: flex;
		align-items: center;
		justify-content: center;
		background: rgba(0, 0, 0, 0.82);
		backdrop-filter: blur(2px);
		cursor: zoom-out;
		/* A drag-to-pan surface: no text should get selected mid-drag (the counter
		   is the only text, and it is not meant to be selectable). draggable=false
		   on the <img> handles the image; this handles selection (TASK-2458). */
		user-select: none;
		-webkit-user-select: none;
	}

	.lightbox-stage {
		/* The box the bare <img> used to occupy — TASK-2454's coordinate system. */
		width: 92vw;
		height: 92vh;
		flex: none;
		display: flex;
		align-items: center;
		justify-content: center;
		/*
		 * The empty letterbox around the image is transparent to pointer events, so
		 * a click there reaches the backdrop and closes — the pre-zoom behaviour.
		 * The image below re-enables them. Deliberately NOT `touch-action: none`
		 * here: that would kill native pinch before 3d's handler exists, leaving
		 * phone users no zoom at all in the interval (out of TASK-2455's scope).
		 */
		pointer-events: none;
	}

	.lightbox-image {
		max-width: 100%;
		max-height: 100%;
		object-fit: contain;
		border-radius: var(--radius);
		box-shadow: 0 8px 40px rgba(0, 0, 0, 0.5);
		cursor: default;
		pointer-events: auto;
		transform-origin: center;
		transition: transform 0.15s ease-out;
	}

	/* While DRAGGING, the image must track the pointer 1:1 — the easing transition
	   would make it lag behind the cursor (TASK-2458). Discrete zoom (keys / wheel /
	   double-click) keeps the transition. */
	.lightbox-image.panning {
		transition: none;
	}

	/* Reduced motion suppresses the TRANSITION only — the zoom still works
	   (Modal.svelte's precedent). */
	@media (prefers-reduced-motion: reduce) {
		.lightbox-image {
			transition: none;
		}
	}

	/* Loading spinner + error, centred over the stage (TASK-2459). */
	.lightbox-status {
		position: absolute;
		inset: 0;
		display: flex;
		flex-direction: column;
		align-items: center;
		justify-content: center;
		gap: var(--space-3);
		z-index: 1;
	}

	.lightbox-loading {
		/* Never intercept a pan / double-click over the stage. */
		pointer-events: none;
	}

	/* The no-bytes fallback arm (TASK-2476). Centred over the stage like the status
	   overlays; inert, so a click on the surrounding area still reaches the backdrop
	   and closes. Logical properties + min-width:0 so a long filename ellipsizes
	   (DR-13) rather than blowing the box out. */
	.lightbox-fallback {
		position: absolute;
		inset: 0;
		display: flex;
		flex-direction: column;
		align-items: center;
		justify-content: center;
		gap: var(--space-2);
		padding: var(--space-4);
		text-align: center;
		color: #fff;
		pointer-events: none;
	}

	.lightbox-fallback-icon {
		display: inline-flex;
		color: rgba(255, 255, 255, 0.85);
	}

	.lightbox-fallback-name {
		margin: 0;
		max-inline-size: min(80vw, 560px);
		min-width: 0;
		font-size: 1rem;
		font-weight: 600;
		white-space: nowrap;
		overflow: hidden;
		text-overflow: ellipsis;
	}

	.lightbox-fallback-detail {
		margin: 0;
		font-size: 0.85rem;
		opacity: 0.85;
	}

	.lightbox-fallback-note {
		margin: 0;
		margin-block-start: var(--space-1);
		font-size: 0.85rem;
		opacity: 0.7;
	}

	.lightbox-spinner {
		width: 42px;
		height: 42px;
		border: 3px solid rgba(255, 255, 255, 0.25);
		border-top-color: rgba(255, 255, 255, 0.9);
		border-radius: 50%;
		animation: lightbox-spin 0.8s linear infinite;
	}

	@media (prefers-reduced-motion: reduce) {
		/* Slowed, not stopped — it must still read as "in progress". */
		.lightbox-spinner {
			animation-duration: 2.4s;
		}
	}

	@keyframes lightbox-spin {
		to {
			transform: rotate(360deg);
		}
	}

	.lightbox-error {
		/* The layer is inert; the button re-enables itself below. */
		pointer-events: none;
		color: #fff;
		text-align: center;
	}

	.lightbox-error-text {
		margin: 0;
		font-size: 0.95rem;
	}

	.lightbox-retry {
		/* EXPLICIT: the stage sets `pointer-events: none`, so the one interactive
		   control in this layer has to turn them back on (TASK-2459). */
		pointer-events: auto;
		padding: var(--space-2) var(--space-4);
		background: rgba(0, 0, 0, 0.5);
		border: 1px solid rgba(255, 255, 255, 0.3);
		border-radius: var(--radius);
		color: #fff;
		font-size: 0.9rem;
		cursor: pointer;
	}

	.lightbox-retry:hover {
		background: rgba(0, 0, 0, 0.75);
	}

	/* Tap-to-load placeholder for the mobile deferred cell (TASK-2460). The layer
	   is inert; the button re-enables pointer events below. */
	.lightbox-deferred {
		pointer-events: none;
	}

	.lightbox-tap-load {
		/* EXPLICIT: the stage sets `pointer-events: none`, so the tap target has to
		   turn them back on — a control the user cannot tap is the failure this cell
		   exists to avoid (TASK-2460). */
		pointer-events: auto;
		display: flex;
		align-items: center;
		justify-content: center;
		/* Sizing (aspect ratio when known, neutral box otherwise) comes from the
		   inline `placeholderStyle`. */
		background: rgba(255, 255, 255, 0.06);
		border: 1px dashed rgba(255, 255, 255, 0.35);
		border-radius: var(--radius);
		color: #fff;
		cursor: pointer;
	}

	.lightbox-tap-load:hover,
	.lightbox-tap-load:focus-visible {
		background: rgba(255, 255, 255, 0.12);
		border-color: rgba(255, 255, 255, 0.6);
	}

	.lightbox-tap-label {
		padding: var(--space-2) var(--space-4);
		font-size: 0.95rem;
		font-weight: 500;
	}

	.lightbox-close {
		position: absolute;
		/* Above the stage's stacking context. Small on purpose: the viewer's own
		   z-index sweep (TASK-2436) forbids any app overlay at or above 100000. */
		z-index: 1;
		top: var(--space-3);
		right: var(--space-3);
		width: 40px;
		height: 40px;
		display: flex;
		align-items: center;
		justify-content: center;
		background: rgba(0, 0, 0, 0.4);
		border: 1px solid rgba(255, 255, 255, 0.2);
		border-radius: 50%;
		color: #fff;
		font-size: 1.1rem;
		cursor: pointer;
		line-height: 1;
	}

	.lightbox-close:hover {
		background: rgba(0, 0, 0, 0.7);
	}

	.lightbox-nav {
		position: absolute;
		z-index: 1;
		top: 50%;
		transform: translateY(-50%);
		width: 48px;
		height: 48px;
		display: flex;
		align-items: center;
		justify-content: center;
		background: rgba(0, 0, 0, 0.4);
		border: 1px solid rgba(255, 255, 255, 0.2);
		border-radius: 50%;
		color: #fff;
		font-size: 1.8rem;
		cursor: pointer;
		line-height: 1;
		/* The nav lives inside the `pointer-events: none` stage (see the template
		   note), so it must re-enable pointer events to stay clickable. On desktop
		   this is the already-inherited default (the backdrop is interactive), so the
		   computed value is unchanged and the desktop layout stays byte-identical. */
		pointer-events: auto;
	}

	.lightbox-nav:hover {
		background: rgba(0, 0, 0, 0.7);
	}

	.lightbox-nav.prev {
		left: var(--space-3);
	}

	.lightbox-nav.next {
		right: var(--space-3);
	}

	.lightbox-counter {
		position: absolute;
		z-index: 1;
		bottom: var(--space-3);
		left: 50%;
		transform: translateX(-50%);
		padding: var(--space-1) var(--space-3);
		background: rgba(0, 0, 0, 0.5);
		border-radius: 9999px;
		color: #fff;
		font-size: 0.8rem;
	}

	/* Metadata header (TASK-2475). Bottom-left, clear of the top toolbar / close
	   (DR-18). Logical properties + `min-width: 0` so a long filename ellipsizes
	   inside its own box rather than blowing out the layout (DR-13). A LABEL, like
	   the counter: it keeps its default pointer events so a press on it is the
	   header's own (and is excluded from the pan/zoom gestures via `.lightbox-meta`
	   in the three exclusion lists) rather than falling THROUGH to the image behind
	   and arming a pan. It does not close the viewer — a filename is not a dismiss
	   target — and only Retry inside it does anything. Width is capped short of the
	   centred counter (reserve ~72px each side of centre) so the two never touch,
	   at any width or in RTL. */
	.lightbox-meta {
		position: absolute;
		z-index: 1;
		inset-block-end: var(--space-3);
		inset-inline-start: var(--space-3);
		min-width: 0;
		max-inline-size: min(calc(50% - 72px), 420px);
		display: flex;
		flex-direction: column;
		gap: 2px;
		color: #fff;
		/* Legible over any image — a soft shadow rather than a plate, so it stays
		   unobtrusive while the filename is still readable on a light photo. */
		text-shadow: 0 1px 3px rgba(0, 0, 0, 0.9);
	}

	.lightbox-meta-name {
		min-width: 0;
		max-inline-size: 100%;
		font-size: 0.85rem;
		font-weight: 600;
		white-space: nowrap;
		overflow: hidden;
		text-overflow: ellipsis;
	}

	.lightbox-meta-detail {
		min-width: 0;
		font-size: 0.75rem;
		opacity: 0.85;
		white-space: nowrap;
		overflow: hidden;
		text-overflow: ellipsis;
	}

	.lightbox-meta-error {
		display: flex;
		align-items: center;
		gap: var(--space-1);
		font-size: 0.75rem;
		color: var(--accent-red, #ff6b6b);
	}

	.lightbox-meta-retry {
		padding: 1px var(--space-1);
		border: 1px solid currentColor;
		border-radius: var(--radius-sm, 4px);
		background: transparent;
		color: inherit;
		font: inherit;
		font-size: 0.75rem;
		cursor: pointer;
	}

	.lightbox-meta-retry:hover,
	.lightbox-meta-retry:focus-visible {
		background: rgba(255, 255, 255, 0.16);
	}

	/* Action toolbar (TASK-2474). The WRAPPER is positioning + the pointer-
	   exclusion hook only; each inner state carries its own look. Top-centre,
	   above the stage's stacking context (`z-index: 1`, like the other chrome). */
	.lightbox-toolbar {
		position: absolute;
		z-index: 1;
		top: var(--space-3);
		left: 50%;
		transform: translateX(-50%);
		max-width: min(92vw, 640px);
	}

	/* The confirm panel is TALLER and WIDER than the action pill and would collide
	   with the top-right close button at narrow widths — drop it below the close
	   button's row (40px control at `top: var(--space-3)`). */
	.lightbox-toolbar:has(.lightbox-delete-confirm) {
		top: calc(var(--space-3) + 52px);
	}

	/* The action pill: a horizontal row of icon/label controls. */
	.lightbox-toolbar-actions {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		flex-wrap: wrap;
		justify-content: center;
		padding: var(--space-1) var(--space-2);
		background: rgba(0, 0, 0, 0.5);
		border: 1px solid rgba(255, 255, 255, 0.2);
		border-radius: 9999px;
	}

	/* The delete drill-down: a small panel of stacked rows (the shared
	   AttachmentDeleteConfirm), the same column shape the panel's menu gives it. */
	.lightbox-delete-confirm {
		display: flex;
		flex-direction: column;
		align-items: stretch;
		border-radius: var(--radius);
		padding: var(--space-2);
		background: var(--bg-primary, #1a1a1a);
		color: var(--text-primary, #fff);
		border: 1px solid rgba(255, 255, 255, 0.2);
		min-width: min(88vw, 320px);
	}

	.lightbox-tool {
		display: inline-flex;
		align-items: center;
		gap: var(--space-1);
		padding: var(--space-1) var(--space-2);
		border: none;
		border-radius: 9999px;
		background: transparent;
		color: #fff;
		font: inherit;
		font-size: 0.85rem;
		line-height: 1;
		cursor: pointer;
		text-decoration: none;
	}

	.lightbox-tool:hover,
	.lightbox-tool:focus-visible {
		background: rgba(255, 255, 255, 0.16);
	}

	.lightbox-tool:disabled,
	.lightbox-tool[aria-disabled='true'] {
		opacity: 0.45;
		cursor: default;
		pointer-events: none;
	}

	.lightbox-tool.danger {
		color: var(--accent-red, #ff6b6b);
	}

	.lightbox-tool-icon {
		display: inline-flex;
		width: 18px;
		height: 18px;
	}

	/* DR-18: the label is ALWAYS in the accessible name (`aria-label`); its VISIBLE
	   text shows on phone (≤768, the app's canonical breakpoint) and is hidden on
	   the roomier desktop toolbar, which stays icon-only. */
	.lightbox-tool-label {
		display: none;
	}

	@media (max-width: 768px) {
		.lightbox-tool-label {
			display: inline;
		}
	}

	.lightbox-toolbar-error {
		flex-basis: 100%;
		text-align: center;
		color: var(--accent-red, #ff6b6b);
		font-size: 0.8rem;
	}

	/* ── Mobile sheet layout (PLAN-2392 3c-ii / T5, AM-3) ────────────────────────
	   The phone presentation of the converged surface. Selected by the
	   `.lightbox-sheet` class the root toggles off `viewport.isMobile` (see the
	   script) — NOT a bare `@media`, so JS and CSS share the one app breakpoint and
	   the flip is a DOM fact the modal-contract jsdom suite can drive and read. This
	   is a PURE RE-LAYOUT of the EXISTING chrome: the toolbar and meta leave their
	   desktop absolute anchors and DOCK, stacked, to the bottom edge as a sheet; the
	   stage yields them the room and fills the rest. No DOM node is added, moved, or
	   re-keyed, so a breakpoint flip mid-open re-lays-out the SAME instance — the
	   modal contract, the portal, the loader, the zoom transform and every
	   gesture/Escape registration are untouched. Every rule here is scoped under
	   `.lightbox-sheet`, so the DESKTOP layout is byte-identical. */
	.lightbox-backdrop.lightbox-sheet {
		/* Bottom-anchored: a content column with the chrome docked to the bottom. */
		flex-direction: column;
		align-items: stretch;
		justify-content: flex-end;
	}

	/* The stage fills the space ABOVE the dock and yields its width to the phone.
	   `min-height: 0` is load-bearing: a flex item's default `min-height: auto`
	   refuses to shrink below its content, which would push the docked chrome off
	   the bottom of the screen. The zoom coordinate system re-reads this smaller box
	   live (`readGeometry`), so the pan/zoom bounds track the new geometry — a
	   browser-only proof (T7).
	   `position: relative` makes the stage the containing block for its OWN
	   `inset: 0` overlays (the loading spinner, the mobile tap-to-load affordance,
	   the no-preview fallback, the missing arm). Without it those absolutes resolve
	   against the fixed backdrop — the FULL viewport — and would centre over the
	   bottom dock instead of the shortened stage. On desktop the stage stays static
	   and the overlays centre against the full-viewport backdrop, which coincides
	   with the 92vh centred stage; the shortened sheet stage breaks that coincidence,
	   so it establishes the containing block explicitly here (sheet-scoped, so
	   desktop is unchanged). No fixed descendant exists, so this traps nothing. */
	.lightbox-sheet .lightbox-stage {
		position: relative;
		width: 100vw;
		height: auto;
		flex: 1 1 auto;
		min-height: 0;
		order: 0;
	}

	/* META + TOOLBAR become flow items in the column so they dock, stacked, at the
	   bottom with NO magic-number coordination between them — `order` alone puts
	   content on top (0), then meta (1), then the toolbar as the very bottom bar (2),
	   primary actions in thumb reach. They are the SAME elements with the SAME
	   classes, so `.lightbox-toolbar` / `.lightbox-meta` still match all three
	   pointer-exclusion `.closest()` lists (pointerdown / dblclick / wheel) — a
	   press, double-click or wheel on the docked chrome stays excluded from the
	   pan/zoom exactly as on desktop. Dropping to `position: static` also makes the
	   desktop `:has(.lightbox-delete-confirm)` top-offset inert (a `top` on a static
	   box is ignored); the confirm drill-down instead grows the toolbar upward and
	   the stage yields, so it never collides with the top-right close. */
	.lightbox-sheet .lightbox-meta,
	.lightbox-sheet .lightbox-toolbar {
		position: static;
		inset: auto;
		transform: none;
		width: 100%;
		max-width: none;
		max-inline-size: none;
		flex: none;
		/* One sheet surface: a translucent plate, padded for touch, with a hairline
		   top edge separating the dock from the content above. */
		padding-inline: var(--space-3);
		padding-block: var(--space-2);
		background: rgba(0, 0, 0, 0.6);
		border-top: 1px solid rgba(255, 255, 255, 0.12);
	}
	.lightbox-sheet .lightbox-meta {
		order: 1;
	}
	.lightbox-sheet .lightbox-toolbar {
		order: 2;
	}

	/* The counter can't stay bottom-centre — the toolbar dock is there now. Dock it
	   to the top-LEFT (clearing the centring transform), unambiguously clear of the
	   top-right close at any width or count length — a top-CENTRE counter could graze
	   the close on a narrow phone. The top-left corner is free in the sheet (the meta
	   moved into the bottom dock). */
	.lightbox-sheet .lightbox-counter {
		top: var(--space-3);
		bottom: auto;
		left: var(--space-3);
		transform: none;
	}

	/* Forced-colors on the SHEET chrome (T5, DR-4). Forced-colors drops the docked
	   plate's translucent fill and hairline border, so the sheet would merge into the
	   content with no visible boundary — give the docked meta + toolbar an opaque
	   Canvas fill and a real system-colour top edge. Scoped under `.lightbox-sheet`,
	   so the desktop forced-colors rules below are unchanged. The inner tool buttons
	   already get their `ButtonText` borders from the desktop block below (it is not
	   sheet-scoped, so it applies here too). */
	@media (forced-colors: active) {
		.lightbox-sheet .lightbox-meta,
		.lightbox-sheet .lightbox-toolbar {
			background: Canvas;
			border-top: 1px solid CanvasText;
		}
	}

	/* Forced-colors (PLAN-2392 DR-4). The custom palette is discarded, so the
	   image's box-shadow boundary vanishes — give the image a real BORDER so its
	   boundary stays visible. LAST in the sheet so it wins over the controls' own
	   (equal-specificity) border rules above, giving them explicit system-colour
	   borders rather than relying on the UA's forced-colors adjustment. */
	@media (forced-colors: active) {
		.lightbox-image {
			border: 2px solid CanvasText;
		}
		.lightbox-close,
		.lightbox-nav,
		.lightbox-retry,
		.lightbox-tap-load,
		.lightbox-tool,
		.lightbox-meta-retry {
			border: 1px solid ButtonText;
		}
	}
</style>
