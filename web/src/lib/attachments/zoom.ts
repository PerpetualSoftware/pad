/**
 * Zoom / pan math for the attachment viewer — DOM-free, framework-free, and
 * the single owner of every number the viewer's transform is built from
 * (PLAN-2392 phase 3b / TASK-2454).
 *
 * WHY IT IS A SEPARATE MODULE. jsdom reports all-zero rects, so nothing that
 * reads layout can be unit-tested there. Keeping the arithmetic here means the
 * hard properties — anchor invariance, the pan bounds, the scale floor and
 * ceiling — are provable without a browser, and the component (TASK-2455) is
 * left with measuring, wiring and clamping on resize.
 *
 * TWO WORDS, ONE MEANING EACH.
 *
 *  - The **fitted box** is the rendered size `object-fit: contain` gives the
 *    image inside the stage. The browser computes it; this module never
 *    re-derives it from natural-image pixels.
 *  - **fit** is `scale === 1`, i.e. {@link FIT}. There is no third quantity
 *    called a "fit scale" — conflating the two is what put the CSS and the
 *    module in different coordinate systems in an early revision of the design.
 *
 * THE COORDINATE SYSTEM. The stage is a box sized as the old bare `<img>` was
 * (92vw x 92vh, centred), with the `<img>` inside it at `max-width: 100%;
 * max-height: 100%; object-fit: contain`. The transform is
 *
 *     translate(<x>px, <y>px) scale(<scale>)      transform-origin: center
 *
 * so a point sitting `d` CSS px from the image's centre (measured in UNSCALED
 * fitted-box px) paints at
 *
 *     stageCentre + (x, y) + scale * d
 *
 * At `scale === 1, x === 0, y === 0` that is byte-identical to today's
 * rendering — the parity property the phase is built around.
 *
 * All exported functions are pure: none mutates its arguments, and every one
 * that returns a {@link ZoomState} returns a fresh object.
 *
 * TWO PROPERTIES CONSUMERS MUST NOT ASSUME, both consequences of clamping every
 * result rather than tracking an unclamped shadow state:
 *
 *  - **A gesture is not associative.** Ten small {@link zoomTo} steps do not
 *    equal one big one once a pan bound is reached — the intermediate results
 *    were clamped and the excess is gone. So a DRAG must be computed from the
 *    state captured at pointer-down plus the total pointer delta, never by
 *    feeding each `pointermove` delta into the previous clamped result:
 *    overshooting a bound and coming back would otherwise move the image
 *    immediately instead of taking up the slack (TASK-2458).
 *  - **Clamping is lossy.** Pan discarded because the stage shrank does not come
 *    back when it grows again. A consumer that wants that must keep its own
 *    unclamped intent; the viewer deliberately does not (TASK-2455).
 *
 * NOTHING HERE ROUNDS. Geometry arrives fractional on any non-integer
 * `devicePixelRatio` (the browser lays elements out on device-pixel boundaries,
 * which are not CSS-pixel boundaries at dpr 1.25 / 1.5 / 2.75), and rounding
 * intermediate values there drifts the anchored point visibly across a sequence
 * of zoom steps.
 */

/**
 * The measured geometry of one painted image, in CSS px.
 *
 * Every field is expected finite and positive — a caller with no decoded bitmap
 * has nothing to zoom and does not call in at all (TASK-2460 keeps the
 * transform inert until one exists). The functions below are nevertheless
 * defensive about zeros and non-finite values, because geometry is read from
 * live layout and a mid-teardown measurement can produce either; a degenerate
 * geometry must still yield a finite, in-bounds transform — never `NaN` or
 * `Infinity` in a CSS string.
 *
 * MEASURING `fittedW/H` (TASK-2455). They are the image's UNSCALED box, so they
 * cannot come from `getBoundingClientRect()` on the transformed `<img>` — that
 * returns the box AFTER `scale()`, and feeding it back in would make the bounds
 * grow with the zoom. Read the untransformed layout box (`offsetWidth` /
 * `offsetHeight`, which transforms do not affect), or measure once at fit.
 */
export interface Geometry {
	/** Stage box width. */
	stageW: number;
	/** Stage box height. */
	stageH: number;
	/** Rendered image width at `scale === 1`. Never exceeds `stageW`. */
	fittedW: number;
	/** Rendered image height at `scale === 1`. Never exceeds `stageH`. */
	fittedH: number;
	/**
	 * The CURRENTLY PAINTED bitmap's intrinsic width — not the original's.
	 * While only a `thumb-md` variant has decoded, "actual size" means 1:1 with
	 * the thumb, and {@link maxScale} grows when the original swaps in
	 * (TASK-2459). NOTE: this is not the mobile fetch trigger; that trigger is
	 * `scale > 1`, i.e. past fit (TASK-2460).
	 */
	naturalW: number;
	/** The currently painted bitmap's intrinsic height. See {@link Geometry.naturalW}. */
	naturalH: number;
}

/** A point in stage-local CSS px, measured from the stage's top-left corner. */
export interface Point {
	x: number;
	y: number;
}

/**
 * The viewer's transform state.
 *
 * `scale` is relative to fit, which is what makes {@link FIT} a constant rather
 * than a derived quantity. `x` / `y` are the image centre's offset from the
 * stage centre in CSS px.
 */
export interface ZoomState {
	scale: number;
	x: number;
	y: number;
}

/**
 * `scale` at fit — the floor {@link clampScale} and {@link clampState} enforce.
 *
 * Not enforced by {@link clampPan}, which passes any finite scale through so it
 * stays orthogonal to the scale bounds; use {@link clampState} when both matter.
 */
export const FIT = 1;

/** {@link maxScale} is this multiple of 1:1 (or of fit, whichever is larger). */
export const MAX_SCALE_FACTOR = 4;

/**
 * Where {@link toggleFitOrActual} goes when the image is already at 1:1 at fit
 * (an image smaller than the stage: `object-fit: contain` never upscales, so
 * fit IS actual size). Without this, double-click would be a no-op on exactly
 * the images where a user is most likely to try it.
 */
export const TOGGLE_SMALL_SCALE = 2;

/**
 * How close to {@link FIT} still counts as "at fit" for {@link isAtFit} and
 * therefore for {@link toggleFitOrActual}'s direction.
 *
 * A tolerance rather than an equality test because a wheel or pinch gesture
 * lands on 1.0000000002 routinely, and a double-click there must zoom IN, not
 * perform a visually-invisible reset.
 */
export const FIT_EPSILON = 0.001;

/**
 * The multiplicative step for one discrete zoom command — the `+` / `-` keys
 * (TASK-2455). Exported so no call site re-types the literal.
 */
export const ZOOM_STEP = 1.25;

/**
 * Coerce a possibly-NaN/Infinite measurement to something usable.
 *
 * Infinity takes the fallback rather than saturating at a bound: an infinite
 * scale or offset is always a caller-side arithmetic bug (a pinch handler
 * dividing by a zero pointer distance is the likely one), and quietly snapping
 * it to maximum zoom hides the bug behind plausible-looking behaviour, where
 * falling back to the neutral value shows it.
 */
function finiteOr(value: number, fallback: number): number {
	return Number.isFinite(value) ? value : fallback;
}

/**
 * Clamp to `[-bound, bound]`, normalising negative zero away.
 *
 * `Math.max(-0, -5)` is `-0`, so a centred axis would otherwise emit
 * `translate(-0px)` and, more importantly, make `Object.is(x, 0)` false for a
 * value this module documents as exactly zero.
 *
 * It deliberately does NOT re-guard a negative bound: {@link panBound} is the
 * single owner of the centring rule, and a second copy of it here would make
 * that one untestable — breaking the real rule would look correct because this
 * function silently repaired it.
 */
function clampSymmetric(value: number, bound: number): number {
	const clamped = Math.min(bound, Math.max(-bound, value));
	return clamped === 0 ? 0 : clamped;
}

/** True only for a measurement that can be divided by. */
function usable(value: number): boolean {
	return Number.isFinite(value) && value > 0;
}

/**
 * The largest extent any axis is allowed to claim, in CSS px.
 *
 * Ten million px is four orders of magnitude past any layout a browser will
 * produce (the widest real stage is a few thousand), and its purpose is purely
 * arithmetic: it makes `extent * scale` unable to overflow to `Infinity`.
 *
 * That overflow is not cosmetic. {@link panBound} subtracts the stage from the
 * scaled extent, and `Infinity - stage` is `Infinity`, so the pan bound becomes
 * infinite and the clamp silently stops clamping — the one failure mode that
 * lets blank stage show past an image edge. Saturating the inputs instead keeps
 * every bound finite and ordered.
 */
const MAX_EXTENT = 1e7;

/** Saturate a measurement into `[0, MAX_EXTENT]` so products cannot overflow. */
function saneExtent(value: number): number {
	const v = finiteOr(value, 0);
	if (v <= 0) return 0;
	return v > MAX_EXTENT ? MAX_EXTENT : v;
}

/** The stage's centre, in stage-local px. The anchor for centre-origin zooms. */
export function stageCenter(g: Geometry): Point {
	return {
		x: finiteOr(g.stageW, 0) / 2,
		y: finiteOr(g.stageH, 0) / 2,
	};
}

/**
 * The `scale` at which the painted bitmap is displayed 1:1 with its own pixels.
 *
 * Measured on the WIDTH axis, as the contract specifies. `object-fit: contain`
 * preserves the aspect ratio, so for any geometry the browser actually produces
 * the height ratio is the same number; a hand-built non-uniform `Geometry` is
 * the only way to make them disagree, and the width pair wins there.
 *
 * `object-fit: contain` never upscales, so an image smaller than the stage has
 * `fittedW === naturalW` and this returns exactly 1 — fit already IS actual
 * size for it.
 */
export function actualScale(g: Geometry): number {
	if (!usable(g.fittedW) || !usable(g.naturalW)) return FIT;
	const ratio = g.naturalW / g.fittedW;
	// A subnormal `fittedW` divides to `Infinity` even though both inputs passed
	// `usable`. Returning it would not merely be untidy: `toggleFitOrActual`
	// hands this value straight to `zoomTo`, whose `clampScale` treats a
	// non-finite target as invalid and falls back to fit — turning the toggle
	// into the silent no-op the small-image rule exists to prevent. Falling back
	// to FIT here instead routes that geometry to TOGGLE_SMALL_SCALE, so the
	// gesture still does something visible.
	return Number.isFinite(ratio) ? ratio : FIT;
}

/**
 * The upper bound on `scale`, i.e. the contract's `MAX_SCALE(g)`.
 *
 * `max(actualScale, 1)` rather than `actualScale` so a small image — whose
 * actual scale is 1 — still gets a real zoom range instead of being pinned at
 * fit, and so a nonsensical sub-1 actual scale can never invert the bounds.
 */
export function maxScale(g: Geometry): number {
	const ceiling = Math.max(actualScale(g), FIT) * MAX_SCALE_FACTOR;
	// A finite ratio can still overflow when multiplied: a bitmap reported at
	// Number.MAX_VALUE px gives a finite actual scale and an infinite ceiling,
	// which would silently disable the scale clamp entirely.
	return Number.isFinite(ceiling) ? ceiling : Number.MAX_VALUE;
}

/** Clamp a scale into `[FIT, maxScale(g)]`. */
export function clampScale(scale: number, g: Geometry): number {
	const max = maxScale(g);
	const s = finiteOr(scale, FIT);
	if (s < FIT) return FIT;
	if (s > max) return max;
	return s;
}

/** The largest `|offset|` an axis may take before a stage edge shows past the image. */
function panBound(stageExtent: number, fittedExtent: number, scale: number): number {
	// Saturated, not merely finiteness-checked: the product is what overflows,
	// and a finite `fittedExtent` near Number.MAX_VALUE times a finite scale is
	// still `Infinity`, which would make the bound infinite and disable the
	// clamp entirely.
	const stage = saneExtent(stageExtent);
	const scaled = saneExtent(saneExtent(fittedExtent) * finiteOr(scale, FIT));
	// An axis that does not overflow the stage is CENTRED, not free to roam.
	if (scaled <= stage) return 0;
	return (scaled - stage) / 2;
}

/**
 * Clamp the pan offsets for the state's own scale.
 *
 * Per axis: if the scaled extent is at most the stage, the offset is forced to
 * 0 — a smaller-than-stage axis is CENTRED, never draggable. Otherwise the
 * offset is bounded so no stage edge can show past the image edge.
 *
 * `scale` is passed through untouched (only sanitised for finiteness) — clamping
 * it is {@link clampScale}'s job, and {@link clampState} composes the two in the
 * order a resize needs.
 */
export function clampPan(state: ZoomState, g: Geometry): ZoomState {
	const scale = finiteOr(state.scale, FIT);
	const bx = panBound(g.stageW, g.fittedW, scale);
	const by = panBound(g.stageH, g.fittedH, scale);
	const x = finiteOr(state.x, 0);
	const y = finiteOr(state.y, 0);
	return {
		scale,
		x: clampSymmetric(x, bx),
		y: clampSymmetric(y, by),
	};
}

/**
 * Clamp scale FIRST, then pan — the order a geometry change requires.
 *
 * `maxScale` is geometry-dependent and geometry is viewport-dependent, so
 * ENLARGING the window lowers `actualScale` and with it `maxScale`, stranding a
 * previously-valid scale above the new ceiling. Clamping pan first would bound
 * it against a scale that is about to change (TASK-2455).
 */
export function clampState(state: ZoomState, g: Geometry): ZoomState {
	return clampPan({ ...state, scale: clampScale(state.scale, g) }, g);
}

/**
 * Zoom to `targetScale` keeping the image point currently under `anchor` under
 * `anchor` afterwards.
 *
 * The invariant, with `u` the anchor relative to the stage centre and
 * `k = newScale / oldScale`:
 *
 *     t' = u - k * (u - t)
 *
 * Anchor invariance holds exactly UNLESS the result has to be pan-clamped —
 * the bounds win, because letting the anchor survive would mean showing blank
 * stage past an image edge. A non-finite anchor falls back to the stage centre.
 *
 * The incoming state is normalised through {@link clampState} first, so the
 * invariant is stated against a VALID starting state. Clamping the incoming
 * scale while trusting its pan would mix a pan belonging to one scale with
 * another, and the invariant would then be false for the one caller shape most
 * likely to hit it — a geometry change that lowered the ceiling under a state
 * the consumer has not re-clamped yet.
 */
export function zoomTo(
	state: ZoomState,
	targetScale: number,
	anchor: Point,
	g: Geometry,
): ZoomState {
	const current = clampState(state, g);
	const from = current.scale;
	const to = clampScale(targetScale, g);
	const centre = stageCenter(g);
	const ux = finiteOr(anchor?.x, centre.x) - centre.x;
	const uy = finiteOr(anchor?.y, centre.y) - centre.y;
	// `from` came out of clampScale, so it is at least FIT — never a zero divide.
	const k = to / from;
	const x = ux - k * (ux - current.x);
	const y = uy - k * (uy - current.y);
	return clampPan({ scale: to, x, y }, g);
}

/**
 * Whether `state` is at fit, within {@link FIT_EPSILON}.
 *
 * ASYMMETRIC AT THE BOUNDARY, AND DELIBERATELY LEFT SO. `FIT + FIT_EPSILON`
 * reads as at-fit while `FIT - FIT_EPSILON` does not, because neither value is
 * exactly representable in binary and the subtraction lands a few ulps outside
 * the window. Widening the comparison to hide that would be papering over an
 * unreachable case: every scale this module emits has been through
 * {@link clampScale}, whose floor is FIT, so a sub-fit state cannot arise from
 * any sequence of operations on it. The asymmetry is only observable on a
 * hand-constructed state, and both legs are pinned by tests so a future change
 * to the floor cannot make it matter silently.
 */
export function isAtFit(state: ZoomState): boolean {
	return Math.abs(finiteOr(state.scale, FIT) - FIT) <= FIT_EPSILON;
}

/**
 * The double-click / double-tap toggle: fit <-> actual size.
 *
 * At fit (within {@link FIT_EPSILON}) it goes to {@link actualScale} anchored at
 * `anchor`; anywhere else it goes back to fit, centred. When actual size IS fit
 * — an image smaller than the stage — it goes to {@link TOGGLE_SMALL_SCALE}
 * instead, so the gesture always does something visible.
 *
 * DELIBERATE GENERALISATION OF THE CONTRACT. The written rule says "when
 * `actualScale === 1`, go to {@link TOGGLE_SMALL_SCALE}"; this uses
 * `actualScale <= FIT + FIT_EPSILON` instead. Exact equality serves the rule's
 * PURPOSE only on integer geometry — at a fractional `devicePixelRatio` a 1:1
 * image measures 1.0000000004 and at a rounded layout it can measure 1.0005,
 * and a toggle to either is a zoom of at most 0.05%: invisible, i.e. exactly
 * the no-op the rule exists to prevent. The epsilon is the same one
 * {@link isAtFit} uses, so "already at fit" and "actual size is fit" cannot
 * disagree about the same image.
 */
export function toggleFitOrActual(state: ZoomState, anchor: Point, g: Geometry): ZoomState {
	if (!isAtFit(state)) return reset();
	const actual = actualScale(g);
	const target = actual > FIT + FIT_EPSILON ? actual : TOGGLE_SMALL_SCALE;
	return zoomTo(state, target, anchor, g);
}

/**
 * The identity transform: fit, centred.
 *
 * Takes no geometry, which is precisely what makes it safe to call before a
 * bitmap exists — on open, on image change, and on close (TASK-2455).
 */
export function reset(): ZoomState {
	return { scale: FIT, x: 0, y: 0 };
}
