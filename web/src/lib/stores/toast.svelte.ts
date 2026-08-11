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
const timers = new Map<string, ReturnType<typeof setTimeout>>();

function generateId(): string {
	return Date.now().toString(36) + Math.random().toString(36).slice(2, 7);
}

function show(message: string, type: Toast['type'] = 'info', duration: number = DEFAULT_DURATION, link?: string, action?: ToastAction): string {
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
	const timer = setTimeout(() => {
		dismiss(id);
	}, duration);
	timers.set(id, timer);

	return id;
}

function dismiss(id: string): void {
	clearTimerFor(id);
	const idx = toasts.findIndex((t) => t.id === id);
	if (idx !== -1) {
		toasts.splice(idx, 1);
	}
}

function clearTimerFor(id: string): void {
	const timer = timers.get(id);
	if (timer) {
		clearTimeout(timer);
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
	markAllRead,
	clearHistory
};
