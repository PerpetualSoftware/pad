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

function show(message: string, type: Toast['type'] = 'info', duration?: number, link?: string, action?: ToastAction): string {
	duration ??= type === 'error' ? ERROR_DURATION : DEFAULT_DURATION;
	const id = generateId();
	const toast: Toast = { id, message, type, duration, link, action };

	toasts.push(toast);

	// Add to history
	history.unshift({ id, message, type, timestamp: Date.now(), link });
	while (history.length > MAX_HISTORY) {
		history.pop();
	}
	unreadCount++;

	// Drop oldest if exceeded max
	while (toasts.length > MAX_TOASTS) {
		const oldest = toasts.shift();
		if (oldest) {
			clearTimerFor(oldest.id);
		}
	}

	// Auto-dismiss after duration
	const clock: ToastClock = { timer: null, startedAt: 0, remaining: duration, holds: 0 };
	timers.set(id, clock);
	startClock(id, clock);

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
