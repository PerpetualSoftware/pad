// The single owner of "is Chat open, and on what" (TASK-4315 PR 3b).
//
// Chat opens as a SHALLOW navigation: `goto('/messages…', { shallow: true })`
// changes the URL and pushes a history entry but keeps the underlying route
// mounted (SvelteKit 3). `page.url` stays the route beneath; `page.shallow`
// holds the Chat target, so a closed surface is simply `page.shallow === null`.
// A reload of a shallow URL is an ordinary route load, which is the direct
// entry: `/messages/N` rendered as a route inside Pad's chrome.
//
// `goto(…, { shallow: true, state })` REPLACES `page.state`, so every call here
// spreads the current state first (the split pane's depth lives there).
//
// Phone (<=768px) and route mode (the underlying URL is already /messages*) use
// a plain `goto`; nothing here changes how a phone reaches Chat.
import { untrack } from 'svelte';
import { goto } from '$app/navigation';
import { page } from '$app/state';
import { browser } from '$app/env';
import { viewport } from '$lib/stores/breakpoint.svelte';
import { authStore } from '$lib/stores/auth.svelte';

export type ChatTarget =
	| { kind: 'inbox' }
	| { kind: 'thread'; n: number }
	| { kind: 'notFound' };

/** The conversation number a /messages/[n] URL names, or null when it cannot name one. */
export function parseThreadNumber(raw: string): number | null {
	return /^[1-9][0-9]*$/.test(raw) ? Number(raw) : null;
}

/**
 * What a pathname asks of Chat: the inbox, a thread, or a number that cannot
 * name one. null means the path is not Chat (/messagesfoo, /messages/1/x).
 */
export function parseChatPath(pathname: string): ChatTarget | null {
	const m = /^\/messages(?:\/([^/]*))?\/?$/.exec(pathname);
	if (!m) return null;
	if (m[1] === undefined || m[1] === '') return { kind: 'inbox' };
	const n = parseThreadNumber(m[1]);
	return n === null ? { kind: 'notFound' } : { kind: 'thread', n };
}

function sameTarget(a: ChatTarget | null, b: ChatTarget | null): boolean {
	if (!a || !b || a.kind !== b.kind) return false;
	return a.kind !== 'thread' || a.n === (b as { n: number }).n;
}

function plainLeftClick(e: MouseEvent): boolean {
	if (e.defaultPrevented || e.button !== 0) return false;
	if (e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return false;
	const a = (e.currentTarget as Element | null)?.closest?.('a');
	if (a && (a.target && a.target !== '_self')) return false;
	if (a?.hasAttribute('download')) return false;
	return true;
}

// history.go has no completion promise, so a second Close or Back issued before
// the first one's popstate lands would pop past the opener. A traversal we issue
// latches until it has SETTLED: its popstate has fired AND the surface state has
// moved off the key it was issued from (Kit applies the state-only update a tick
// after the popstate, and that update raises no afterNavigate). A state change
// alone does not release: an open() completing mid-traversal moves the key too.
// The fallback is only a last resort for a popstate that never arrives.
const GO_FALLBACK_MS = 3000;
type GoLatch = {
	from: string;
	popped: boolean;
	timer: ReturnType<typeof setTimeout>;
	/** Runs once the surface is closed and the traversal has landed. */
	then: (() => void) | null;
};
let goLatch: GoLatch | null = null;

// Page-owned URL writes (a board filter, a search, a view) made while Chat is
// open. A shallow navigation or a replaceState under an open Chat would rewrite
// Chat's own history entry, so the write waits here and runs once Chat has
// closed, by whatever path (see onClosed). One entry per key: last wins.
type Deferred = { pathname: string; fence: () => boolean; run: () => void | Promise<unknown> };
const deferred = new Map<string, Deferred>();
// Heals that cannot wait (a renamed slug, an aliasing ?item=): they close Chat
// first and run after it, queued here in arrival order so a Close already in
// flight cannot swallow them. A heal is bound to the page it was queued on (its
// pathname): if that is no longer the page when it would run, the user went
// somewhere else and the heal is stale, so it is skipped rather than overriding
// them. A heal REPORTS what it did: true, or a promise resolving true, means it
// navigated (its navigation settled); anything else (false, undefined, a rejected
// or throwing run) is "did not, or not known", and the page's held writes stay.
type HealOutcome = void | boolean | Promise<unknown>;
type Heal = { run: () => HealOutcome; path: string };
const heals: Heal[] = [];

/** The `?new=1` strip an ItemDetail holds; a heal that retargets the item drops `new` itself. */
export const STRIP_NEW_KEY = 'item-detail#strip-new';

/** Run the queued heals; the pathnames of those that reported a navigation. Sync unless one returned a promise. */
function runHeals(): string[] | Promise<string[]> {
	const outs: Array<string | null | Promise<string | null>> = [];
	for (const h of heals.splice(0)) {
		if (h.path !== page.url.pathname) continue;
		try {
			const r = h.run();
			if (r === true) outs.push(h.path);
			else if (r && typeof (r as Promise<unknown>).then === 'function') {
				outs.push(
					Promise.resolve(r).then(
						(v) => (v === true ? h.path : null),
						(e) => {
							console.error('[chatSurface] heal failed', e);
							return null;
						}
					)
				);
			}
		} catch (e) {
			console.error('[chatSurface] heal failed', e);
		}
	}
	if (outs.some((o) => o instanceof Promise)) {
		return Promise.all(outs).then((all) => all.filter((p): p is string => p !== null));
	}
	return outs.filter((p): p is string => typeof p === 'string');
}

// The flush queues in flight (see flushDeferred): a real navigation away must reach
// the writes still waiting in them, not only the ones in the Map.
const flushing = new Set<Deferred[]>();
// Heal navigations in flight: until they settle, the writes they may supersede are not flushed.
let healWait = 0;
// The pages whose heal navigation has not settled. healWait is released at the 3s bound
// (the close's continuation and the writes of OTHER pages go on), but a write of one of
// these pages stays held: run on the dead URL it would supersede the heal. It runs or is
// dropped when that heal settles, or is dropped by a real navigation (dropDeferredExcept).
type HealMark = { paths: string[] };
const healing = new Set<HealMark>();
const heldByHeal = (pathname: string) => [...healing].some((m) => m.paths.includes(pathname));
// Bumped when a gesture proceeds (closeThen) and when a real navigation starts
// (noteNavigation): a parked closeThen continuation fires only if neither happened since it
// was parked. ownNavs holds one token per awaited flush-chain write: the navigation that
// write issues is consumed by noteNavigation, so only navigations beyond the chain's own
// (in whatever order they arrive) cancel the continuation parked behind the chain.
let gestureGen = 0;
const ownNavs = new Set<object>();

/** Run a close's continuation; a throw is logged and does not strand the heals or the flush. */
function runThen(then: (() => void) | null): void {
	try {
		then?.();
	} catch (e) {
		console.error('[chatSurface] continuation failed', e);
	}
}

/** Resolves when `r` settles or after GO_FALLBACK_MS, whichever first; never rejects. */
function settledOrBounded(r: Promise<unknown>): Promise<void> {
	return new Promise((done) => {
		const t = setTimeout(done, GO_FALLBACK_MS);
		Promise.resolve(r)
			.catch((e) => console.error('[chatSurface] deferred write failed', e))
			.then(() => {
				clearTimeout(t);
				done();
			});
	});
}

/**
 * Run the deferred writes ONE AFTER ANOTHER (insertion order, each isolated),
 * clearing the Map BEFORE running so nothing can run twice. A write that returns
 * a promise (its navigation) is awaited before the next starts, so each re-reads
 * the live page.url: two writes that both rebuild the URL from it would otherwise
 * both start from the same URL and the second would overwrite the first. As each
 * write comes up the identity fence AND the page are checked (a write made on
 * another pathname is skipped, as the heals are), and dropDeferredExcept reaches
 * the queue still waiting. A wait is bounded (GO_FALLBACK_MS): Kit's goto settles
 * on every path but a load that never answers, and that must not hold the rest or
 * the close's continuation. A throwing fence or write is logged and skipped, so the
 * chain never rejects. Returns the settle of the chain, or null when every write
 * was synchronous.
 */
function flushDeferred(): Promise<unknown> | null {
	const queue: Deferred[] = [];
	for (const [k, d] of deferred) {
		if (heldByHeal(d.pathname)) continue;
		queue.push(d);
		deferred.delete(k);
	}
	flushing.add(queue);
	const step = (): Promise<unknown> | null => {
		while (queue.length) {
			const d = queue.shift()!;
			let r: void | Promise<unknown>;
			try {
				if (!d.fence() || d.pathname !== page.url.pathname) continue;
				r = d.run();
			} catch (e) {
				console.error('[chatSurface] deferred write failed', e);
				continue;
			}
			if (r) {
				const tok = {};
				ownNavs.add(tok);
				return settledOrBounded(r).then(() => {
					ownNavs.delete(tok);
					return step();
				});
			}
		}
		flushing.delete(queue);
		return null;
	};
	return step();
}

/**
 * Chat is closed and the page underneath is current. With heals queued, the
 * parked continuation and the heals run first (a heal reads the live state itself,
 * hasDeferredWrite), then the pending writes that a heal that reported a navigation
 * did not supersede: a heal retargets the URL the writes of ITS page were aimed
 * at, so those are dropped once its navigation has settled; a heal that did nothing,
 * failed or did not say leaves them to run (a write made on another page is skipped
 * by the flush, which runs only writes of the current pathname). Otherwise the writes run first and the continuation
 * after they settle, so a continuation that builds its URL from page.url sees the
 * filters. True when a continuation or heal ran.
 */
function onClosed(then: (() => void) | null): boolean {
	// A heal's navigation is still in flight: the writes wait for it (its finish flushes them).
	if (!heals.length && healWait > 0) {
		runThen(then);
		return true;
	}
	if (heals.length) {
		runThen(then);
		const mark: HealMark = { paths: heals.map((h) => h.path) };
		const acted = runHeals();
		const finish = (paths: string[]) => {
			for (const [k, d] of deferred) if (paths.includes(d.pathname)) deferred.delete(k);
			// Chat reopened while the heal was pending: the writes wait for its real close.
			if (page.shallow !== null || goLatch !== null) return;
			void flushDeferred();
		};
		if (Array.isArray(acted)) finish(acted);
		else {
			// The wait is bounded (a load that never answers must not stall the rest), but
			// the bound only opens it for everything else: this page's writes stay held
			// until the heal itself settles (navigated: dropped; not: run) or a real
			// navigation drops them.
			healing.add(mark);
			healWait++;
			let opened = false;
			const open = () => {
				if (!opened) healWait--;
				opened = true;
			};
			void acted.then((p) => {
				open();
				healing.delete(mark);
				finish(p);
			});
			void settledOrBounded(acted).then(() => {
				if (opened) return;
				open();
				finish([]);
			});
		}
		return true;
	}
	const w = flushDeferred();
	if (!then) return false;
	if (w) void w.then(() => runThen(then));
	else runThen(then);
	return true;
}

function surfaceKey(): string {
	return `${page.shallow?.url.href ?? ''}|${page.state.chatDepth ?? 0}|${page.state.chatFromInbox ? 1 : 0}`;
}

/**
 * Release the latch; true when it ran a continuation. `fallback` is the 3s timer:
 * the only release that runs a heal as a plain navigation with Chat still open.
 */
function releaseGo(fallback = false): boolean {
	if (!goLatch) return false;
	const { timer, then } = goLatch;
	clearTimeout(timer);
	goLatch = null;
	// Only a closed surface is the page the continuation was waiting for.
	if (page.shallow === null) return onClosed(then);
	if (!heals.length) return false;
	// A normal settle that finds Chat still open (a Back that landed on the inbox
	// under it): the heal and the continuation keep waiting, and the close goes on
	// from where it now is. Running the heal now would replace Chat's own entry.
	const depth = page.state.chatDepth ?? 0;
	if (!fallback && parseChatPath(page.shallow.url.pathname) !== null && depth > 0) {
		go(-depth, then);
		return false;
	}
	// The 3s fallback (or Chat is not really open): the heal still happens, as the
	// plain navigation it was before Chat handling. Deferred writes stay for the
	// real close; a real navigation drops them.
	runHeals();
	return true;
}

/** Release the latch once its traversal has settled; true when that ran a continuation. */
function settleGo(): boolean {
	if (goLatch && goLatch.popped && goLatch.from !== surfaceKey()) return releaseGo();
	return false;
}

// Watching it here, not only when the next Close asks, matters: Back then a fresh
// thread returns to the key Back left, and a continuation must run unprompted.
// Browser only: the window is the source of the popstate, and the module is also
// imported (never run) by the build's analysis in Node.
// What the surface-key effect does. A user Back with no latch lands on the page
// underneath with nothing to release, so what waited runs here; with a latch
// live, releaseGo owns the close. At depth 2 the first Back leaves the inbox
// open, so nothing runs until the second.
function onSurfaceKeyMoved(): void {
	settleGo();
	if (!goLatch && page.shallow === null) onClosed(null);
}

if (browser) {
	// Kit's own popstate listener may have applied the state before this one
	// runs, so the effect has already looked and seen popped=false: settle here
	// too, and either order releases.
	window.addEventListener('popstate', () => {
		if (!goLatch) return;
		goLatch.popped = true;
		settleGo();
	});
	$effect.root(() => {
		$effect(() => {
			surfaceKey();
			untrack(onSurfaceKeyMoved);
		});
	});
}

// True while pending, and also on the read that ran the continuation: that read
// may have started a navigation of its own, so the caller must not go on as if idle.
function traversing(): boolean {
	const ran = settleGo();
	return ran || goLatch !== null;
}

function go(delta: number, then: (() => void) | null = null): void {
	goLatch = {
		from: surfaceKey(),
		popped: false,
		then,
		timer: setTimeout(() => releaseGo(true), GO_FALLBACK_MS)
	};
	history.go(delta);
}

export const chatSurface = {
	/** The Chat target of an open shallow surface, else null. */
	get target(): ChatTarget | null {
		const url = page.shallow?.url;
		return url ? parseChatPath(url.pathname) : null;
	},

	get isOpen(): boolean {
		return this.target !== null;
	},

	/**
	 * A traversal Chat issued has not settled yet: no other history.go may be issued.
	 * Also true once, on the read that settles it and runs its parked continuation,
	 * because that read may have started a navigation of its own.
	 */
	get traversing(): boolean {
		return traversing();
	},

	/** The history entries Chat has pushed on top of the page. */
	get depth(): number {
		return page.state.chatDepth ?? 0;
	},

	/** The page itself is /messages* (direct entry): Chat is a route, not a surface. */
	get routeMode(): boolean {
		return parseChatPath(page.url.pathname) !== null;
	},

	/** Open Chat on `href` (/messages or /messages/N). */
	async open(href: string): Promise<void> {
		const next = parseChatPath(new URL(href, page.url.href).pathname);
		if (!next || viewport.isMobile || this.routeMode || !authStore.userId) {
			await goto(href);
			return;
		}
		const current = this.target;
		if (sameTarget(current, next)) return;
		// The inbox is already open and a thread is asked for: that is "choose a
		// conversation". A thread to the inbox is "back".
		if (current?.kind === 'thread' && next.kind === 'inbox') {
			this.back();
			return;
		}
		await goto(href, {
			shallow: true,
			state: {
				...page.state,
				chatDepth: this.depth + 1,
				chatFromInbox: current?.kind === 'inbox' && next.kind !== 'inbox' ? true : undefined
			}
		});
	},

	/** Close the surface: pop what Chat pushed, which lands on the opening page. */
	close(): void {
		if (!this.isOpen || traversing()) return;
		const depth = this.depth;
		if (depth > 0) {
			go(-depth);
			return;
		}
		// No recorded depth (should not happen): put the underlying page back.
		void goto(page.url.pathname + page.url.search, { replaceState: true });
	},

	/**
	 * Close the surface, then run `run` once the page underneath is the current
	 * one. For a navigation that must not leave Chat's entries under it (opening a
	 * pane): they would bring Chat back when that navigation is undone. Returns
	 * false, having done nothing, when Chat is not open; the caller then runs its
	 * navigation itself. True means the surface took it, including a gesture that
	 * arrived mid-traversal, which is held rather than queued.
	 */
	closeThen(run: () => void): boolean {
		if (!this.isOpen || this.depth <= 0) {
			gestureGen++; // the caller runs its navigation now
			return false;
		}
		if (traversing()) return true;
		// Fired only if no newer gesture proceeded and the page is still this one: it may
		// outlive the latch inside a flush chain, where traversing() cannot see it.
		const gen = ++gestureGen;
		const path = page.url.pathname;
		go(-this.depth, () => {
			if (gen === gestureGen && page.url.pathname === path) run();
		});
		return true;
	},

	/**
	 * For a heal that cannot wait: close Chat, then run `run` on the page
	 * underneath, or run it now when Chat is not open. Unlike closeThen it is never
	 * dropped: behind a Close or a parked continuation it queues, and if the
	 * traversal never lands (3s fallback, Chat still open) it runs as a plain
	 * navigation. `run` must read page.url inside itself. It is bound to the page it
	 * was queued on and skipped if that is no longer the page. Report the outcome:
	 * return true (or a promise resolving true, once the navigation settled) when it
	 * navigated, which drops the page's pending writes; false, nothing or a rejection
	 * keeps them.
	 */
	runAfterClose(run: () => HealOutcome): void {
		if (!this.isOpen || this.depth <= 0) {
			run();
			return;
		}
		heals.push({ run, path: page.url.pathname });
		if (traversing() || goLatch) return;
		if (this.isOpen && this.depth > 0) go(-this.depth);
		else runHeals();
	},

	/**
	 * Hold a page-owned URL write while Chat is open. True: it is held (last write
	 * per `key` wins) and runs, live, once Chat closes by any path; the caller
	 * returns. False: run it now. A real navigation to another path drops it (the
	 * ChatHost onNavigate hook); so does a change of signed-in user.
	 */
	deferPageWrite(key: string, run: () => void | Promise<unknown>): boolean {
		if (!this.isOpen || this.routeMode || !authStore.userId) return false;
		deferred.set(key, { pathname: page.url.pathname, fence: authStore.identityFence(), run });
		return true;
	},

	/** A write is held under `key` (a heal uses live state instead of the URL then). */
	hasDeferredWrite(key: string): boolean {
		return deferred.has(key);
	},

	/**
	 * `search` for a heal's URL: without `new=` while the ?new=1 strip is held. A
	 * heal that navigates drops the page's held writes, the strip among them, so it
	 * carries the strip's effect itself instead of leaving ?new=1 on the new URL.
	 */
	searchForHeal(search: string): string {
		if (!deferred.has(STRIP_NEW_KEY)) return search;
		const p = new URLSearchParams(search);
		p.delete('new');
		const out = p.toString();
		return out ? `?${out}` : '';
	},

	/** A real (non-shallow) navigation started: it cancels a parked continuation unless it is a flush-chain write's own. */
	noteNavigation(): void {
		const own = ownNavs.values().next();
		if (own.done) gestureGen++;
		else ownNavs.delete(own.value);
	},

	/**
	 * Drop held writes, the writes still queued in a flush, and queued heals made on any other path (a real navigation
	 * away); null drops all.
	 */
	dropDeferredExcept(pathname: string | null): void {
		if (pathname === null || pathname !== page.url.pathname) gestureGen++;
		for (const m of healing) {
			m.paths = m.paths.filter((p) => p === pathname);
			if (!m.paths.length) healing.delete(m);
		}
		for (const [k, d] of deferred) if (d.pathname !== pathname) deferred.delete(k);
		for (let i = heals.length - 1; i >= 0; i--) if (heals[i].path !== pathname) heals.splice(i, 1);
		for (const q of flushing) {
			for (let i = q.length - 1; i >= 0; i--) if (q[i].pathname !== pathname) q.splice(i, 1);
		}
	},

	/** From a thread to the inbox: one entry back when the inbox is the entry below. */
	back(): void {
		if (this.target?.kind !== 'thread' || traversing()) return;
		if (page.state.chatFromInbox) {
			go(-1);
			return;
		}
		void goto('/messages', {
			shallow: true,
			replaceState: true,
			state: { ...page.state, chatFromInbox: undefined }
		});
	},

	/**
	 * The click handler for an anchor whose href is /messages*. A plain left
	 * click opens the surface; anything else (modifier, middle button, target,
	 * download) keeps the href, which is the direct entry.
	 */
	handleLinkClick(e: MouseEvent, href: string): void {
		if (!plainLeftClick(e)) return;
		e.preventDefault();
		void this.open(href);
	}
};

/** Test seam: forget any in-flight traversal. */
export function __resetChatSurfaceForTests(): void {
	if (goLatch) clearTimeout(goLatch.timer);
	goLatch = null;
	deferred.clear();
	flushing.clear();
	healWait = 0;
	healing.clear();
	gestureGen = 0;
	ownNavs.clear();
	heals.length = 0;
}

/** Test seam: what the surface-key effect does when the state moves (jsdom's page is not reactive). */
export function __checkChatSurfaceForTests(): void {
	onSurfaceKeyMoved();
}

const urlKey = () => page.url.pathname + page.url.search;

/**
 * A heal's outcome from its navigation: true only when the navigation committed.
 * Kit's goto RESOLVES, it does not reject, when a beforeNavigate guard cancels it, so
 * a settled promise is not enough: the URL must have moved from where it was when the
 * heal started. Call it on the goto the heal just issued (the URL read is synchronous).
 * False when it failed, was cancelled, or went nowhere.
 */
export function navigated(p: Promise<unknown>, before: string = urlKey()): Promise<boolean> {
	return p.then(
		() => urlKey() !== before,
		() => false
	);
}
