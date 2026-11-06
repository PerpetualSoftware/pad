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
// latches until the surface state it was issued from changes (the popstate
// landed), or a bounded fallback, so a lost popstate cannot freeze Close.
const GO_SETTLE_MS = 800;
let goLatch: { from: string; timer: ReturnType<typeof setTimeout> } | null = null;

function surfaceKey(): string {
	return `${page.shallow?.url.href ?? ''}|${page.state.chatDepth ?? 0}|${page.state.chatFromInbox ? 1 : 0}`;
}

function releaseGo(): void {
	if (goLatch) clearTimeout(goLatch.timer);
	goLatch = null;
}

// The popstate landed when the key moves. Watching it here, not only when the next
// Close asks, matters: Back then a fresh thread returns to the key Back left.
$effect.root(() => {
	$effect(() => {
		const key = surfaceKey();
		untrack(() => {
			if (goLatch && goLatch.from !== key) releaseGo();
		});
	});
});

function traversing(): boolean {
	if (goLatch && goLatch.from !== surfaceKey()) releaseGo();
	return goLatch !== null;
}

function go(delta: number): void {
	goLatch = { from: surfaceKey(), timer: setTimeout(releaseGo, GO_SETTLE_MS) };
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
	releaseGo();
}
