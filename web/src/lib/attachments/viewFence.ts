/**
 * The view-identity fence — ONE implementation of the invariant that a
 * continuation resuming after an `await` may only write state belonging to the
 * view the user is still looking at.
 *
 * WHY THIS EXISTS. Attachment surfaces (the item attachment strip, the Storage
 * tab) stay MOUNTED across the navigations that change what they show: the
 * strip lives outside ItemDetail's `{#key itemSlug}` block, and
 * `/{user}/{ws}/settings` is one SvelteKit route, so a workspace switch changes
 * `wsSlug` under a mounted tab. Every fetch, mutation and timer therefore has to
 * say which view it belongs to. Written out by hand at each call site, that
 * invariant failed the same way four review rounds running — always by
 * comparing only PART of the identity, almost always by forgetting the
 * workspace half. This module makes the identity a single value that is read in
 * exactly one place per component and carried on a token, so no call site can
 * assemble a partial one.
 *
 * THREE FENCES, THREE QUESTIONS. They are deliberately NOT collapsible into one
 * — a prior review round established that merging any two loses either A→B
 * suppression or same-item-Retry reconciliation:
 *
 *   1. REQUEST fence (`createFence`, restarted per request) — "is this RESPONSE
 *      still the current one?" A Retry supersedes its own predecessor here.
 *   2. VIEW fence (`createFence`, invalidated only when the view really
 *      changes) — "may this async CONTINUATION still reconcile local state?" A
 *      Retry is the same view reloading and must NOT invalidate it, or an
 *      in-flight delete of a row still on screen stops rolling back.
 *   3. PAINT-TIME fence (`createPaintFence`) — "does the CONTROL the user
 *      clicked belong to what is on screen?" Props update synchronously and
 *      effects flush later, so in between the DOM still shows the previous
 *      view. This one has to be at ENTRY: the other two run after an await, and
 *      no fence can unsend a request.
 *
 * Both (1) and (2) compare a captured GENERATION as well as the identity,
 * because a counter alone misses A→B→A (the identity matches again) and an
 * identity alone misses A→A (a superseded request for the same view).
 */

/**
 * The parts that name a view. A record rather than a tuple so the captured
 * snapshot can be read back by name — `token.value.ws` is the workspace the
 * request was ISSUED for, which is what callers should pass to the API instead
 * of re-reading the live prop.
 */
export type IdentityParts = Record<string, string | null | undefined>;

/**
 * Serialise the parts into a comparable key, or null when the view is not
 * addressable yet.
 *
 * A MISSING PART MAKES THE WHOLE KEY NULL. That is the load-bearing rule: a
 * view named by (workspace, item) is not identifiable while either half is
 * unknown, and a null key never matches anything — so a half-identified view
 * can't accidentally pass a fence.
 */
function serialize(parts: IdentityParts): string | null {
	// Sorted so the key does not depend on property declaration order.
	const names = Object.keys(parts).sort();
	if (names.length === 0) return null;
	const pairs: [string, string][] = [];
	for (const name of names) {
		const value = parts[name];
		if (value === null || value === undefined || value === '') return null;
		pairs.push([name, value]);
	}
	// JSON rather than a delimiter join: a hand-rolled separator has to be a
	// character the parts cannot contain, and "identity parts are always slugs
	// and uuids" is an assumption this module has no way to enforce. JSON quotes
	// and escapes both halves, so no two distinct part sets can serialise the
	// same — a collision here would silently let a stale fence pass, which is
	// the exact failure this module exists to prevent (Codex round 1).
	return JSON.stringify(pairs);
}

/** A captured identity: what the view was named when this token was taken. */
export interface IdentityToken<T extends IdentityParts> {
	/** Snapshot of the parts, for reading back the workspace/item that was captured. */
	readonly value: T;
	/** The comparable key, or null when the view wasn't addressable. */
	readonly key: string | null;
	/**
	 * True when the live view no longer matches this capture — including when
	 * the capture itself was never addressable.
	 */
	changed(): boolean;
}

export interface ViewIdentity<T extends IdentityParts> {
	/** The live key, or null when a part is missing. */
	key(): string | null;
	/** True when `key` names the live view. A null key never matches. */
	matches(key: string | null): boolean;
	/** Take a token for the live view. */
	capture(): IdentityToken<T>;
}

/**
 * Declare what names a view. `read` is called on every capture/compare and must
 * read the LIVE reactive values:
 *
 *     const view = viewIdentity(() => ({ ws: wsSlug, item: itemId }));
 *
 * This is the ONE place a component states its identity; every fence below is
 * built from it, so there is no second call site that could state a shorter one.
 */
export function viewIdentity<T extends IdentityParts>(read: () => T): ViewIdentity<T> {
	const identity: ViewIdentity<T> = {
		key: () => serialize(read()),
		matches: (key) => key !== null && key === serialize(read()),
		capture: () => {
			const value = { ...read() } as T;
			const key = serialize(value);
			return {
				value,
				key,
				changed: () => key === null || !identity.matches(key),
			};
		},
	};
	return identity;
}

/** An identity token that also carries a generation. */
export interface FenceToken<T extends IdentityParts> extends IdentityToken<T> {
	/**
	 * True when this token may no longer write: the fence was invalidated (or
	 * restarted) since it was taken, or the view moved on.
	 */
	stale(): boolean;
}

export interface Fence<T extends IdentityParts> {
	/**
	 * Take a token WITHOUT superseding outstanding ones. For the view fence, and
	 * for anything that must coexist with its siblings — two concurrent deletes
	 * must not invalidate each other.
	 */
	begin(): FenceToken<T>;
	/**
	 * Supersede every outstanding token, then take a new one. For a request
	 * fence: issuing request N+1 means request N's response may no longer write.
	 */
	restart(): FenceToken<T>;
	/** Supersede every outstanding token without taking a new one. */
	invalidate(): void;
}

/**
 * A generation + identity fence. Two of these per surface: one restarted per
 * request (fence 1) and one invalidated only on a real view change (fence 2).
 */
export function createFence<T extends IdentityParts>(identity: ViewIdentity<T>): Fence<T> {
	let generation = 0;

	function take(): FenceToken<T> {
		const captured = identity.capture();
		const gen = generation;
		return {
			value: captured.value,
			key: captured.key,
			changed: captured.changed,
			stale: () => gen !== generation || captured.changed(),
		};
	}

	return {
		begin: take,
		restart: () => {
			generation++;
			return take();
		},
		invalidate: () => {
			generation++;
		},
	};
}

export interface PaintFence<T extends IdentityParts> {
	/**
	 * Claim what is now on screen. Recording a token whose key is null (an
	 * un-addressable view) correctly claims nothing.
	 */
	record(token: IdentityToken<T> | null): void;
	/** The identity the last paint claimed, for reading the parts back. */
	painted(): IdentityToken<T> | null;
	/** True when what is painted is what the live props name. */
	isCurrent(): boolean;
}

/**
 * Fence 3. Deliberately LAGS the live props by the prop-update → effect-flush
 * window: that lag is the only thing that can answer "was this control rendered
 * for the view that is on screen NOW?", which the live props cannot — they
 * already read the NEW view while the OLD controls are still mounted.
 */
export function createPaintFence<T extends IdentityParts>(
	identity: ViewIdentity<T>
): PaintFence<T> {
	let painted: IdentityToken<T> | null = null;
	return {
		record: (token) => {
			painted = token && token.key !== null ? token : null;
		},
		painted: () => painted,
		isCurrent: () => painted !== null && identity.matches(painted.key),
	};
}
