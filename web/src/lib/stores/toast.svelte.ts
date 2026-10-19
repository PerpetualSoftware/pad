import { authStore } from './auth.svelte';
export interface ToastAction {
	label: string;
	onAction: () => void;
}

export interface Toast {
	id: string;
	message: string;
	type: 'success' | 'error' | 'info';
	duration: number;
	link?: string;
	// Optional inline action button (e.g. "Undo" on a bulk archive,
	// TASK-1674). Distinct from `link`, which navigates. Not persisted to
	// history — the callback is only meaningful while the toast is live.
	action?: ToastAction;
}

export interface HistoryEntry {
	id: string;
	message: string;
	type: Toast['type'];
	timestamp: number;
	link?: string;
}

const MAX_TOASTS = 5;
const MAX_HISTORY = 20;
const DEFAULT_DURATION = 3000;
/**
 * Errors linger (TASK-2202): an error is the toast a person most needs to read,
 * and at 3s it vanished before most could. Applied when the caller passes no
 * duration; an explicit duration still wins.
 */
export const ERROR_DURATION = 10000;
/**
 * A toast that offers something to do (an action button or a link) lingers as
 * long as an error (TASK-3973): at 3s the "Open" on a quick-add confirmation
 * was gone before a person could reach it. Applied when the caller passes no
 * duration; an explicit duration still wins.
 */
export const ACTIONABLE_DURATION = ERROR_DURATION;

/**
 * Test-surface kill switch for CROSS-ACTOR notification toasts (BUG-2334).
 *
 * The e2e suite shares one pad instance and one workspace, so items seeded by
 * OTHER concurrently-running specs arrive over SSE and stack "X created: …"
 * info toasts bottom-right — directly over bottom-right UI (the graph drawer's
 * detail card), turning unrelated specs' clicks into a race. The shared e2e
 * fixture sets this flag via addInitScript; the ONE call site that shows a
 * toast for another actor's SSE event checks it.
 *
 * Scope is deliberately narrow: only toasts announcing ANOTHER actor's work is
 * gated. Toasts the page earns with its own actions (copy results, errors,
 * undo) are untouched, so specs still exercise — and can still be broken by —
 * the real toast surface. Production never sets the flag; the e2e control leg
 * pins the flag-off behavior so the product toast can't silently regress.
 */
export function quietExternalToasts(): boolean {
	try {
		return globalThis.localStorage?.getItem('pad:e2e-quiet-external-toasts') === '1';
	} catch {
		// Storage unavailable (privacy mode, sandboxed iframe): behave like prod.
		return false;
	}
}

let toasts = $state<Toast[]>([]);
let history = $state<HistoryEntry[]>([]);
let unreadCount = $state(0);
/**
 * Each toast's auto-dismiss clock. `holds` counts the reasons it is paused
 * (hover, keyboard focus: TASK-2202); the clock runs only at zero holds, and
 * `remaining` is what is left of the duration when it was last paused.
 */
interface ToastClock {
	timer: ReturnType<typeof setTimeout> | null;
	startedAt: number;
	remaining: number;
	holds: number;
}
const timers = new Map<string, ToastClock>();

function startClock(id: string, clock: ToastClock): void {
	clock.startedAt = Date.now();
	clock.timer = setTimeout(() => dismiss(id), clock.remaining);
}

// A counter makes the id unique by construction; the toast lists are keyed by
// it, and two random suffixes in one millisecond could collide (TASK-3539).
let toastSeq = 0;
function generateId(): string {
	return Date.now().toString(36) + '-' + (++toastSeq).toString(36);
}

/**
 * What counts toward the bell's unread badge (TASK-3593): every ERROR, and
 * anything else only when the caller sets `unread`. A confirmation of the
 * user's own action is not news to them, and counting every one of them made
 * the badge meaningless; an error is never a confirmation, whoever caused it,
 * and an error from something running in the background (an autosave, a
 * page-load fetch) may auto-dismiss unseen. The rule lives here rather than
 * at each call site so a new error toast counts without anyone choosing.
 * `unread` is for a non-error toast about something the user did not do:
 * another actor's work, or a background notice they may have missed.
 */
export interface ShowOptions {
	unread?: boolean;
}

function show(
	message: string,
	type: Toast['type'] = 'info',
	duration?: number,
	link?: string,
	action?: ToastAction,
	opts: ShowOptions = {},
): string {
	duration ??=
		type === 'error' ? ERROR_DURATION : action || link ? ACTIONABLE_DURATION : DEFAULT_DURATION;
	const id = generateId();
	const toast: Toast = { id, message, type, duration, link, action };

	toasts.push(toast);

	// Add to history
	history.unshift({ id, message, type, timestamp: Date.now(), link });
	while (history.length > MAX_HISTORY) {
		history.pop();
	}
	if (opts.unread || type === 'error') unreadCount++;

	// Drop oldest if exceeded max
	while (toasts.length > MAX_TOASTS) {
		const oldest = toasts.shift();
		if (oldest) {
			clearTimerFor(oldest.id);
			forgetGroupOf(oldest.id);
		}
	}

	// Auto-dismiss after duration
	const clock: ToastClock = { timer: null, startedAt: 0, remaining: duration, holds: 0 };
	timers.set(id, clock);
	startClock(id, clock);

	return id;
}

/**
 * `show` with `unread` set (TASK-3593): for a NON-error toast about something
 * the user may not have caused or seen, e.g. the server-busy notice, which a
 * background request can raise. An error counts without this.
 */
function showUnread(
	message: string,
	type: Toast['type'] = 'info',
	duration?: number,
	link?: string,
	action?: ToastAction,
): string {
	return show(message, type, duration, link, action, { unread: true });
}

/**
 * How long after the last event of a burst the next one still joins it
 * (TASK-3593). A burst also ends when its toast leaves the screen, whichever
 * comes first: an event after that starts a new toast.
 */
export const GROUP_WINDOW_MS = 10_000;

interface ToastGroup<A> {
	id: string;
	acc: A;
	lastAt: number;
}
const groups = new Map<string, ToastGroup<unknown>>();

/** Live burst groups; for tests that a gone toast leaves none behind. */
export function __groupCountForTests(): number {
	return groups.size;
}

/** Forget the burst a toast belonged to once the toast is gone. */
function forgetGroupOf(id: string): void {
	for (const [key, group] of groups) {
		if (group.id === id) groups.delete(key);
	}
}

/**
 * Fold a burst of events from one source into ONE toast (TASK-3593): a busy
 * agent creating eighteen items raises "Agent created 18 items", not
 * eighteen toasts. `key` names the source. `fold` folds each event into a
 * running summary (undefined for the first), and `render` turns the summary
 * into the toast's text and link, so a long burst holds one bounded value,
 * not every event it has seen. A later event in the burst updates the live
 * toast and its history entry in place and restarts its clock; the burst
 * counts once toward unread. A burst is forgotten when its toast goes.
 */
function showGrouped<T, A>(
	key: string,
	entry: T,
	fold: (acc: A | undefined, entry: T) => A,
	render: (acc: A) => { message: string; link?: string },
	type: Toast['type'] = 'info',
	duration?: number,
	opts: ShowOptions = {},
): string {
	const now = Date.now();
	const group = groups.get(key) as ToastGroup<A> | undefined;
	const live = group ? toasts.find((t) => t.id === group.id) : undefined;
	if (group && live && now - group.lastAt <= GROUP_WINDOW_MS) {
		group.acc = fold(group.acc, entry);
		group.lastAt = now;
		const { message, link } = render(group.acc);
		live.message = message;
		live.link = link;
		// The history entry moves to the front with its new timestamp, so the
		// tray stays newest-first when another toast landed mid-burst (codex r3).
		const at = history.findIndex((h) => h.id === group.id);
		if (at !== -1) {
			const [past] = history.splice(at, 1);
			past.message = message;
			past.link = link;
			past.timestamp = now;
			history.unshift(past);
		}
		// The burst is still going: give the toast its full time again. A held
		// clock (hover, focus) takes it up when the hold is released.
		const clock = timers.get(group.id);
		if (clock) {
			clock.remaining = live.duration;
			if (clock.timer !== null) {
				clearTimeout(clock.timer);
				startClock(group.id, clock);
			}
		}
		return group.id;
	}
	if (group) groups.delete(key);
	const acc = fold(undefined, entry);
	const { message, link } = render(acc);
	const id = show(message, type, duration, link, undefined, opts);
	groups.set(key, { id, acc, lastAt: now } as ToastGroup<unknown>);
	return id;
}

/**
 * Hold a toast on screen while the pointer is over it or focus is inside it
 * (TASK-2202): a person reading or reaching for its button should not have it
 * vanish. Holds nest, so a hover that ends while focus stays keeps it held.
 */
function pause(id: string): void {
	const clock = timers.get(id);
	if (!clock) return;
	clock.holds++;
	if (clock.holds > 1 || clock.timer === null) return;
	clearTimeout(clock.timer);
	clock.timer = null;
	clock.remaining = Math.max(0, clock.remaining - (Date.now() - clock.startedAt));
}

/** Release one hold; the clock resumes with what was left when the last one goes. */
function resume(id: string): void {
	const clock = timers.get(id);
	if (!clock || clock.holds === 0) return;
	clock.holds--;
	if (clock.holds === 0) startClock(id, clock);
}

function dismiss(id: string): void {
	clearTimerFor(id);
	forgetGroupOf(id);
	const idx = toasts.findIndex((t) => t.id === id);
	if (idx !== -1) {
		toasts.splice(idx, 1);
	}
}

function clearTimerFor(id: string): void {
	const clock = timers.get(id);
	if (clock) {
		if (clock.timer !== null) clearTimeout(clock.timer);
		timers.delete(id);
	}
}

function markAllRead(): void {
	unreadCount = 0;
}

function clearHistory(): void {
	history.length = 0;
	unreadCount = 0;
}

/**
 * Drop live toasts, the history and the unread count (BUG-3005).
 *
 * Timers are cancelled rather than left to fire: a pending auto-dismiss for a
 * toast that no longer exists would call `dismiss` on a missing id, and the
 * map would keep the handle until then.
 */
function clearAll(): void {
	for (const id of [...timers.keys()]) clearTimerFor(id);
	groups.clear();
	toasts = [];
	clearHistory();
}

export const toastStore = {
	get toasts(): Toast[] {
		return toasts;
	},
	get history(): HistoryEntry[] {
		return history;
	},
	get unreadCount(): number {
		return unreadCount;
	},
	show,
	showUnread,
	showGrouped,
	dismiss,
	pause,
	resume,
	markAllRead,
	clearHistory,
	clearAll
};

// The notification tray is a readable LOG of the previous user's actions
// (BUG-3005, the enumeration table). Toast text routinely names items —
// "Archived TASK-12" — so leaving the history in place lets B read what A just
// did. Live toasts go too: a toast fired for A's action has no meaning in B's
// session, and `dismissAll` also cancels their timers.
authStore.onIdentityChange(() => {
	toastStore.clearAll();
});
