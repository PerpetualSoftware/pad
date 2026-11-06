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
// them. A heal that finds nothing to do returns false.
type Heal = { run: () => void | boolean; path: string };
const heals: Heal[] = [];

/** Run the queued heals; the pathnames of those that navigated (did not return false). */
function runHeals(): string[] {
	const acted: string[] = [];
	for (const h of heals.splice(0)) {
		if (h.path !== page.url.pathname) continue;
		try {
			if (h.run() !== false) acted.push(h.path);
		} catch (e) {
			console.error('[chatSurface] heal failed', e);
		}
	}
	return acted;
}

/**
 * Run the deferred writes ONE AFTER ANOTHER (insertion order, each isolated),
 * clearing the Map BEFORE running so nothing can run twice. A write that returns
 * a promise (its navigation) is awaited before the next starts, so each re-reads
 * the live page.url: two writes that both rebuild the URL from it would otherwise
 * both start from the same URL and the second would overwrite the first. The
 * identity fence is checked as each write comes up, so a user change while an
 * earlier one was in flight skips the rest. Returns the settle of the chain, or
 * null when every write was synchronous.
 */
function flushDeferred(): Promise<unknown> | null {
	const queue = [...deferred.values()];
	deferred.clear();
	const step = (): Promise<unknown> | null => {
		while (queue.length) {
			const d = queue.shift()!;
			if (!d.fence()) continue;
			let r: void | Promise<unknown>;
			try {
				r = d.run();
			} catch (e) {
				console.error('[chatSurface] deferred write failed', e);
				continue;
			}
			if (r) {
				return Promise.resolve(r)
					.catch((e) => console.error('[chatSurface] deferred write failed', e))
					.then(step);
			}
		}
		return null;
	};
	return step();
}

/**
 * Chat is closed and the page underneath is current. With heals queued, the
 * parked continuation and the heals run first (a heal reads the live state itself,
 * hasDeferredWrite), then the pending writes that a heal that actually navigated
 * did not supersede: a heal retargets the URL the writes of ITS page were aimed
 * at, so those are dropped; a heal that did nothing, or a write made on another
 * page, leaves them to run. Otherwise the writes run first and the continuation
 * after they settle, so a continuation that builds its URL from page.url sees the
 * filters. True when a continuation or heal ran.
 */
function onClosed(then: (() => void) | null): boolean {
	if (heals.length) {
		then?.();
		const acted = runHeals();
		for (const [k, d] of deferred) if (acted.includes(d.pathname)) deferred.delete(k);
		void flushDeferred();
		return true;
	}
	const w = flushDeferred();
	if (!then) return false;
	if (w) void w.then(then);
	else then();
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
		if (!this.isOpen || this.depth <= 0) return false;
		if (traversing()) return true;
		go(-this.depth, run);
		return true;
	},

	/**
	 * For a heal that cannot wait: close Chat, then run `run` on the page
	 * underneath, or run it now when Chat is not open. Unlike closeThen it is never
	 * dropped: behind a Close or a parked continuation it queues, and if the
	 * traversal never lands (3s fallback, Chat still open) it runs as a plain
	 * navigation. `run` must read page.url inside itself. It is bound to the page it
	 * was queued on and skipped if that is no longer the page; return false when it
	 * found nothing to do, so the page's pending writes are not dropped for it.
	 */
	runAfterClose(run: () => void | boolean): void {
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
	 * Drop held writes and queued heals made on any other path (a real navigation
	 * away); null drops all.
	 */
	dropDeferredExcept(pathname: string | null): void {
		for (const [k, d] of deferred) if (d.pathname !== pathname) deferred.delete(k);
		for (let i = heals.length - 1; i >= 0; i--) if (heals[i].path !== pathname) heals.splice(i, 1);
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
	heals.length = 0;
}

/** Test seam: what the surface-key effect does when the state moves (jsdom's page is not reactive). */
export function __checkChatSurfaceForTests(): void {
	onSurfaceKeyMoved();
}
