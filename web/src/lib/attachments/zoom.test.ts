import { describe, it, expect } from 'vitest';
import {
	FIT,
	FIT_EPSILON,
	MAX_SCALE_FACTOR,
	TOGGLE_SMALL_SCALE,
	ZOOM_STEP,
	actualScale,
	clampPan,
	clampScale,
	clampState,
	isAtFit,
	maxScale,
	reset,
	stageCenter,
	toggleFitOrActual,
	zoomTo,
	type Geometry,
	type Point,
	type ZoomState,
} from './zoom';

// ---------------------------------------------------------------------------
// Helpers
//
// Geometry is built the way the BROWSER builds it — `object-fit: contain`,
// which scales down to fit and never up — so the fixtures cannot drift from the
// coordinate system the module documents. Re-typing fitted sizes by hand is how
// a test ends up asserting against a geometry the CSS never produces.
// ---------------------------------------------------------------------------

function containScale(stageW: number, stageH: number, natW: number, natH: number): number {
	return Math.min(stageW / natW, stageH / natH, 1);
}

function geom(stageW: number, stageH: number, natW: number, natH: number): Geometry {
	const k = containScale(stageW, stageH, natW, natH);
	return { stageW, stageH, fittedW: natW * k, fittedH: natH * k, naturalW: natW, naturalH: natH };
}

/**
 * The image-space point currently painted under `anchor`, measured in UNSCALED
 * fitted-box px from the image's centre. This is the quantity an anchored zoom
 * must leave unchanged; computing it from the documented transform rather than
 * from the implementation is what makes the invariance assertions independent.
 */
function imagePointUnder(state: ZoomState, g: Geometry, anchor: Point): Point {
	return {
		x: (anchor.x - g.stageW / 2 - state.x) / state.scale,
		y: (anchor.y - g.stageH / 2 - state.y) / state.scale,
	};
}

/** Where an image-space point paints, in stage-local px. Inverse of the above. */
function stagePointOf(state: ZoomState, g: Geometry, d: Point): Point {
	return {
		x: g.stageW / 2 + state.x + state.scale * d.x,
		y: g.stageH / 2 + state.y + state.scale * d.y,
	};
}

function expectClose(actual: number, expected: number, tol = 1e-9): void {
	expect(Math.abs(actual - expected)).toBeLessThanOrEqual(tol);
}

/** The pan bound the module promises for one axis, recomputed independently. */
function bound(stageExtent: number, fittedExtent: number, scale: number): number {
	return Math.max(0, (fittedExtent * scale - stageExtent) / 2);
}

function expectWithinBounds(state: ZoomState, g: Geometry, tol = 1e-9): void {
	const bx = bound(g.stageW, g.fittedW, state.scale);
	const by = bound(g.stageH, g.fittedH, state.scale);
	expect(state.x).toBeLessThanOrEqual(bx + tol);
	expect(state.x).toBeGreaterThanOrEqual(-bx - tol);
	expect(state.y).toBeLessThanOrEqual(by + tol);
	expect(state.y).toBeGreaterThanOrEqual(-by - tol);
	// The centring rule: an axis that does not overflow is pinned to 0, not
	// merely "within a zero-width bound" — assert it as its own property so a
	// bound-only implementation that leaves 1e-16 drift is still caught.
	if (bx === 0) expect(state.x).toBe(0);
	if (by === 0) expect(state.y).toBe(0);
}

// A square-ish image whose fitted box exactly fills the stage: the only shape
// for which a CORNER-anchored zoom lands exactly on the pan bound rather than
// outside it, so anchor invariance is testable there without the bounds
// legitimately overriding it.
const EXACT = geom(1000, 800, 2000, 1600);
// Landscape image in a landscape stage: fills the width, letterboxed vertically.
const WIDE = geom(1000, 800, 4000, 1000);
// Portrait image in a landscape stage: fills the height, pillarboxed.
const TALL = geom(1000, 800, 1000, 4000);
// Landscape image in a PORTRAIT stage.
const WIDE_IN_PORTRAIT = geom(800, 1000, 4000, 1000);
// An image smaller than the stage on both axes: `contain` does not upscale, so
// fit already is 1:1.
const SMALL = geom(1000, 800, 400, 300);

describe('geometry fixtures', () => {
	it('are built by the same contain rule the CSS applies', () => {
		expect(EXACT.fittedW).toBe(1000);
		expect(EXACT.fittedH).toBe(800);
		expect(WIDE.fittedW).toBe(1000);
		expect(WIDE.fittedH).toBe(250);
		expect(TALL.fittedW).toBe(200);
		expect(TALL.fittedH).toBe(800);
		expect(SMALL.fittedW).toBe(400);
		expect(SMALL.fittedH).toBe(300);
	});
});

describe('the contract constants', () => {
	// THE ONE PLACE a literal from the spec is written down. Every other test
	// references the exported constant, so behaviour tests cannot drift into
	// re-typed magic numbers — but without this, renumbering MAX_SCALE_FACTOR to
	// 5 or TOGGLE_SMALL_SCALE to 3 would silently change the product and keep the
	// whole suite green, because every assertion would move with it.
	it('carry the values PLAN-2392 specifies', () => {
		expect(FIT).toBe(1);
		expect(MAX_SCALE_FACTOR).toBe(4);
		expect(TOGGLE_SMALL_SCALE).toBe(2);
		expect(FIT_EPSILON).toBe(0.001);
		expect(ZOOM_STEP).toBe(1.25);
	});
});

describe('reset', () => {
	it('is fit, centred', () => {
		expect(reset()).toEqual({ scale: FIT, x: 0, y: 0 });
	});

	it('returns a fresh object each call so callers cannot alias state', () => {
		const a = reset();
		a.x = 99;
		expect(reset().x).toBe(0);
	});
});

describe('actualScale', () => {
	it('is the painted bitmap over the fitted box', () => {
		expect(actualScale(WIDE)).toBe(4000 / 1000);
		expect(actualScale(TALL)).toBe(1000 / 200);
	});

	it('is exactly fit for an image smaller than the stage — contain never upscales', () => {
		expect(actualScale(SMALL)).toBe(FIT);
	});

	it('is the WIDTH pair, not the height pair', () => {
		// `contain` preserves the aspect ratio, so on every realistic fixture the
		// two ratios agree and a height-based implementation is indistinguishable.
		// Pin the axis the contract names with a deliberately non-uniform
		// geometry — measurements DO diverge slightly per-axis once layout is
		// snapped to a fractional device-pixel grid.
		const nonUniform: Geometry = {
			stageW: 1000,
			stageH: 800,
			fittedW: 1000,
			fittedH: 100,
			naturalW: 3000,
			naturalH: 900,
		};
		expect(actualScale(nonUniform)).toBe(3);
		expect(actualScale(nonUniform)).not.toBe(nonUniform.naturalH / nonUniform.fittedH);
	});

	it('falls back to fit rather than dividing by a degenerate measurement', () => {
		expect(actualScale({ ...WIDE, fittedW: 0 })).toBe(FIT);
		expect(actualScale({ ...WIDE, fittedW: Number.NaN })).toBe(FIT);
		expect(actualScale({ ...WIDE, naturalW: 0 })).toBe(FIT);
	});

	it('grows when a higher-resolution bitmap swaps in under the same layout', () => {
		// The thumb-then-original path (TASK-2459): same fitted box, bigger
		// bitmap, so 1:1 — and with it the zoom ceiling — moves.
		const thumb = { ...WIDE, naturalW: 1024, naturalH: 256 };
		const original = { ...WIDE, naturalW: 4000, naturalH: 1000 };
		expect(maxScale(original)).toBeGreaterThan(maxScale(thumb));
	});
});

describe('maxScale', () => {
	it('is MAX_SCALE_FACTOR times 1:1', () => {
		expect(maxScale(WIDE)).toBe(actualScale(WIDE) * MAX_SCALE_FACTOR);
	});

	it('stays finite when a nonsensical bitmap size overflows the multiplication', () => {
		// A finite ratio can still overflow: an infinite ceiling would disable the
		// scale clamp altogether rather than merely raising it.
		// fittedW must be 1 for the RATIO itself to reach MAX_VALUE — with a
		// larger fitted box the ratio is small enough that x4 stays finite and the
		// guard is never exercised.
		const absurd: Geometry = {
			stageW: 1,
			stageH: 1,
			fittedW: 1,
			fittedH: 1,
			naturalW: Number.MAX_VALUE,
			naturalH: Number.MAX_VALUE,
		};
		expect(actualScale(absurd)).toBe(Number.MAX_VALUE);
		expect(Number.isFinite(actualScale(absurd))).toBe(true);
		expect(Number.isFinite(maxScale(absurd))).toBe(true);
		expect(clampScale(Number.MAX_VALUE, absurd)).toBeLessThanOrEqual(maxScale(absurd));
	});

	it('floors at fit so a small image still has a zoom range', () => {
		expect(maxScale(SMALL)).toBe(MAX_SCALE_FACTOR);
		// A nonsensical sub-1 actual scale must not invert the bounds.
		const upscaled: Geometry = { ...SMALL, fittedW: 800, fittedH: 600 };
		expect(actualScale(upscaled)).toBeLessThan(FIT);
		expect(maxScale(upscaled)).toBe(MAX_SCALE_FACTOR);
	});
});

describe('clampScale', () => {
	it('clamps to the fit floor and the maxScale ceiling', () => {
		expect(clampScale(0.25, WIDE)).toBe(FIT);
		expect(clampScale(1e6, WIDE)).toBe(maxScale(WIDE));
		expect(clampScale(2.5, WIDE)).toBe(2.5);
	});

	it('coerces a non-finite scale to fit rather than emitting NaN into a CSS string', () => {
		// Infinity falls back rather than saturating: it only ever arrives from a
		// caller-side arithmetic bug, and snapping it to maximum zoom would look
		// like a deliberate gesture.
		expect(clampScale(Number.NaN, WIDE)).toBe(FIT);
		expect(clampScale(Number.POSITIVE_INFINITY, WIDE)).toBe(FIT);
		expect(clampScale(Number.NEGATIVE_INFINITY, WIDE)).toBe(FIT);
	});

	it('normalises negative zero away — a centred axis is exactly +0', () => {
		expect(Object.is(clampPan({ scale: 2, x: -5, y: -5 }, WIDE).y, 0)).toBe(true);
		expect(Object.is(clampPan({ scale: 2, x: -0, y: -0 }, WIDE).x, 0)).toBe(true);
	});
});

describe('clampPan', () => {
	it('centres an axis whose scaled extent fits inside the stage', () => {
		// WIDE at 2x: the width overflows, the height still does not.
		const clamped = clampPan({ scale: 2, x: 300, y: 300 }, WIDE);
		expect(clamped.y).toBe(0);
		expect(clamped.x).toBe(300);
	});

	it('centres BOTH axes at fit — the parity property', () => {
		for (const g of [EXACT, WIDE, TALL, WIDE_IN_PORTRAIT, SMALL]) {
			expect(clampPan({ scale: FIT, x: 500, y: -500 }, g)).toEqual({ scale: FIT, x: 0, y: 0 });
		}
	});

	it('bounds all four edges of a wider-than-stage image', () => {
		const g = WIDE;
		const s = 2;
		const bx = bound(g.stageW, g.fittedW, s); // 500
		expect(bx).toBe(500);
		expect(clampPan({ scale: s, x: 10_000, y: 0 }, g).x).toBe(bx);
		expect(clampPan({ scale: s, x: -10_000, y: 0 }, g).x).toBe(-bx);
		// The vertical axis is the non-overflowing one here: both directions pin to 0.
		expect(clampPan({ scale: s, x: 0, y: 10_000 }, g).y).toBe(0);
		expect(clampPan({ scale: s, x: 0, y: -10_000 }, g).y).toBe(0);
	});

	it('bounds all four edges of a taller-than-stage image', () => {
		const g = TALL;
		const s = 3;
		const bx = bound(g.stageW, g.fittedW, s); // 200*3=600 > 1000? no -> 0
		const by = bound(g.stageH, g.fittedH, s); // 800*3=2400 -> 800
		expect(bx).toBe(0);
		expect(by).toBe(800);
		expect(clampPan({ scale: s, x: 10_000, y: 0 }, g).x).toBe(0);
		expect(clampPan({ scale: s, x: -10_000, y: 0 }, g).x).toBe(0);
		expect(clampPan({ scale: s, x: 0, y: 10_000 }, g).y).toBe(by);
		expect(clampPan({ scale: s, x: 0, y: -10_000 }, g).y).toBe(-by);
	});

	it('bounds a corner — both axes at once, in both orientations', () => {
		for (const g of [EXACT, WIDE_IN_PORTRAIT]) {
			const s = 4;
			const bx = bound(g.stageW, g.fittedW, s);
			const by = bound(g.stageH, g.fittedH, s);
			for (const [sx, sy] of [
				[1, 1],
				[1, -1],
				[-1, 1],
				[-1, -1],
			]) {
				const c = clampPan({ scale: s, x: sx * 10_000, y: sy * 10_000 }, g);
				// `|| 0` only normalises the -0 a zero bound produces on the test
				// side; a real bound is never zero, so no expectation is weakened.
				expect(c.x).toBe(sx * bx || 0);
				expect(c.y).toBe(sy * by || 0);
			}
		}
	});

	it('leaves an in-bounds offset untouched', () => {
		expect(clampPan({ scale: 2, x: 123.5, y: 0 }, WIDE)).toEqual({ scale: 2, x: 123.5, y: 0 });
	});

	it('does not mutate its argument', () => {
		const state = { scale: 2, x: 10_000, y: 10_000 };
		clampPan(state, WIDE);
		expect(state).toEqual({ scale: 2, x: 10_000, y: 10_000 });
	});

	it('passes scale through — clamping it is clampScale/clampState work', () => {
		expect(clampPan({ scale: 99, x: 0, y: 0 }, WIDE).scale).toBe(99);
		expect(clampPan({ scale: Number.NaN, x: 0, y: 0 }, WIDE).scale).toBe(FIT);
	});
});

describe('clampState (the resize path)', () => {
	it('clamps scale FIRST, then bounds pan against the NEW scale', () => {
		// Enlarging the window lowers actualScale and with it maxScale, stranding
		// a scale above the new ceiling (TASK-2455).
		const before = geom(1000, 800, 4000, 1000); // actual 4 -> max 16
		const after = geom(2000, 1600, 4000, 1000); // fitted 2000x500, actual 2 -> max 8
		const zoomed = { scale: 16, x: bound(before.stageW, before.fittedW, 16), y: 0 };
		const next = clampState(zoomed, after);
		expect(next.scale).toBe(maxScale(after));
		expectWithinBounds(next, after);
		// Pan-first would have kept the OLD scale's much larger bound and left x
		// far outside the new one; assert the value, not just the invariant.
		expect(next.x).toBe(bound(after.stageW, after.fittedW, maxScale(after)));
	});

	it('preserves a scale that is still inside the new bounds', () => {
		const after = geom(2000, 1600, 4000, 1000);
		expect(clampState({ scale: 3, x: 0, y: 0 }, after).scale).toBe(3);
	});
});

describe('zoomTo anchor invariance', () => {
	const anchors: Array<[string, Point]> = [
		['stage centre', { x: 500, y: 400 }],
		['an off-centre interior point', { x: 250, y: 600 }],
		['the top-left corner', { x: 0, y: 0 }],
	];

	for (const [label, anchor] of anchors) {
		it(`keeps the image point under ${label} under it`, () => {
			const from: ZoomState = { scale: 1, x: 0, y: 0 };
			const before = imagePointUnder(from, EXACT, anchor);
			const after = zoomTo(from, 2, anchor, EXACT);
			const painted = stagePointOf(after, EXACT, before);
			expectClose(painted.x, anchor.x);
			expectClose(painted.y, anchor.y);
		});
	}

	it('holds across a chain of steps from an already-panned state', () => {
		const anchor = { x: 812.5, y: 137.25 };
		let state: ZoomState = clampPan({ scale: 2, x: -200, y: -150 }, EXACT);
		for (let i = 0; i < 6; i++) {
			const before = imagePointUnder(state, EXACT, anchor);
			const next = zoomTo(state, state.scale * ZOOM_STEP, anchor, EXACT);
			// Once the chain reaches the ceiling the scale stops changing, and an
			// unchanged scale trivially preserves the anchor — assert only while
			// the step is real.
			if (next.scale !== state.scale) {
				const painted = stagePointOf(next, EXACT, before);
				expectClose(painted.x, anchor.x, 1e-8);
				expectClose(painted.y, anchor.y, 1e-8);
			}
			state = next;
		}
		expect(state.scale).toBeGreaterThan(2);
	});

	it('is not a no-op dressed up as invariance — the transform actually moves', () => {
		// The invariance assertions above all pass for an implementation that
		// refuses to zoom at all. This is the control leg.
		const out = zoomTo({ scale: 1, x: 0, y: 0 }, 2, { x: 0, y: 0 }, EXACT);
		expect(out.scale).toBe(2);
		expect(out.x).not.toBe(0);
		expect(out.y).not.toBe(0);
	});

	it('lets the pan bounds win over the anchor when they conflict', () => {
		// A corner anchor on a letterboxed image would need an offset outside the
		// bound; showing blank stage past an image edge is worse than losing the
		// anchor, so the bound wins — and the non-overflowing axis stays centred.
		const out = zoomTo({ scale: 1, x: 0, y: 0 }, 2, { x: 0, y: 0 }, WIDE);
		expectWithinBounds(out, WIDE);
		expect(out.x).toBe(bound(WIDE.stageW, WIDE.fittedW, 2));
		expect(out.y).toBe(0);
	});

	it('clamps the target scale into [FIT, maxScale]', () => {
		const g = WIDE;
		expect(zoomTo({ scale: 2, x: 0, y: 0 }, 0.1, { x: 0, y: 0 }, g).scale).toBe(FIT);
		expect(zoomTo({ scale: 2, x: 0, y: 0 }, 1e6, { x: 0, y: 0 }, g).scale).toBe(maxScale(g));
	});

	it('zooming back out to fit re-centres both axes', () => {
		const zoomed = zoomTo({ scale: 1, x: 0, y: 0 }, 3, { x: 0, y: 0 }, EXACT);
		const back = zoomTo(zoomed, FIT, { x: 0, y: 0 }, EXACT);
		expect(back).toEqual({ scale: FIT, x: 0, y: 0 });
	});

	it('falls back to the stage centre for a non-finite anchor', () => {
		const centred = zoomTo({ scale: 1, x: 0, y: 0 }, 2, stageCenter(EXACT), EXACT);
		const nan = zoomTo({ scale: 1, x: 0, y: 0 }, 2, { x: Number.NaN, y: Number.NaN }, EXACT);
		expect(nan).toEqual(centred);
	});

	it('does not mutate its argument', () => {
		const state = { scale: 1, x: 0, y: 0 };
		zoomTo(state, 3, { x: 0, y: 0 }, EXACT);
		expect(state).toEqual({ scale: 1, x: 0, y: 0 });
	});

	it('normalises an already-invalid incoming state before anchoring', () => {
		// The shape a consumer hits after a resize lowers the ceiling under a state
		// it has not re-clamped yet. Clamping the incoming SCALE while trusting its
		// PAN would apply a pan belonging to the old scale, so the invariant must
		// be stated against the normalised state — assert it exactly that way.
		const g = WIDE;
		// Pan out of bounds for its own scale, which is what a shrinking resize
		// leaves behind. Bound at scale 2 is 500, so 700 is 200px past it.
		const stale: ZoomState = { scale: 2, x: 700, y: 0 };
		const normalised = clampState(stale, g);
		expect(normalised.x).toBe(500);

		const out = zoomTo(stale, 2.2, stageCenter(g), g);
		expect(out).toEqual(zoomTo(normalised, 2.2, stageCenter(g), g));
		expectWithinBounds(out, g);

		// Not vacuous: the rejected alternative — clamp the incoming SCALE, trust
		// the incoming PAN — is written out here and lands somewhere else, so the
		// assertion above has something to catch. A centre anchor scales the pan
		// by k, and 700 * 1.1 = 770 exceeds the new bound while 500 * 1.1 does not.
		expect(out.x).toBe(550);
		expect(clampPan({ scale: 2.2, x: 700 * 1.1, y: 0 }, g).x).toBe(600);
	});
});

describe('the drag contract (TASK-2458 consumes this)', () => {
	// A drag MUST be computed from the state captured at pointer-down plus the
	// total pointer delta. These two legs pin why: clamping is lossy, so feeding
	// each move delta into the previous clamped result silently changes the
	// gesture's feel at an edge.
	const g = WIDE;
	const scale = 2;
	const bx = bound(g.stageW, g.fittedW, scale); // 500

	it('takes up the slack when the drag is computed from the pointer-down baseline', () => {
		const start: ZoomState = { scale, x: 0, y: 0 };
		const overshoot = clampPan({ ...start, x: bx + 150 }, g);
		expect(overshoot.x).toBe(bx);
		// Pointer comes back 30px: total delta is +bx+120, still inside the bound.
		const back = clampPan({ ...start, x: bx + 150 - 30 }, g);
		expect(back.x).toBe(bx);
	});

	it('moves immediately — the wrong feel — if deltas are accumulated instead', () => {
		// The control leg. Same pointer path, incremental composition: the image
		// starts moving the instant the pointer reverses, 30px early.
		const wrong = clampPan({ scale, x: clampPan({ scale, x: bx + 150, y: 0 }, g).x - 30, y: 0 }, g);
		expect(wrong.x).toBe(bx - 30);
		expect(wrong.x).not.toBe(bx);
	});

	it('an in-bounds drag moves by exactly the pointer delta', () => {
		// The positive leg: "pan is clamped" alone passes with pan disabled.
		const moved = clampPan({ scale, x: 0 + 123.75, y: 0 }, g);
		expect(moved.x).toBe(123.75);
	});
});

describe('stageCenter', () => {
	it('is half the stage box', () => {
		expect(stageCenter(WIDE)).toEqual({ x: 500, y: 400 });
	});
});

describe('isAtFit / toggleFitOrActual', () => {
	it('at fit, goes to actual size anchored at the pointer', () => {
		const anchor = { x: 700, y: 300 };
		const before = imagePointUnder(reset(), WIDE, anchor);
		const out = toggleFitOrActual(reset(), anchor, WIDE);
		expect(out.scale).toBe(actualScale(WIDE));
		// Anchored, to the extent the bounds allow: the overflowing axis keeps the
		// point, the centred axis cannot.
		const painted = stagePointOf(out, WIDE, before);
		expectClose(painted.x, anchor.x);
		expectWithinBounds(out, WIDE);
	});

	it('away from fit, goes back to fit CENTRED regardless of the anchor', () => {
		const zoomed = zoomTo(reset(), 3, { x: 0, y: 0 }, WIDE);
		expect(toggleFitOrActual(zoomed, { x: 900, y: 700 }, WIDE)).toEqual(reset());
	});

	it('returns on a second toggle — the gesture is two-way', () => {
		const anchor = { x: 700, y: 300 };
		const once = toggleFitOrActual(reset(), anchor, WIDE);
		const twice = toggleFitOrActual(once, anchor, WIDE);
		expect(once.scale).toBeGreaterThan(FIT);
		expect(twice).toEqual(reset());
	});

	it('treats the epsilon boundary as at-fit, and just past it as not', () => {
		const inside: ZoomState = { scale: FIT + FIT_EPSILON, x: 0, y: 0 };
		const outside: ZoomState = { scale: FIT + FIT_EPSILON * 2, x: 0, y: 0 };
		expect(isAtFit(inside)).toBe(true);
		expect(isAtFit(outside)).toBe(false);
		expect(toggleFitOrActual(inside, { x: 500, y: 400 }, WIDE).scale).toBe(actualScale(WIDE));
		expect(toggleFitOrActual(outside, { x: 500, y: 400 }, WIDE)).toEqual(reset());
	});

	it('is never a no-op on an image whose actual size IS fit', () => {
		const out = toggleFitOrActual(reset(), { x: 500, y: 400 }, SMALL);
		expect(actualScale(SMALL)).toBe(FIT);
		expect(out.scale).toBe(TOGGLE_SMALL_SCALE);
		// And it still toggles back.
		expect(toggleFitOrActual(out, { x: 500, y: 400 }, SMALL)).toEqual(reset());
	});

	it('is never a no-op when fractional geometry puts actual size a hair above fit', () => {
		// A 1:1 image measured at dpr 2.75: the ratio is 1.0000000004, not 1, so an
		// `=== 1` test would toggle to a scale the user cannot see.
		const hair: Geometry = { ...SMALL, naturalW: 400.0000001, naturalH: 300.0000001 };
		expect(actualScale(hair)).toBeGreaterThan(FIT);
		expect(toggleFitOrActual(reset(), { x: 500, y: 400 }, hair).scale).toBe(TOGGLE_SMALL_SCALE);
	});
});

// ---------------------------------------------------------------------------
// Fractional geometry / non-integer devicePixelRatio.
//
// The browser lays out on DEVICE-pixel boundaries, which are not CSS-pixel
// boundaries at dpr 1.25 / 1.5 / 2.75 — so every measurement the viewer reads
// is fractional there. An implementation that rounds looks correct on the
// integer fixtures above and drifts the anchored point visibly across a
// sequence of steps.
// ---------------------------------------------------------------------------

describe('fractional geometry and non-integer devicePixelRatio', () => {
	function snap(css: number, dpr: number): number {
		return Math.round(css * dpr) / dpr;
	}

	const dprs = [1.25, 1.5, 1.75, 2.5, 2.75];

	for (const dpr of dprs) {
		describe(`dpr ${dpr}`, () => {
			// 92vw x 92vh of an odd viewport, snapped to the device grid.
			const stageW = snap(0.92 * 1367, dpr);
			const stageH = snap(0.92 * 769, dpr);
			const g = geom(stageW, stageH, 3021, 2013);

			it('produces non-integer geometry (otherwise the case proves nothing)', () => {
				expect(Number.isInteger(g.fittedW) && Number.isInteger(g.fittedH)).toBe(false);
			});

			it('keeps the anchored point under the cursor to well under one device pixel', () => {
				const tol = 1 / dpr / 1000;
				const anchor = { x: stageW * 0.317, y: stageH * 0.733 };
				let state = reset();
				for (let i = 0; i < 8; i++) {
					const before = imagePointUnder(state, g, anchor);
					const next = zoomTo(state, state.scale * ZOOM_STEP, anchor, g);
					const bx = bound(g.stageW, g.fittedW, next.scale);
					const by = bound(g.stageH, g.fittedH, next.scale);
					const painted = stagePointOf(next, g, before);
					// Only where the bounds did not legitimately override the anchor.
					if (Math.abs(next.x) < bx - 1 && next.scale !== state.scale) {
						expectClose(painted.x, anchor.x, tol);
					}
					if (Math.abs(next.y) < by - 1 && next.scale !== state.scale) {
						expectClose(painted.y, anchor.y, tol);
					}
					expectWithinBounds(next, g);
					state = next;
				}
			});

			it('does not round the transform to whole CSS pixels', () => {
				const out = zoomTo(reset(), 2.5, { x: stageW * 0.13, y: stageH * 0.87 }, g);
				expect(Number.isInteger(out.x) && Number.isInteger(out.y)).toBe(false);
			});

			it('bounds a fractional geometry exactly, with no gap or overshoot', () => {
				const s = maxScale(g);
				const far = clampPan({ scale: s, x: 1e9, y: 1e9 }, g);
				expectClose(far.x, bound(g.stageW, g.fittedW, s), 0);
				expectClose(far.y, bound(g.stageH, g.fittedH, s), 0);
			});
		});
	}
});

// ---------------------------------------------------------------------------
// RTL.
//
// The module's coordinates are STAGE-LOCAL — measured from the stage box's
// physical left edge, which is what `getBoundingClientRect()` reports in either
// direction. `direction: rtl` mirrors the LAYOUT, not that measurement, so the
// math must be exactly direction-symmetric: mirror the anchor and the pan, and
// the result must be the mirror of the original. Any place the implementation
// leaked a signed horizontal direction (an inline-start assumption, a
// `stageW - x`) breaks this and nothing else does.
// ---------------------------------------------------------------------------

describe('RTL / horizontal direction symmetry', () => {
	function mirrorPoint(p: Point, g: Geometry): Point {
		return { x: g.stageW - p.x, y: p.y };
	}
	function mirrorState(s: ZoomState): ZoomState {
		return { scale: s.scale, x: -s.x, y: s.y };
	}

	const cases: Array<[string, Geometry]> = [
		['exact fill', EXACT],
		['landscape in landscape', WIDE],
		['portrait in landscape', TALL],
		['landscape in portrait', WIDE_IN_PORTRAIT],
	];

	for (const [label, g] of cases) {
		it(`zoomTo is mirror-symmetric (${label})`, () => {
			const anchor = { x: g.stageW * 0.19, y: g.stageH * 0.62 };
			const start: ZoomState = clampPan({ scale: 2, x: 37.5, y: -12.25 }, g);
			const out = zoomTo(start, 3.5, anchor, g);
			const mirrored = zoomTo(mirrorState(start), 3.5, mirrorPoint(anchor, g), g);
			expect(mirrored.scale).toBe(out.scale);
			expectClose(mirrored.x, -out.x, 1e-9);
			expectClose(mirrored.y, out.y, 1e-9);
		});

		it(`clampPan is mirror-symmetric (${label})`, () => {
			const s = 3;
			const state: ZoomState = { scale: s, x: 1e9, y: 1e9 };
			const a = clampPan(state, g);
			const b = clampPan(mirrorState(state), g);
			expectClose(b.x, -a.x, 0);
		});
	}

	it('the symmetry assertion can fail — a direction-flipped anchor breaks it', () => {
		// Control leg: proves the mirror test is load-bearing rather than
		// tautological. Mirroring the anchor WITHOUT mirroring the pan is exactly
		// the bug shape the test exists to catch.
		const g = EXACT;
		const anchor = { x: g.stageW * 0.19, y: g.stageH * 0.62 };
		const start: ZoomState = { scale: 2, x: 37.5, y: 0 };
		const out = zoomTo(start, 3.5, anchor, g);
		const halfMirrored = zoomTo(start, 3.5, mirrorPoint(anchor, g), g);
		expect(Math.abs(halfMirrored.x - -out.x)).toBeGreaterThan(1);
	});
});

// ---------------------------------------------------------------------------
// The hard bound: NO sequence of operations escapes [FIT, maxScale] or the pan
// bounds. Individually-correct operations can still compose into an escape —
// this is the property the phase's review round called binding.
// ---------------------------------------------------------------------------

describe('bounds no sequence of operations can escape', () => {
	function lcg(seed: number): () => number {
		let s = seed >>> 0;
		return () => {
			s = (Math.imul(s, 1664525) + 1013904223) >>> 0;
			return s / 0x1_0000_0000;
		};
	}

	const fixtures: Array<[string, Geometry]> = [
		['exact fill', EXACT],
		['landscape in landscape', WIDE],
		['portrait in landscape', TALL],
		['landscape in portrait', WIDE_IN_PORTRAIT],
		['smaller than the stage', SMALL],
		['fractional', geom(1257.6, 707.2, 3021, 2013)],
	];

	for (const [label, g] of fixtures) {
		it(`holds over 2000 pseudo-random operations (${label})`, () => {
			const rand = lcg(0xc0ffee);
			let state = reset();
			let sawCeiling = false;
			let sawFloor = false;
			let sawBoundedPan = false;

			for (let i = 0; i < 2000; i++) {
				const anchor = { x: rand() * g.stageW * 1.4 - g.stageW * 0.2, y: rand() * g.stageH * 1.4 - g.stageH * 0.2 };
				const roll = rand();
				if (roll < 0.35) {
					state = zoomTo(state, state.scale * ZOOM_STEP, anchor, g);
				} else if (roll < 0.6) {
					state = zoomTo(state, state.scale / ZOOM_STEP, anchor, g);
				} else if (roll < 0.7) {
					// A wild target, as a wheel burst or a hostile caller would give.
					state = zoomTo(state, (rand() - 0.5) * 1e4, anchor, g);
				} else if (roll < 0.85) {
					// A drag: an unclamped offset handed straight to clampPan.
					state = clampPan(
						{ ...state, x: state.x + (rand() - 0.5) * 4000, y: state.y + (rand() - 0.5) * 4000 },
						g,
					);
				} else if (roll < 0.95) {
					state = toggleFitOrActual(state, anchor, g);
				} else {
					state = clampState(state, g);
				}

				expect(Number.isFinite(state.scale)).toBe(true);
				expect(Number.isFinite(state.x)).toBe(true);
				expect(Number.isFinite(state.y)).toBe(true);
				expect(state.scale).toBeGreaterThanOrEqual(FIT);
				expect(state.scale).toBeLessThanOrEqual(maxScale(g));
				expectWithinBounds(state, g);

				if (state.scale === maxScale(g)) sawCeiling = true;
				if (state.scale === FIT) sawFloor = true;
				if (
					state.scale > FIT &&
					Math.abs(state.x) === bound(g.stageW, g.fittedW, state.scale) &&
					state.x !== 0
				) {
					sawBoundedPan = true;
				}
			}

			// Coverage guards: without these the invariants above could pass on a
			// walk that never approached a bound.
			expect(sawCeiling).toBe(true);
			expect(sawFloor).toBe(true);
			expect(sawBoundedPan).toBe(true);
		});
	}
});

describe('degenerate geometry', () => {
	const zero: Geometry = { stageW: 0, stageH: 0, fittedW: 0, fittedH: 0, naturalW: 0, naturalH: 0 };

	it('never emits NaN into the transform', () => {
		for (const out of [
			zoomTo(reset(), 3, { x: 10, y: 10 }, zero),
			clampPan({ scale: 3, x: 10, y: 10 }, zero),
			toggleFitOrActual(reset(), { x: 10, y: 10 }, zero),
			clampState({ scale: 99, x: 10, y: 10 }, zero),
		]) {
			expect(Number.isFinite(out.scale)).toBe(true);
			expect(Number.isFinite(out.x)).toBe(true);
			expect(Number.isFinite(out.y)).toBe(true);
		}
	});

	it('centres everything when there is no geometry to pan within', () => {
		expect(clampPan({ scale: 3, x: 10, y: 10 }, zero)).toEqual({ scale: 3, x: 0, y: 0 });
	});
});

/**
 * Round 3 of the per-task Codex review took the numerical-robustness angle and
 * found three P2s. Each is pinned here by a test that FAILS against the code as
 * it stood before the fix — the point of the exercise, since a test that passes
 * either way would have let all three back in.
 */
describe('extreme finite geometry (round 3)', () => {
	it('keeps the pan bound finite when extent * scale would overflow', () => {
		// Before the fix `fittedW * scale` was Infinity, so the bound was
		// Infinity, so clampSymmetric left ANY offset untouched — the clamp
		// silently stopped clamping and blank stage could show past the image.
		const huge: Geometry = {
			stageW: Number.MAX_VALUE,
			stageH: 1000,
			fittedW: Number.MAX_VALUE * 0.6,
			fittedH: 800,
			naturalW: Number.MAX_VALUE * 0.6,
			naturalH: 800,
		};
		const out = clampPan({ scale: 2, x: Number.MAX_VALUE, y: 0 }, huge);
		expect(Number.isFinite(out.x)).toBe(true);
		expect(out.x).toBeLessThan(Number.MAX_VALUE);
	});

	it('does not let a subnormal fitted extent turn actualScale infinite', () => {
		const subnormal: Geometry = {
			stageW: 1000,
			stageH: 1000,
			fittedW: Number.MIN_VALUE,
			fittedH: Number.MIN_VALUE,
			naturalW: 4000,
			naturalH: 4000,
		};
		expect(Number.isFinite(actualScale(subnormal))).toBe(true);
	});

	it('leaves the double-click toggle doing something visible on that geometry', () => {
		// The regression this guards: an infinite actualScale reached zoomTo,
		// clampScale rejected it as non-finite and fell back to fit, and the
		// toggle became the silent no-op TOGGLE_SMALL_SCALE exists to prevent.
		const subnormal: Geometry = {
			stageW: 1000,
			stageH: 1000,
			fittedW: Number.MIN_VALUE,
			fittedH: Number.MIN_VALUE,
			naturalW: 4000,
			naturalH: 4000,
		};
		const out = toggleFitOrActual(reset(), { x: 500, y: 500 }, subnormal);
		expect(out.scale).toBeGreaterThan(FIT + FIT_EPSILON);
	});
});

describe('the fit epsilon boundary is asymmetric, and cannot matter (round 3)', () => {
	it('reads FIT + FIT_EPSILON as at-fit but FIT - FIT_EPSILON as not', () => {
		// Neither value is exactly representable in binary; the subtraction lands
		// a few ulps outside the window. Pinned rather than papered over.
		expect(isAtFit({ scale: FIT + FIT_EPSILON, x: 0, y: 0 })).toBe(true);
		expect(isAtFit({ scale: FIT - FIT_EPSILON, x: 0, y: 0 })).toBe(false);
	});

	it('cannot arise, because no emitted scale is ever below FIT', () => {
		// This is what makes the asymmetry unobservable in practice. If the floor
		// ever stops being FIT, this fails and the asymmetry becomes real.
		const g: Geometry = {
			stageW: 1000,
			stageH: 800,
			fittedW: 900,
			fittedH: 700,
			naturalW: 3600,
			naturalH: 2800,
		};
		for (const attempt of [-99, 0, FIT - FIT_EPSILON, 0.5, Number.MIN_VALUE]) {
			expect(clampScale(attempt, g)).toBeGreaterThanOrEqual(FIT);
			expect(clampState({ scale: attempt, x: 0, y: 0 }, g).scale).toBeGreaterThanOrEqual(FIT);
			expect(zoomTo(reset(), attempt, { x: 0, y: 0 }, g).scale).toBeGreaterThanOrEqual(FIT);
		}
	});
});
